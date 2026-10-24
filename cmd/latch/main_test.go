package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhmdxsadk/latch/internal/config"
	"github.com/mhmdxsadk/latch/internal/files"
)

func TestNewFileGuardBlocksConfigAndWarns(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "latch.yaml")
	data := "server: {socket: /run/l.sock, socket_group: no-such-group-xyz}\nfiles:\n  exclude: ['*.secret']\n"
	if err := os.WriteFile(cfgPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("ok"), 0o600)
	os.WriteFile(filepath.Join(dir, "a.secret"), []byte("x"), 0o600)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var errb bytes.Buffer
	g, err := newFileGuard(cfg, cfgPath, nil, &errb)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errb.String(), "latch: warning: agent home directories") {
		t.Errorf("no warning for an unknown socket group: %q", errb.String())
	}
	ctx := context.Background()
	for _, p := range []string{cfgPath, filepath.Join(dir, "a.secret"), "/etc/shadow"} {
		if _, err := g.Check(ctx, p); !errors.Is(err, files.ErrBlocked) {
			t.Errorf("%s: %v", p, err)
		}
	}
	if _, err := g.Check(ctx, filepath.Join(dir, "ok.txt")); err != nil {
		t.Errorf("ok.txt: %v", err)
	}
}

func TestRunDispatch(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, &out, &errb); code != 2 {
		t.Errorf("no args: code %d", code)
	}
	if code := run([]string{"bogus"}, &out, &errb); code != 2 {
		t.Errorf("unknown command: code %d", code)
	}
	for _, shell := range []string{"exec", "sh", "docker"} {
		if code := run([]string{shell}, &out, &errb); code != 2 {
			t.Errorf("%q must not be a command", shell)
		}
	}
	out.Reset()
	if code := run([]string{"version"}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "latch ") {
		t.Errorf("version: %d %q", code, out.String())
	}
	for _, alias := range []string{"--version", "-v"} {
		var again bytes.Buffer
		if code := run([]string{alias}, &again, &errb); code != 0 || again.String() != out.String() {
			t.Errorf("%s must print the same as version: %q vs %q", alias, again.String(), out.String())
		}
	}
}

func TestVersionText(t *testing.T) {
	tests := []struct{ version, commit, want string }{
		{"0.2.1", "ada46060e063e1766edc78a8b782c1ef3b60cc9a", "latch 0.2.1\ncommit ada4606, linux/amd64\n"},
		{"0.2.1", "abc", "latch 0.2.1\ncommit abc, linux/amd64\n"},
		{"dev", "none", "latch dev\nlinux/amd64\n"},
		{"dev", "", "latch dev\nlinux/amd64\n"},
	}
	for _, tt := range tests {
		if got := versionText(tt.version, tt.commit, "linux", "amd64"); got != tt.want {
			t.Errorf("versionText(%q,%q) = %q, want %q", tt.version, tt.commit, got, tt.want)
		}
	}
}

func TestValidateCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"validate", "-config", "../../config/example.yaml"}, &out, &errb); code != 0 {
		t.Fatalf("example config should validate: %s", errb.String())
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	os.WriteFile(bad, []byte("commands: [id]\n"), 0o600)
	errb.Reset()
	if code := run([]string{"validate", "-config", bad}, &out, &errb); code != 1 {
		t.Errorf("bad config should exit 1, got %d", code)
	}
	if code := run([]string{"validate", "-config", "/nonexistent.yaml"}, &out, &errb); code != 1 {
		t.Errorf("missing config should exit 1, got %d", code)
	}
}

func TestServeRefusesInvalidConfig(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	os.WriteFile(bad, []byte("server: {socket: relative}\n"), 0o600)
	var errb bytes.Buffer
	if code := cmdServe([]string{"-config", bad}, &errb); code != 1 {
		t.Errorf("serve must fail closed on invalid config, got %d", code)
	}
}
