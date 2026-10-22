package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
