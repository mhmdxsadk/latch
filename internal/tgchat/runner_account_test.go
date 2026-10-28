package tgchat

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The helpers switch to the agent's account, which needs root. They run as
// the current user when it is root, and the lisbon check covers the rest.
func TestAccountFileHelpers(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to switch accounts")
	}
	me, err := LookupAccount("nobody")
	if err != nil {
		t.Skip("no nobody account")
	}
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "a", "b")
	// The account must be able to walk down to tmp and create dir in it.
	for d, mode := range map[string]os.FileMode{filepath.Dir(tmp): 0o755, tmp: 0o777} {
		if err := os.Chmod(d, mode); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	if err := me.MakeDir(ctx, dir); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "x.db")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	photo := filepath.Join(dir, "photo-1.jpg")
	if err := me.WriteFile(ctx, photo, []byte("pixels")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(photo); err != nil || string(b) != "pixels" {
		t.Fatalf("photo %q, %v", b, err)
	}
	if err := me.WriteFile(ctx, photo, []byte("other")); err == nil {
		t.Fatal("overwrote an existing file")
	}
	if err := me.Remove(ctx, f, f+"-wal", photo); err != nil {
		t.Fatal(err)
	}
}
