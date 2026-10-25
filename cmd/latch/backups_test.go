package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/mhmdxsadk/latch/internal/files"
)

func TestBackupsCommand(t *testing.T) {
	dir := t.TempDir()
	dir, _ = filepath.EvalSymlinks(dir)
	root := filepath.Join(dir, "backups")
	target := filepath.Join(dir, "etc", "app.conf")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := run([]string{"backups", "-backup-dir", root, target}, &out, &errb); code != 0 || out.String() != "latch: "+target+": no backups\n" {
		t.Fatalf("empty: %d %q %q", code, out.String(), errb.String())
	}
	if _, err := files.NewBackups(root).Save(target, []byte("old\n")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run([]string{"backups", "-backup-dir", root, target}, &out, &errb); code != 0 {
		t.Fatalf("list: %d %q", code, errb.String())
	}
	want := regexp.MustCompile(`^latch: ` + regexp.QuoteMeta(target) + `: 1 backup\(s\), newest first\n[0-9]{8}T[0-9]{6}Z  4 bytes\n$`)
	if !want.MatchString(out.String()) {
		t.Errorf("got %q", out.String())
	}

	errb.Reset()
	if code := run([]string{"backups", "relative"}, &out, &errb); code != 2 {
		t.Errorf("relative path: %d %q", code, errb.String())
	}
}
