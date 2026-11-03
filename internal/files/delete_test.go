package files

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestDeleteFileKeepsBackupAndFolderMustBeEmpty(t *testing.T) {
	f, e := newEditorFixture(t)
	ctx := context.Background()

	p := f.write("public/old/compose.yaml", "services: {}\n")
	r, err := e.ResolveDelete(ctx, p)
	if err != nil || r.IsDir {
		t.Fatalf("plan %+v, %v", r, err)
	}
	id, err := e.Delete(ctx, p, r)
	if err != nil || id == "" {
		t.Fatalf("delete: %q %v", id, err)
	}
	if _, err := os.Lstat(p); err == nil {
		t.Error("file still there")
	}
	got, err := e.Backups().Read(r.Path, id)
	if err != nil || string(got) != "services: {}\n" {
		t.Errorf("backup %q %v", got, err)
	}

	// The folder is empty now.
	dir := f.path("public/old")
	r, err = e.ResolveDelete(ctx, dir)
	if err != nil || !r.IsDir {
		t.Fatalf("plan %+v, %v", r, err)
	}
	if id, err := e.Delete(ctx, dir, r); err != nil || id != "" {
		t.Fatalf("delete folder: %q %v", id, err)
	}
	if _, err := os.Lstat(dir); err == nil {
		t.Error("folder still there")
	}
}

func TestResolveDeleteRefusals(t *testing.T) {
	f, e := newEditorFixture(t)
	ctx := context.Background()
	f.write("public/full/a.txt", "a")
	f.write("public/a.txt", "a")
	f.symlink(f.path("public/a.txt"), "public/link")
	big := f.path("public/big.bin")
	if err := os.WriteFile(big, make([]byte, MaxEditBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	f.write("public/bin", "x\x00y")

	for _, c := range []struct {
		name string
		path string
		want error
	}{
		{"folder with content", f.path("public/full"), ErrNotEmpty},
		{"symlink", f.path("public/link"), ErrSymlink},
		{"missing", f.path("public/none"), ErrNotFound},
		{"blocked file", f.path("private/secret.txt"), ErrBlocked},
		{"blocked folder", f.blocked, ErrBlocked},
		{"excluded container folder", f.path("container-data"), ErrBlocked},
		{"too large", big, ErrTooLarge},
		{"binary", f.path("public/bin"), ErrBinary},
		{"root", "/", ErrInvalidPath},
		{"relative", "public/a.txt", ErrInvalidPath},
	} {
		if _, err := e.ResolveDelete(ctx, c.path); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	for _, p := range []string{"public/full/a.txt", "public/a.txt", "private/secret.txt"} {
		if _, err := os.Lstat(f.path(p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestDeleteRechecksThePlan(t *testing.T) {
	f, e := newEditorFixture(t)
	ctx := context.Background()
	p := f.write("public/a.txt", "a")
	r, err := e.ResolveDelete(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	// Changed content after the request is not deleted.
	if err := os.WriteFile(p, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Delete(ctx, p, r); !errors.Is(err, ErrChanged) {
		t.Errorf("changed content: %v", err)
	}
	// A plan for another path is stale.
	q := f.write("public/b.txt", "changed")
	if _, err := e.Delete(ctx, q, r); !errors.Is(err, ErrChanged) {
		t.Errorf("stale plan: %v", err)
	}
	// A failure injected before the unlink leaves the file.
	r, _ = e.ResolveDelete(ctx, p)
	e.beforeCommit = func() error { return errors.New("injected") }
	if _, err := e.Delete(ctx, p, r); err == nil || read(t, p) != "changed" {
		t.Errorf("injected failure: %v", err)
	}
}
