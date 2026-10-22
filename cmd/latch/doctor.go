package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/mhmdxsadk/latch/internal/config"
	"github.com/mhmdxsadk/latch/internal/host"
)

// cmdDoctor checks host capabilities. It never starts the server and never
// prints configuration values that could be sensitive beyond resource names.
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

	failed := false
	check := func(name string, err error) {
		if err != nil {
			failed = true
			fmt.Fprintf(stdout, "FAIL  %s: %v\n", name, err)
			return
		}
		fmt.Fprintf(stdout, "ok    %s\n", name)
	}

	check("config", nil)

	dir := filepath.Dir(cfg.Server.Socket)
	if fi, err := os.Stat(dir); err != nil {
		check("socket directory "+dir, err)
	} else if !fi.IsDir() {
		check("socket directory "+dir, fmt.Errorf("not a directory"))
	} else if fi.Mode().Perm()&0o002 != 0 {
		check("socket directory "+dir, fmt.Errorf("world-writable; use a dedicated directory"))
	} else {
		check("socket directory "+dir, nil)
	}

	if len(cfg.Docker.Services) > 0 {
		fi, err := os.Stat(cfg.Docker.Socket)
		if err == nil && fi.Mode()&os.ModeSocket == 0 {
			err = fmt.Errorf("not a socket")
		}
		check("docker socket", err)
	}

	h := host.System{}
	ctx := context.Background()
	_, err = h.Load(ctx)
	check("host load", err)
	_, err = h.Memory(ctx)
	check("host memory", err)
	for _, d := range cfg.Host.Disks {
		_, err := h.Disk(ctx, d.Path)
		check("disk "+d.Name, err)
	}

	if failed {
		return 1
	}
	return 0
}
