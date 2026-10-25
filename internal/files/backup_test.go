package files

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func testBackups(t *testing.T) (*Backups, *time.Time) {
	t.Helper()
	b := NewBackups(filepath.Join(realDir(t), "lib", "backups"))
	now := time.Date(2026, 10, 25, 14, 30, 0, 0, time.UTC)
	b.now = func() time.Time { return now }
	return b, &now
}

func ids(l []Backup) []string {
	var out []string
	for _, b := range l {
		out = append(out, b.ID)
	}
	return out
}

func TestBackupsKeepThreeNewest(t *testing.T) {
	b, now := testBackups(t)
	for i := range 5 {
		if _, err := b.Save("/etc/app/app.conf", []byte{byte('a' + i)}); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Hour)
	}
	l, err := b.List("/etc/app/app.conf")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"20261025T183000Z", "20261025T173000Z", "20261025T163000Z"}; !slices.Equal(ids(l), want) {
		t.Fatalf("ids %v", ids(l))
	}
	if l[0].Size != 1 || !l[0].Time.Equal(time.Date(2026, 10, 25, 18, 30, 0, 0, time.UTC)) {
		t.Errorf("first %+v", l[0])
	}
	data, err := b.Read("/etc/app/app.conf", "20261025T163000Z")
	if err != nil || string(data) != "c" {
		t.Errorf("read %q %v", data, err)
	}
	// Other files in the same folder are separate.
	if l, _ := b.List("/etc/app/app"); len(l) != 0 {
		t.Errorf("prefix file: %v", l)
	}
	if l, _ := b.List("/etc/other.conf"); l == nil || len(l) != 0 {
		t.Errorf("no backups: %#v", l)
	}
}

func TestBackupsCollisionSuffix(t *testing.T) {
	b, _ := testBackups(t)
	var got []string
	for range 4 {
		id, err := b.Save("/srv/x.conf", []byte("x"))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if want := []string{"20261025T143000Z", "20261025T143000Z-2", "20261025T143000Z-3", "20261025T143000Z-4"}; !slices.Equal(got, want) {
		t.Fatalf("ids %v", got)
	}
	l, _ := b.List("/srv/x.conf")
	if want := []string{"20261025T143000Z-4", "20261025T143000Z-3", "20261025T143000Z-2"}; !slices.Equal(ids(l), want) {
		t.Errorf("kept %v", ids(l))
	}
	for _, bad := range []string{"", "latest", "../x", "20261025T143000Z-", "20261025T143000Z/../../x", "20261025T143000Z-1000"} {
		if _, err := b.Read("/srv/x.conf", bad); !errors.Is(err, ErrNoBackup) {
			t.Errorf("id %q: %v", bad, err)
		}
	}
	if _, err := b.Read("/srv/x.conf", "20201025T143000Z"); !errors.Is(err, ErrNoBackup) {
		t.Errorf("missing id: %v", err)
	}
}

func TestBackupsRefuseSymlinksAndLoosePermissions(t *testing.T) {
	b, _ := testBackups(t)
	if _, err := b.Save("/etc/a.conf", []byte("x")); err != nil {
		t.Fatal(err)
	}
	// A symlinked directory inside the tree is refused, for writing and
	// reading alike.
	elsewhere := realDir(t)
	if err := os.Symlink(elsewhere, filepath.Join(b.Root(), "srv")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Save("/srv/b.conf", []byte("x")); !errors.Is(err, ErrBackupUnsafe) {
		t.Errorf("save through symlink: %v", err)
	}
	if _, err := b.List("/srv/b.conf"); !errors.Is(err, ErrBackupUnsafe) {
		t.Errorf("list through symlink: %v", err)
	}
	if n := names(t, elsewhere); len(n) != 0 {
		t.Errorf("wrote through the symlink: %v", n)
	}
	// A symlinked backup file is not read.
	if err := os.Symlink("/etc/hosts", filepath.Join(b.Root(), "etc", "a.conf.20261025T150000Z")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Read("/etc/a.conf", "20261025T150000Z"); !errors.Is(err, ErrBackupUnsafe) {
		t.Errorf("read symlink: %v", err)
	}
	if l, _ := b.List("/etc/a.conf"); len(l) != 1 {
		t.Errorf("symlink listed: %v", l)
	}
	// A directory others can read is refused.
	if err := os.Chmod(filepath.Join(b.Root(), "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Save("/etc/a.conf", []byte("y")); !errors.Is(err, ErrBackupUnsafe) {
		t.Errorf("loose permissions: %v", err)
	}
	// The root itself as a symlink is refused.
	link := filepath.Join(realDir(t), "backups")
	if err := os.Symlink(b.Root(), link); err != nil {
		t.Fatal(err)
	}
	lb := NewBackups(link)
	if _, err := lb.Save("/x/y", []byte("x")); !errors.Is(err, ErrBackupUnsafe) {
		t.Errorf("root symlink: %v", err)
	}
	if _, err := CheckBackupRoot(link, os.Geteuid()); !errors.Is(err, ErrBackupUnsafe) {
		t.Errorf("check root symlink: %v", err)
	}
}

func TestCheckBackupRoot(t *testing.T) {
	b, _ := testBackups(t)
	if ok, err := CheckBackupRoot(b.Root(), os.Geteuid()); ok || err != nil {
		t.Errorf("absent: %v %v", ok, err)
	}
	if _, err := b.Save("/a", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if ok, err := CheckBackupRoot(b.Root(), os.Geteuid()); !ok || err != nil {
		t.Errorf("present: %v %v", ok, err)
	}
	if _, err := CheckBackupRoot(b.Root(), os.Geteuid()+1); !errors.Is(err, ErrBackupUnsafe) {
		t.Errorf("wrong owner: %v", err)
	}
	if err := os.Chmod(b.Root(), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckBackupRoot(b.Root(), os.Geteuid()); !errors.Is(err, ErrBackupUnsafe) {
		t.Errorf("group readable: %v", err)
	}
}
