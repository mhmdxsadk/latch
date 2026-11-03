package files

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMoveRenamesFileAndFolderWhole(t *testing.T) {
	f, e := newEditorFixture(t)
	ctx := context.Background()

	// A file keeps its inode, content and mode.
	src := f.path("public/hello.txt")
	before, _ := os.Stat(src)
	dst := f.path("public/renamed.txt")
	m, err := e.ResolveMove(ctx, src, dst)
	if err != nil || m.IsDir {
		t.Fatalf("plan %+v, %v", m, err)
	}
	if err := e.Move(ctx, src, dst, m); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(dst)
	if err != nil || !os.SameFile(before, after) || after.Mode() != before.Mode() {
		t.Errorf("file %v %v", after, err)
	}
	if _, err := os.Lstat(src); err == nil {
		t.Error("old name still there")
	}

	// A folder moves with everything in it, including SQLite side files.
	f.write("public/stack/db/data.sqlite", "main")
	f.write("public/stack/db/data.sqlite-wal", "wal")
	f.write("public/stack/db/data.sqlite-shm", "shm")
	f.mkdir("public/target")
	from, to := f.path("public/stack"), f.path("public/target/stack-new")
	m, err = e.ResolveMove(ctx, from, to)
	if err != nil || !m.IsDir {
		t.Fatalf("plan %+v, %v", m, err)
	}
	if err := e.Move(ctx, from, to, m); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"data.sqlite": "main", "data.sqlite-wal": "wal", "data.sqlite-shm": "shm"} {
		if got := read(t, filepath.Join(to, "db", name)); got != want {
			t.Errorf("%s = %q", name, got)
		}
	}
	if _, err := os.Lstat(from); err == nil {
		t.Error("old folder still there")
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestResolveMoveRefusals(t *testing.T) {
	f, e := newEditorFixture(t)
	ctx := context.Background()
	f.write("public/a.txt", "a")
	f.write("public/b.txt", "b")
	f.mkdir("public/folder")
	f.symlink(f.path("public/a.txt"), "public/link")
	f.write("public/.ssh/id_x", "key")
	other := realDir(t)

	for _, c := range []struct {
		name     string
		from, to string
		want     error
	}{
		{"target exists", f.path("public/a.txt"), f.path("public/b.txt"), ErrExists},
		{"folder target exists", f.path("public/a.txt"), f.path("public/folder"), ErrExists},
		{"missing source", f.path("public/none"), f.path("public/x"), ErrNotFound},
		{"missing target folder", f.path("public/a.txt"), f.path("nofolder/x"), ErrNotFound},
		{"symlink source", f.path("public/link"), f.path("public/x"), ErrSymlink},
		{"blocked source", f.path("private/secret.txt"), f.path("public/x"), ErrBlocked},
		{"blocked folder source", f.blocked, f.path("public/x"), ErrBlocked},
		{"blocked target", f.path("public/a.txt"), f.path("private/x"), ErrBlocked},
		{"excluded container folder", f.path("container-data"), f.path("public/x"), ErrBlocked},
		{"blocked name as target", f.path("public/a.txt"), f.path("public/.env"), ErrBlocked},
		{"folder holding blocked trees", f.root, filepath.Join(other, "x"), ErrBlocked},
		{"folder holding an agent home", f.path("home"), f.path("public/x"), ErrBlocked},
		{"folder named .ssh", f.path("public/.ssh"), f.path("public/sshcopy"), ErrBlocked},
		{"into itself", f.path("public/folder"), f.path("public/folder/inner"), ErrMoveInside},
		{"root", "/", f.path("public/x"), ErrInvalidPath},
		{"relative", "public/a.txt", f.path("public/x"), ErrInvalidPath},
	} {
		if _, err := e.ResolveMove(ctx, c.from, c.to); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	// Nothing was moved by any refusal.
	for _, p := range []string{"public/a.txt", "public/b.txt", "public/folder", "public/.ssh/id_x", "private/secret.txt"} {
		if _, err := os.Lstat(f.path(p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestMoveRechecksThePlan(t *testing.T) {
	f, e := newEditorFixture(t)
	ctx := context.Background()
	src, dst := f.path("public/a.txt"), f.path("public/moved.txt")
	f.write("public/a.txt", "a")

	m, err := e.ResolveMove(ctx, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	// A different file at the name after the request is not moved.
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	f.write("public/a.txt", "other")
	if err := e.Move(ctx, src, dst, m); !errors.Is(err, ErrChanged) {
		t.Errorf("replaced source: %v", err)
	}
	// Something at the new path after the request is never replaced.
	m, _ = e.ResolveMove(ctx, src, dst)
	f.write("public/moved.txt", "late")
	if err := e.Move(ctx, src, dst, m); !errors.Is(err, ErrExists) {
		t.Errorf("late target: %v", err)
	}
	if got := read(t, dst); got != "late" {
		t.Errorf("target now %q", got)
	}
	// A plan for other paths is stale.
	if err := e.Move(ctx, src, f.path("public/third.txt"), m); !errors.Is(err, ErrChanged) {
		t.Errorf("stale plan: %v", err)
	}
	// The hook runs right before the rename, where a failure is injected.
	os.Remove(dst)
	m, _ = e.ResolveMove(ctx, src, dst)
	e.beforeCommit = func() error { return errors.New("injected") }
	if err := e.Move(ctx, src, dst, m); err == nil || read(t, src) != "other" {
		t.Errorf("injected failure: %v", err)
	}
}
