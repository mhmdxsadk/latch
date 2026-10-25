package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mhmdxsadk/latch/internal/config"
	"github.com/mhmdxsadk/latch/internal/docker"
	"github.com/mhmdxsadk/latch/internal/files"
	"github.com/mhmdxsadk/latch/internal/health"
	"github.com/mhmdxsadk/latch/internal/host"
	"github.com/mhmdxsadk/latch/internal/policy"
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
	euid    func() int
	// compose reports which docker CLI runs "docker compose".
	compose func(ctx context.Context) (string, error)
	// backups checks the backup folder: whether it exists and is safe.
	backups func() (bool, error)
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
		printConfigProblems(stderr, *path, err)
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
		euid:    os.Geteuid,
		compose: func(ctx context.Context) (string, error) { return docker.CheckComposeCLI(ctx, cfg.Docker.Socket) },
		backups: func() (bool, error) { return files.CheckBackupRoot(files.DefaultBackupRoot, 0) },
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

// check records one result. hint is only shown on failure.
func (r *report) check(name string, err error, hint string) {
	if err == nil {
		fmt.Fprintf(r.out, "%s  %s\n", r.paint("32", "OK  "), name)
		return
	}
	r.failed++
	fmt.Fprintf(r.out, "%s  %s\n", r.paint("31", "FAIL"), name)
	fmt.Fprintf(r.out, "      %s\n", err)
	if hint != "" {
		fmt.Fprintf(r.out, "      fix: %s\n", strings.ToLower(hint[:1])+hint[1:])
	}
}

// warn records something worth knowing that is not a problem: it is not
// counted in the summary.
func (r *report) warn(name, detail, hint string) {
	fmt.Fprintf(r.out, "%s  %s\n", r.paint("33", "WARN"), name)
	fmt.Fprintf(r.out, "      %s\n", detail)
	if hint != "" {
		fmt.Fprintf(r.out, "      fix: %s\n", strings.ToLower(hint[:1])+hint[1:])
	}
}

// checkDir checks that a socket directory exists and is not writable by
// everyone.
func (r *report) checkDir(name, dir string) {
	const dirHint = "Use a dedicated directory that other accounts cannot write to."
	name += " " + dir
	switch fi, err := os.Stat(dir); {
	case errors.Is(err, os.ErrNotExist):
		r.check(name, err, "The service creates it when it starts, so this is expected before the first start.")
	case err != nil:
		r.check(name, err, dirHint)
	case !fi.IsDir():
		r.check(name, errors.New("not a directory"), dirHint)
	case fi.Mode().Perm()&0o002 != 0:
		r.check(name, errors.New("anyone can write to it"), dirHint)
	default:
		r.check(name, nil, "")
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

	fmt.Fprintln(out)
	r.check("config valid", nil, "")
	var rootErr error
	if uid := in.euid(); uid != 0 {
		rootErr = fmt.Errorf("running as uid %d, not root", uid)
	}
	r.check("running as root", rootErr,
		"Run doctor with sudo. The latch service must run as root (User=root in latch.service) to make changes and read protected files.")
	r.checkDir("socket directory", filepath.Dir(cfg.Server.Socket))
	if adminDir := filepath.Dir(cfg.Server.AdminSocket); adminDir != filepath.Dir(cfg.Server.Socket) {
		r.checkDir("admin socket directory", adminDir)
	}
	switch exists, err := in.backups(); {
	case err != nil:
		r.check("backup folder "+files.DefaultBackupRoot, err,
			"It must be a real folder (not a symlink) owned by root with mode 0700, like every folder above it under /var/lib/latch.")
	case !exists:
		r.check("backup folder "+files.DefaultBackupRoot+" (created on the first file change)", nil, "")
	default:
		r.check("backup folder "+files.DefaultBackupRoot, nil, "")
	}
	_, err := in.groupID(cfg.Server.SocketGroup)
	r.check("socket group "+cfg.Server.SocketGroup, err,
		"Create it with 'groupadd --system "+cfg.Server.SocketGroup+"', or set socket_group to a group that exists.")

	pol := policy.New(cfg)

	const linuxHint = "Only available on Linux."
	hostHint := func(err error) string {
		if errors.Is(err, host.ErrUnsupported) {
			return linuxHint
		}
		return ""
	}
	_, err = in.host.Load(ctx)
	r.check("host load", err, hostHint(err))
	_, err = in.host.Memory(ctx)
	r.check("host memory", err, hostHint(err))
	for _, d := range cfg.Host.Disks {
		_, err := in.host.Disk(ctx, d.Path)
		r.check("disk "+d.Name+" ("+d.Path+")", err, "Check that the path exists and is a mount point or directory.")
	}

	if err := in.docker.Ping(ctx); err != nil {
		r.check("docker daemon", err, "Is Docker running? Check docker.socket in the config ("+cfg.Docker.Socket+") and that the latch service can read it.")
	} else {
		r.check("docker daemon", nil, "")
		names, err := in.docker.List(ctx)
		r.check("docker containers"+visible(names, err, pol.ContainerVisible), err, "Can the latch service read the Docker socket?")
	}
	// docker_compose_up needs the CLI with the compose plugin; nothing else
	// does, so a missing one is a warning, not a problem.
	if bin, err := in.compose(ctx); err != nil {
		r.warn("docker compose", err.Error()+"; docker_compose_up will fail",
			"Install the docker CLI and the docker-compose-plugin package if you want docker_compose_up.")
	} else {
		r.check("docker compose ("+bin+")", nil, "")
	}

	if err := in.systemd.Ping(ctx); err != nil {
		r.check("systemd bus", err, "Is systemd running, and can the latch service reach /run/dbus/system_bus_socket?")
	} else {
		r.check("systemd bus", nil, "")
		units, err := in.systemd.List(ctx)
		var names []string
		for _, u := range units {
			names = append(names, u.Name)
		}
		r.check("systemd services"+visible(names, err, pol.UnitVisible), err, "Can the latch service reach the system bus?")
	}

	for _, t := range cfg.Health.DNS {
		res, err := in.health.DNS(ctx, t)
		if err == nil && !res.OK {
			err = errors.New(string(res.Failure))
		}
		r.check("dns check "+t.Name, err, "Check the hostname and that this server can reach a DNS resolver.")
	}

	r.summary()
	return r.failed == 0
}

// visible describes how many of names the agent can see, e.g. " (12 visible, 2 excluded)".
func visible(names []string, err error, ok func(string) bool) string {
	if err != nil {
		return ""
	}
	n := 0
	for _, name := range names {
		if ok(name) {
			n++
		}
	}
	return fmt.Sprintf(" (%d visible, %d excluded)", n, len(names)-n)
}
