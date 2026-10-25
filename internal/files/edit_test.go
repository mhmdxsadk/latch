package files

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// realDir returns a new temporary directory with symlinks resolved (on
// macOS /var is a symlink, which the backup tree refuses).
func realDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func newEditorFixture(t *testing.T) (*fixture, *Editor) {
	t.Helper()
	f := newFixture(t)
	b := NewBackups(filepath.Join(realDir(t), "backups"))
	b.now = func() time.Time { return time.Date(2026, 10, 25, 14, 30, 0, 0, time.UTC) }
	return f, NewEditor(f.guard, b)
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestReplaceKeepsOwnerModeAndBacksUp(t *testing.T) {
	f, e := newEditorFixture(t)
	ctx := context.Background()
	p := f.write("public/app.conf", "port=1\npassword="+planSecret+"\n")
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(p)

	file, err := e.Load(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if file.Mode != 0o640 || !strings.HasSuffix(file.Path, "/public/app.conf") {
		t.Fatalf("loaded %+v", file)
	}
	pl, err := PlanEdit(file.Content, "port=1", "port=2")
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Replace(ctx, p, file, pl.Content, true)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "port=2\npassword="+planSecret+"\n" || string(res.Old) != string(file.Content) {
		t.Fatalf("content %q", got)
	}
	after, _ := os.Stat(p)
	if after.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", after.Mode())
	}
	bs, as := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
	if bs.Uid != as.Uid || bs.Gid != as.Gid {
		t.Errorf("owner %d:%d -> %d:%d", bs.Uid, bs.Gid, as.Uid, as.Gid)
	}
	if res.BackupID != "20261025T143000Z" {
		t.Errorf("backup id %q", res.BackupID)
	}

	// Layout: <root>/<full path>.<timestamp>, directories 0700, file 0600.
	bp := filepath.Join(e.backups.Root(), file.Path+".20261025T143000Z")
	data, err := os.ReadFile(bp)
	if err != nil || string(data) != string(file.Content) {
		t.Fatalf("backup %q %v", data, err)
	}
	if fi, _ := os.Stat(bp); fi.Mode().Perm() != 0o600 {
		t.Errorf("backup mode %v", fi.Mode())
	}
	for d := filepath.Dir(bp); d != filepath.Dir(e.backups.Root()); d = filepath.Dir(d) {
		if fi, _ := os.Stat(d); fi.Mode().Perm() != 0o700 {
			t.Errorf("%s mode %v", d, fi.Mode())
		}
	}
	if n := names(t, filepath.Dir(p)); len(n) != 2 { // hello.txt, app.conf
		t.Errorf("leftover files %v", n)
	}
}

func TestReplaceRefusesChangedFile(t *testing.T) {
	f, e := newEditorFixture(t)
	ctx := context.Background()
	p := f.write("public/app.conf", "a=1\n")
	file, err := e.Load(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	f.write("public/app.conf", "a=1\nb=2\n")
	if _, err := e.Replace(ctx, p, file, []byte("a=2\n"), true); !errors.Is(err, ErrChanged) {
		t.Fatalf("err %v", err)
	}
	if got, _ := os.ReadFile(p); string(got) != "a=1\nb=2\n" {
		t.Errorf("overwritten: %q", got)
	}
	if l, _ := e.backups.List(file.Path); len(l) != 0 {
		t.Errorf("backup made for a refused edit: %v", l)
	}
}

func TestReplaceFailureLeavesNoTempFile(t *testing.T) {
	f, e := newEditorFixture(t)
	ctx := context.Background()
	p := f.write("public/app.conf", "a=1\n")
	file, _ := e.Load(ctx, p)
	e.beforeCommit = func() error { return errors.New("disk full") }
	if _, err := e.Replace(ctx, p, file, []byte("a=2\n"), false); err == nil {
		t.Fatal("no error")
	}
	if got, _ := os.ReadFile(p); string(got) != "a=1\n" {
		t.Errorf("content %q", got)
	}
	for _, n := range names(t, filepath.Dir(p)) {
		if strings.Contains(n, "latch") {
			t.Errorf("temp file left: %s", n)
		}
	}
	nf, err := e.ResolveNew(ctx, f.path("public/new.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Create(ctx, f.path("public/new.conf"), nf, []byte("x\n"), 0o644); err == nil {
		t.Fatal("create: no error")
	}
	for _, n := range names(t, filepath.Dir(p)) {
		if strings.Contains(n, "latch") || n == "new.conf" {
			t.Errorf("file left: %s", n)
		}
	}
}

func TestEditThroughSymlink(t *testing.T) {
	f, e := newEditorFixture(t)
	ctx := context.Background()
	target := f.write("public/real.conf", "a=1\n")
	link := f.symlink(target, "public/link.conf")
	file, err := e.Load(ctx, link)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(file.Path, "/public/real.conf") {
		t.Fatalf("path %q", file.Path)
	}
	if _, err := e.Replace(ctx, link, file, []byte("a=2\n"), false); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&fs.ModeSymlink == 0 {
		t.Error("symlink replaced by a file")
	}
	if got, _ := os.ReadFile(target); string(got) != "a=2\n" {
		t.Errorf("target %q", got)
	}
}

// A symlink swapped between request and execution must not redirect the
// write: into a blocked folder it is blocked, and to another allowed file
// the resolved path no longer matches.
func TestEditSymlinkSwap(t *testing.T) {
	f, e := newEditorFixture(t)
	ctx := context.Background()
	a := f.write("public/a.conf", "same\n")
	other := f.write("public/b.conf", "same\n")
	link := f.symlink(a, "public/link.conf")
	file, err := e.Load(ctx, link)
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []struct {
		target string
		want   error
	}{
		{filepath.Join(f.blocked, "secret.txt"), ErrBlocked},
		{other, ErrChanged},
		{f.path("container-data/db.conf"), ErrBlocked},
	} {
		os.Remove(link)
		if err := os.Symlink(to.target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Replace(ctx, link, file, []byte("evil\n"), true); !errors.Is(err, to.want) {
			t.Errorf("swap to %s: %v, want %v", to.target, err, to.want)
		}
	}
	for _, p := range []string{filepath.Join(f.blocked, "secret.txt"), other, a} {
		if got, _ := os.ReadFile(p); strings.Contains(string(got), "evil") {
			t.Errorf("%s written", p)
		}
	}
}

func TestBlockedFilesCannotBeEditedOrCreated(t *testing.T) {
	f, e := newEditorFixture(t)
	ctx := context.Background()
	f.write("public/app.key", "k\n")
	f.write("public/x.secret", "s\n")
	f.symlink(f.blocked, "public/to-private")
	for _, p := range []string{
		filepath.Join(f.blocked, "secret.txt"),
		f.path("public/app.key"),
		f.path("public/x.secret"),
		f.path("public/to-private/secret.txt"),
		f.cfg,
		f.path("home/agent/.codex/auth.json"),
		f.path("container-data/db.conf"),
	} {
		if _, err := e.Load(ctx, p); !errors.Is(err, ErrBlocked) {
			t.Errorf("load %s: %v", p, err)
		}
	}
	for _, p := range []string{
		filepath.Join(f.blocked, "new.txt"),
		f.path("public/to-private/new.txt"),
		f.path("public/new.pem"),
		f.path("public/.env"),
		f.path("container-data/new.conf"),
	} {
		if _, err := e.ResolveNew(ctx, p); !errors.Is(err, ErrBlocked) {
			t.Errorf("create %s: %v", p, err)
		}
	}
	if _, err := e.Locate(ctx, filepath.Join(f.blocked, "secret.txt")); !errors.Is(err, ErrBlocked) {
		t.Errorf("locate: %v", err)
	}
}

func TestLoadRefusals(t *testing.T) {
	f, e := newEditorFixture(t)
	ctx := context.Background()
	bin := f.write("public/bin", "a\x00b")
	big := f.write("public/big", strings.Repeat("a", MaxEditBytes+1))
	suid := f.write("public/suid.sh", "#!/bin/sh\n")
	if err := os.Chmod(suid, 0o755|os.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]error{
		bin: ErrBinary, big: ErrTooLarge, suid: ErrSpecialBits,
		f.path("public"): ErrNotFile, f.path("public/missing"): ErrNotFound, "relative": ErrInvalidPath,
	} {
		if _, err := e.Load(ctx, p); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", p, err, want)
		}
	}
}

func TestCreate(t *testing.T) {
	f, e := newEditorFixture(t)
	ctx := context.Background()
	p := f.path("public/new.conf")
	nf, err := e.ResolveNew(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Create(ctx, p, nf, []byte("hello\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	parent, _ := os.Stat(f.path("public"))
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", fi.Mode())
	}
	ps, cs := parent.Sys().(*syscall.Stat_t), fi.Sys().(*syscall.Stat_t)
	if ps.Uid != cs.Uid || ps.Gid != cs.Gid {
		t.Errorf("owner %d:%d, parent %d:%d", cs.Uid, cs.Gid, ps.Uid, ps.Gid)
	}
	if err := e.Create(ctx, p, nf, []byte("again\n"), 0o640); !errors.Is(err, ErrExists) {
		t.Errorf("second create: %v", err)
	}
	if _, err := e.ResolveNew(ctx, p); !errors.Is(err, ErrExists) {
		t.Errorf("resolve existing: %v", err)
	}
	// Something appearing at the name after the request is never replaced.
	q := f.path("public/late.conf")
	nf, _ = e.ResolveNew(ctx, q)
	f.symlink(filepath.Join(f.blocked, "secret.txt"), "public/late.conf")
	if err := e.Create(ctx, q, nf, []byte("x\n"), 0o644); !errors.Is(err, ErrExists) {
		t.Errorf("late: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(f.blocked, "secret.txt")); string(got) != "s3cret\n" {
		t.Errorf("blocked file written: %q", got)
	}
	if err := e.Create(ctx, f.path("public/s"), NewFile{}, nil, 0o4755); !errors.Is(err, ErrSpecialBits) {
		t.Errorf("setuid: %v", err)
	}
	if _, err := e.ResolveNew(ctx, f.path("public/hello.txt/x")); !errors.Is(err, ErrNotDir) && !errors.Is(err, ErrNotFound) {
		t.Errorf("parent is a file: %v", err)
	}
	if _, err := e.ResolveNew(ctx, f.path("missing/x")); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing parent: %v", err)
	}
}

func TestIsUnitPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/etc/systemd/system/web.service":                  true,
		"/etc/systemd/system/web.service.d/override.conf":  true,
		"/run/systemd/system/x.service":                    true,
		"/usr/lib/systemd/system/ssh.service":              true,
		"/lib/systemd/system/cron.service":                 true,
		"/etc/systemd/systemx/web.service":                 false,
		"/etc/systemd/journald.conf":                       false,
		"/etc/nginx/nginx.conf":                            false,
		"/home/u/.config/systemd/user/x.service":           false,
		"/etc/systemd/system":                              false,
		"/srv/etc/systemd/system/web.service":              false,
		"/usr/lib/systemd/system/getty@.service.d/x.conf": true,
	} {
		if IsUnitPath(p) != want {
			t.Errorf("%s: want %v", p, want)
		}
	}
}
