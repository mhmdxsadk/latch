package host

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLoadavg(t *testing.T) {
	l, err := ParseLoadavg("0.52 0.31 0.20 1/234 5678\n")
	if err != nil || l.Load1 != 0.52 || l.Load15 != 0.20 {
		t.Fatalf("got %+v, %v", l, err)
	}
	for _, bad := range []string{"", "a b c", "1 2"} {
		if _, err := ParseLoadavg(bad); err == nil {
			t.Errorf("ParseLoadavg(%q) should fail", bad)
		}
	}
}

func TestParseMeminfo(t *testing.T) {
	in := "MemTotal:       1000 kB\nMemFree: 10 kB\nMemAvailable:    400 kB\nSwapTotal: 8 kB\nSwapFree: 4 kB\nbogus line\n"
	m, err := ParseMeminfo(bufio.NewScanner(strings.NewReader(in)))
	if err != nil {
		t.Fatal(err)
	}
	if m.TotalBytes != 1000*1024 || m.UsedBytes != 600*1024 || m.SwapFreeBytes != 4*1024 {
		t.Errorf("got %+v", m)
	}
	// Missing MemAvailable and available > total must both fail rather than
	// report misleading numbers.
	for _, bad := range []string{"MemTotal: 1 kB\n", "MemTotal: 1 kB\nMemAvailable: 2 kB\n", ""} {
		if _, err := ParseMeminfo(bufio.NewScanner(strings.NewReader(bad))); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestStatfsOnRoot(t *testing.T) {
	d, err := statfs("/")
	if err != nil {
		t.Fatal(err)
	}
	if d.TotalBytes == 0 || d.UsedBytes > d.TotalBytes {
		t.Errorf("implausible: %+v", d)
	}
}

func TestDisksSkipsPseudoAndBindMounts(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	mounts := "/dev/sda1 / ext4 rw 0 0\n" +
		"proc /proc proc rw 0 0\n" +
		"tmpfs /run tmpfs rw 0 0\n" +
		"overlay /var/lib/docker/overlay2/x/merged overlay rw 0 0\n" +
		"/dev/sda1 /srv/bind ext4 rw 0 0\n" +
		"/dev/sdb1 " + strings.ReplaceAll(tmp, " ", `\040`) + " xfs rw 0 0\n" +
		"/dev/sdc1 /does/not/exist ext4 rw 0 0\n"
	if err := os.WriteFile(filepath.Join(root, "self", "mounts"), []byte(mounts), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := System{ProcRoot: root}.Disks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Mount != "/" || got[0].Type != "ext4" || got[1].Mount != tmp || got[1].Type != "xfs" {
		t.Errorf("got %+v", got)
	}
}

func TestParseMountsUnescapes(t *testing.T) {
	m := ParseMounts("/dev/sda1 /mnt/my\\040disk ext4 rw 0 0\nshort\n")
	if len(m) != 1 || m[0].Mount != "/mnt/my disk" || m[0].Source != "/dev/sda1" {
		t.Errorf("got %+v", m)
	}
}

func TestMissingProcIsUnsupported(t *testing.T) {
	s := System{ProcRoot: t.TempDir()}
	if _, err := s.Load(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("got %v", err)
	}
}
