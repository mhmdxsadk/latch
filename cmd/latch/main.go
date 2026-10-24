// Command latch securely lets AI agents monitor and manage servers without
// giving them unrestricted access. See README.md for the trust model.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/mhmdxsadk/latch/internal/audit"
	"github.com/mhmdxsadk/latch/internal/config"
	"github.com/mhmdxsadk/latch/internal/docker"
	"github.com/mhmdxsadk/latch/internal/excluded"
	"github.com/mhmdxsadk/latch/internal/files"
	"github.com/mhmdxsadk/latch/internal/health"
	"github.com/mhmdxsadk/latch/internal/host"
	"github.com/mhmdxsadk/latch/internal/mcp"
	"github.com/mhmdxsadk/latch/internal/policy"
	"github.com/mhmdxsadk/latch/internal/ports"
	"github.com/mhmdxsadk/latch/internal/server"
	"github.com/mhmdxsadk/latch/internal/systemd"
)

// Set by GoReleaser via -ldflags.
var (
	version = "dev"
	commit  = "none"
)

// versionText is the output of "latch version": the version, then the commit
// and platform so a bug report can name the exact build.
func versionText(version, commit, goos, goarch string) string {
	detail := goos + "/" + goarch
	if commit != "" && commit != "none" {
		if len(commit) > 7 {
			commit = commit[:7]
		}
		detail = "commit " + commit + ", " + detail
	}
	return "latch " + version + "\n" + detail + "\n"
}

const usage = `Usage: latch <command> [flags]

Commands:
  serve      Start the Latch server
  validate   Validate configuration without starting the server
  doctor     Check that configured integrations are accessible
  version    Print version information

Run "latch <command> -h" for command flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return cmdServe(args[1:], stderr)
	case "validate":
		return cmdValidate(args[1:], stdout, stderr)
	case "doctor":
		return cmdDoctor(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprint(stdout, versionText(version, commit, runtime.GOOS, runtime.GOARCH))
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "latch: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// printConfigProblems prints one "latch: file[:line]: message" line per
// problem, the way compilers and tools like nginx -t and sshd -t do.
func printConfigProblems(w io.Writer, path string, err error) {
	for _, p := range config.Problems(err) {
		if p.Line > 0 {
			fmt.Fprintf(w, "latch: %s:%d: %s\n", path, p.Line, p.Text)
		} else {
			fmt.Fprintf(w, "latch: %s: %s\n", path, p.Text)
		}
	}
}

func configFlag(fs *flag.FlagSet) *string {
	return fs.String("config", "/etc/latch/config.yaml", "path to the configuration file")
}

func cmdValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if _, err := config.Load(*path); err != nil {
		printConfigProblems(stderr, *path, err)
		return 1
	}
	fmt.Fprintf(stdout, "latch: %s: configuration OK\n", *path)
	return 0
}

// newFileGuard builds the block list for the file tools: the built-in rules,
// the config file itself, files.exclude and the home directories of the
// accounts in the socket group (the agents). If those homes cannot all be
// resolved, Latch still starts, with a warning, blocking the ones it found.
func newFileGuard(cfg *config.Config, configPath string, folders files.Folders, stderr io.Writer) (*files.Guard, error) {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return nil, fmt.Errorf("config path: %w", err)
	}
	homes, err := files.AgentHomes(cfg.Server.SocketGroup)
	if err != nil {
		fmt.Fprintf(stderr, "latch: warning: %v\n", err)
	}
	return files.NewGuard(files.DefaultRules(abs, homes, cfg.Files.Exclude), folders), nil
}

func cmdServe(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := configFlag(fs)
	stdio := fs.Bool("stdio", false, "serve a single client on stdin/stdout (development only; no privilege separation)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*path)
	if err != nil {
		printConfigProblems(stderr, *path, err)
		return 1
	}

	auditOut := stderr
	if cfg.Audit.File != "" {
		f, err := os.OpenFile(cfg.Audit.File, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintf(stderr, "latch: open audit log: %v\n", err)
			return 1
		}
		defer f.Close()
		auditOut = f
	}
	al := audit.New(slog.New(slog.NewJSONHandler(auditOut, nil)))

	pol := policy.New(cfg)
	dk := docker.NewEngine(cfg.Docker.Socket)
	tracker := &excluded.Tracker{Docker: dk, Policy: pol}
	fileGuard, err := newFileGuard(cfg, *path, tracker, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "latch: %v\n", err)
		return 1
	}
	deps := &mcp.Deps{
		Policy:   pol,
		Host:     host.System{},
		Docker:   dk,
		Systemd:  systemd.NewBus(),
		Health:   health.Prober{},
		Ports:    ports.Proc{},
		Files:    files.NewReader(fileGuard, files.DefaultLimits()),
		Excluded: tracker,
		Audit:    al,
		Version:  version,
	}
	srv := mcp.NewServer(deps)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *stdio {
		fmt.Fprintln(stderr, "latch: WARNING: stdio mode has no privilege separation; use the Unix socket in production")
		if err := server.RunStdio(ctx, srv); err != nil && ctx.Err() == nil {
			fmt.Fprintf(stderr, "latch: %v\n", err)
			return 1
		}
		return 0
	}

	gid, err := server.GroupID(cfg.Server.SocketGroup)
	if err != nil {
		fmt.Fprintf(stderr, "latch: %v\n", err)
		return 1
	}
	ln, err := server.Listen(server.Options{Path: cfg.Server.Socket, Group: cfg.Server.SocketGroup})
	if err != nil {
		fmt.Fprintf(stderr, "latch: listen: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "latch: listening on unix://%s\n", cfg.Server.Socket)
	err = server.ServeUnix(ctx, ln, server.Handler(srv), gid, al)
	os.Remove(cfg.Server.Socket)
	if err != nil {
		fmt.Fprintf(stderr, "latch: %v\n", err)
		return 1
	}
	return 0
}
