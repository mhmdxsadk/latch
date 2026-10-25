package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/mhmdxsadk/latch/internal/admin"
	"github.com/mhmdxsadk/latch/internal/approval"
	"github.com/mhmdxsadk/latch/internal/config"
	"github.com/mhmdxsadk/latch/internal/textsafe"
)

const adminTimeout = 30 * time.Second

// adminFlags adds --config and --admin-socket. The socket path comes from the
// flag, or else from server.admin_socket in the config.
func adminFlags(fs *flag.FlagSet) func(stderr io.Writer) (string, bool) {
	path := configFlag(fs)
	sock := fs.String("admin-socket", "", "path to the admin socket (default: server.admin_socket from the config)")
	return func(stderr io.Writer) (string, bool) {
		if *sock != "" {
			return *sock, true
		}
		cfg, err := config.Load(*path)
		if err != nil {
			printConfigProblems(stderr, *path, err)
			return "", false
		}
		return cfg.Server.AdminSocket, true
	}
}

// connError explains a failure to reach the admin socket.
func connError(stderr io.Writer, sock string, err error) {
	hint := ""
	switch {
	case errors.Is(err, os.ErrPermission), errors.Is(err, syscall.EACCES):
		hint = " (run as root)"
	case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ECONNREFUSED):
		hint = " (is the latch service running?)"
	}
	fmt.Fprintf(stderr, "latch: %s: %v%s\n", sock, err, hint)
}

func cmdPending(args []string, stdout, stderr io.Writer) int {
	return listCommand("pending", args, stdout, stderr, (*admin.Client).Pending, printPending)
}

func cmdRequests(args []string, stdout, stderr io.Writer) int {
	return listCommand("requests", args, stdout, stderr, (*admin.Client).Recent, printRequests)
}

// listCommand runs a command that fetches requests and prints them.
func listCommand(name string, args []string, stdout, stderr io.Writer,
	fetch func(*admin.Client, context.Context) ([]approval.Request, error),
	print func(io.Writer, []approval.Request, time.Time)) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := adminFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "latch: %s takes no arguments\n", name)
		return 2
	}
	sock, ok := socket(stderr)
	if !ok {
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	reqs, err := fetch(admin.NewClient(sock), ctx)
	var ae *admin.APIError
	switch {
	case errors.As(err, &ae):
		fmt.Fprintf(stderr, "latch: %s\n", ae.Message)
		return 1
	case err != nil:
		connError(stderr, sock, err)
		return 1
	}
	print(stdout, reqs, time.Now())
	return 0
}

// printRequests prints one plain block per recent request with its outcome:
//
//	request 42  docker_stop  jellyfin  done
//	  asked 3 minutes ago, finished 2 minutes ago
//	  result: stopped; container state exited
func printRequests(w io.Writer, reqs []approval.Request, now time.Time) {
	if len(reqs) == 0 {
		fmt.Fprintln(w, "latch: no requests")
		return
	}
	clean := func(s string) string { return textsafe.Line(s) }
	for i, r := range reqs {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "request %d  %s  %s  %s\n", r.ID, clean(r.Action), clean(r.Resource.Name), clean(string(r.Status)))
		when := "asked " + ago(now.Sub(r.CreatedAt))
		if !r.FinishedAt.IsZero() {
			when += ", finished " + ago(now.Sub(r.FinishedAt))
		}
		fmt.Fprintf(w, "  %s\n", when)
		if r.Result != nil {
			fmt.Fprintf(w, "  result: %s\n", clean(r.Result.Summary))
			for _, d := range r.Result.Details {
				fmt.Fprintf(w, "    %s\n", clean(d))
			}
		}
	}
}

// printPending prints one plain block per request, like:
//
//	request 42  docker_stop  jellyfin
//	  asked by uid 1000 (pid 4242) 3 minutes ago, expires in 57 minutes
//	  stop container jellyfin (Docker waits 30s for it to stop, then kills it)
func printPending(w io.Writer, reqs []approval.Request, now time.Time) {
	if len(reqs) == 0 {
		fmt.Fprintln(w, "latch: no pending requests")
		return
	}
	clean := func(s string) string { return textsafe.Line(s) }
	for i, r := range reqs {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "request %d  %s  %s\n", r.ID, clean(r.Action), clean(r.Resource.Name))
		if r.RunsOnTimeout {
			fmt.Fprintf(w, "  filed by Latch %s, RUNS BY ITSELF %s unless denied\n",
				ago(now.Sub(r.CreatedAt)), in(r.ExpiresAt.Sub(now)))
		} else {
			fmt.Fprintf(w, "  asked by uid %d (pid %d) %s, expires %s\n",
				r.RequestedBy.UID, r.RequestedBy.PID, ago(now.Sub(r.CreatedAt)), in(r.ExpiresAt.Sub(now)))
		}
		for _, d := range r.Details {
			fmt.Fprintf(w, "  %s\n", clean(d))
		}
		for _, p := range r.Params {
			fmt.Fprintf(w, "  %s: %s\n", clean(p.Name), clean(p.Value))
		}
	}
}

func span(d time.Duration) string {
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return strconv.Itoa(n) + " " + unit + "s"
	}
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute")
	default:
		return plural(int(d/time.Hour), "hour")
	}
}

func ago(d time.Duration) string { return span(d) + " ago" }
func in(d time.Duration) string  { return "in " + span(d) }

// cmdDecide runs "latch approve ID" or "latch deny ID".
func cmdDecide(verb string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := adminFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "latch: usage: latch %s [flags] ID\n", verb)
		return 2
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || id < 1 {
		fmt.Fprintf(stderr, "latch: invalid request id %q\n", textsafe.Line(fs.Arg(0)))
		return 2
	}
	sock, ok := socket(stderr)
	if !ok {
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	c := admin.NewClient(sock)
	do, past := c.Approve, "approved"
	if verb == "deny" {
		do, past = c.Deny, "denied"
	}
	_, err = do(ctx, id)
	var ae *admin.APIError
	switch {
	case errors.As(err, &ae):
		fmt.Fprintf(stderr, "latch: request %d: %s\n", id, ae.Message)
		return 1
	case err != nil:
		connError(stderr, sock, err)
		return 1
	}
	fmt.Fprintf(stdout, "latch: request %d %s\n", id, past)
	return 0
}
