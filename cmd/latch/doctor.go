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
	"github.com/mhmdxsadk/latch/internal/systemd"
)

const doctorTimeout = 2 * time.Minute

// integrations are what doctor exercises; tests substitute fakes.
type integrations struct {
	host    host.Provider
	docker  docker.Client
	systemd systemd.Client
	health  health.Checker
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
	}, stdout)
	if !ok {
		return 1
	}
	return 0
}

func doctor(ctx context.Context, cfg *config.Config, in integrations, out io.Writer) bool {
	failed := false
	check := func(name string, err error) {
		if err != nil {
			failed = true
			fmt.Fprintf(out, "FAIL  %s: %v\n", name, err)
			return
		}
		fmt.Fprintf(out, "ok    %s\n", name)
	}

	check("config", nil)

	dir := filepath.Dir(cfg.Server.Socket)
	if fi, err := os.Stat(dir); err != nil {
		check("socket directory "+dir, err)
	} else if !fi.IsDir() {
		check("socket directory "+dir, errors.New("not a directory"))
	} else if fi.Mode().Perm()&0o002 != 0 {
		check("socket directory "+dir, errors.New("world-writable; use a dedicated directory"))
	} else {
		check("socket directory "+dir, nil)
	}

	_, err := in.host.Load(ctx)
	check("host load", err)
	_, err = in.host.Memory(ctx)
	check("host memory", err)
	for _, d := range cfg.Host.Disks {
		_, err := in.host.Disk(ctx, d.Path)
		check("disk "+d.Name, err)
	}

	if len(cfg.Docker.Services) > 0 {
		if err := in.docker.Ping(ctx); err != nil {
			check("docker daemon", err)
		} else {
			check("docker daemon", nil)
			for _, s := range cfg.Docker.Services {
				_, err := in.docker.Status(ctx, s.Container)
				check(fmt.Sprintf("docker service %s (container %s)", s.Name, s.Container), err)
			}
		}
	}

	if len(cfg.Systemd.Units) > 0 {
		if err := in.systemd.Ping(ctx); err != nil {
			check("systemd", err)
		} else {
			check("systemd", nil)
			for _, u := range cfg.Systemd.Units {
				_, err := in.systemd.UnitStatus(ctx, u)
				check("systemd unit "+u, err)
			}
		}
	}

	for _, t := range cfg.Health.HTTP {
		r, err := in.health.HTTP(ctx, t)
		check("http check "+t.Name, httpProblem(t, r, err))
	}
	for _, t := range cfg.Health.DNS {
		r, err := in.health.DNS(ctx, t)
		if err == nil && !r.OK {
			err = errors.New(string(r.Failure))
		}
		check("dns check "+t.Name, err)
	}

	return !failed
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
