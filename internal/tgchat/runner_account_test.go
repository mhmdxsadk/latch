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
	dir := filepath.Join(t.TempDir(), "a", "b")
	if err := os.Chmod(filepath.Dir(filepath.Dir(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := me.MakeDir(ctx, dir); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "x.db")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := me.Remove(ctx, f, f+"-wal"); err != nil {
		t.Fatal(err)
	}
}
