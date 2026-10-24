package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/mhmdxsadk/latch/internal/config"
	"github.com/mhmdxsadk/latch/internal/docker"
	"github.com/mhmdxsadk/latch/internal/health"
	"github.com/mhmdxsadk/latch/internal/host"
	"github.com/mhmdxsadk/latch/internal/server"
	"github.com/mhmdxsadk/latch/internal/systemd"
)

const doctorTimeout = 2 * time.Minute

// integrations are what doctor exercises; tests substitute fakes.
type integrations struct {
	host    host.Provider
	docker  docker.Client
	systemd systemd.Client
	health  health.Checker
	groupID func(name string) (uint32, error)
}

// cmdDoctor checks that every configured integration is reachable. It never
// starts the server. Output is for the operator, so it names configured
// resources, but it never prints secrets (config cannot contain any).
func cmdDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintf(stderr, "invalid configuration:\n%v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
	defer cancel()
	ok := doctor(ctx, cfg, integrations{
		host:    host.System{},
		docker:  docker.NewEngine(cfg.Docker.Socket),
		systemd: systemd.NewBus(),
		health:  health.Prober{},
		groupID: server.GroupID,
	}, stdout, useColor(stdout))
	if !ok {
		return 1
	}
	return 0
}

// useColor reports whether w is a terminal that should get ANSI colors.
// NO_COLOR (https://no-color.org) and TERM=dumb turn them off.
func useColor(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// report prints doctor results grouped under headings.
type report struct {
	out    io.Writer
	color  bool
	failed int
}

func (r *report) paint(code, s string) string {
	if !r.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (r *report) section(name string) {
	fmt.Fprintf(r.out, "\n%s\n", r.paint("1", name))
}

// check records one result. hint is only shown on failure.
func (r *report) check(name string, err error, hint string) {
	if err == nil {
		fmt.Fprintf(r.out, "  %s %s\n", r.paint("32", "✓"), name)
		return
	}
	r.failed++
	fmt.Fprintf(r.out, "  %s %s\n", r.paint("31", "✗"), name)
	fmt.Fprintf(r.out, "    %s\n", err)
	if hint != "" {
		fmt.Fprintf(r.out, "    %s\n", r.paint("2", hint))
	}
}

func (r *report) summary() {
	fmt.Fprintln(r.out)
	switch r.failed {
	case 0:
		fmt.Fprintln(r.out, r.paint("32", "All checks passed."))
	case 1:
		fmt.Fprintln(r.out, r.paint("31", "1 problem found."))
	default:
		fmt.Fprintln(r.out, r.paint("31", fmt.Sprintf("%d problems found.", r.failed)))
	}
}

func doctor(ctx context.Context, cfg *config.Config, in integrations, out io.Writer, color bool) bool {
	r := &report{out: out, color: color}
	fmt.Fprintln(out, r.paint("1", "Latch doctor"))

	r.section("Config")
	r.check("config valid", nil, "")

	r.section("Socket")
	dir := filepath.Dir(cfg.Server.Socket)
	const dirHint = "Use a dedicated directory that other accounts cannot write to."
	if fi, err := os.Stat(dir); err != nil {
		hint := dirHint
		if errors.Is(err, os.ErrNotExist) {
			hint = "The service creates it when it starts, so this is expected before the first start."
		}
		r.check("directory "+dir, err, hint)
	} else if !fi.IsDir() {
		r.check("directory "+dir, errors.New("not a directory"), dirHint)
	} else if fi.Mode().Perm()&0o002 != 0 {
		r.check("directory "+dir, errors.New("anyone can write to it"), dirHint)
	} else {
		r.check("directory "+dir, nil, "")
	}
	_, err := in.groupID(cfg.Server.SocketGroup)
	r.check("group "+cfg.Server.SocketGroup, err,
		"Create it with 'groupadd --system "+cfg.Server.SocketGroup+"', or set socket_group to a group that exists.")

	r.section("Host")
	const linuxHint = "Only available on Linux."
	hostHint := func(err error) string {
		if errors.Is(err, host.ErrUnsupported) {
			return linuxHint
		}
		return ""
	}
	_, err = in.host.Load(ctx)
	r.check("load", err, hostHint(err))
	_, err = in.host.Memory(ctx)
	r.check("memory", err, hostHint(err))
	for _, d := range cfg.Host.Disks {
		_, err := in.host.Disk(ctx, d.Path)
		r.check("disk "+d.Name+" ("+d.Path+")", err, "Check that the path exists and is a mount point or directory.")
	}

	if len(cfg.Docker.Services) > 0 {
		r.section("Docker")
		if err := in.docker.Ping(ctx); err != nil {
			r.check("daemon", err, "Is Docker running? Check docker.socket in the config ("+cfg.Docker.Socket+") and that the latch service can read it.")
		} else {
			r.check("daemon", nil, "")
			for _, s := range cfg.Docker.Services {
				_, err := in.docker.Status(ctx, s.Container)
				hint := ""
				if errors.Is(err, docker.ErrNotFound) {
					hint = "No container has exactly this name. Check 'docker ps -a'."
				}
				r.check(fmt.Sprintf("%s (container %s)", s.Name, s.Container), err, hint)
			}
		}
	}

	if len(cfg.Systemd.Units) > 0 {
		r.section("systemd")
		if err := in.systemd.Ping(ctx); err != nil {
			r.check("system bus", err, "Is systemd running, and can the latch service reach /run/dbus/system_bus_socket?")
		} else {
			r.check("system bus", nil, "")
			for _, u := range cfg.Systemd.Units {
				_, err := in.systemd.UnitStatus(ctx, u)
				hint := ""
				if errors.Is(err, systemd.ErrNotFound) {
					hint = "No unit with this name is installed. Check 'systemctl list-unit-files'."
				}
				r.check(u, err, hint)
			}
		}
	}

	if len(cfg.Health.HTTP) > 0 || len(cfg.Health.DNS) > 0 {
		r.section("Health checks")
	}
	for _, t := range cfg.Health.HTTP {
		res, err := in.health.HTTP(ctx, t)
		r.check("http "+t.Name, httpProblem(t, res, err), httpHint(res.Failure))
	}
	for _, t := range cfg.Health.DNS {
		res, err := in.health.DNS(ctx, t)
		if err == nil && !res.OK {
			err = errors.New(string(res.Failure))
		}
		r.check("dns "+t.Name, err, "Check the hostname and that this server can reach a DNS resolver.")
	}

	r.summary()
	return r.failed == 0
}

func httpHint(f health.Failure) string {
	switch f {
	case health.FailureConnectionRefused:
		return "Nothing is listening at that address and port. Is the service running?"
	case health.FailureTimeout:
		return "No answer within the timeout. Raise timeout in the config, or check the service."
	case health.FailureTLS:
		return "The TLS certificate is invalid or expired."
	}
	return ""
}

func httpProblem(t config.HTTPTarget, r health.HTTPResult, err error) error {
	switch {
	case err != nil:
		return err
	case r.Failure == health.FailureBlockedAddress && t.AllowPrivate:
		return errors.New("target resolves to an address that is always blocked (cloud metadata, unspecified, multicast or reserved)")
	case r.Failure == health.FailureBlockedAddress:
		return errors.New("target resolves to a blocked address (set allow_private: true if it is meant to be internal)")
	case r.Failure == health.FailureUnexpectedStatus:
		return fmt.Errorf("unexpected status %d", r.StatusCode)
	case !r.OK:
		return errors.New(string(r.Failure))
	}
	return nil
}
