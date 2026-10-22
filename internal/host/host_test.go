package host

import (
	"bufio"
	"context"
	"errors"
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

func TestDiskOnRoot(t *testing.T) {
	d, err := System{}.Disk(context.Background(), "/")
	if err != nil {
		t.Fatal(err)
	}
	if d.TotalBytes == 0 || d.UsedBytes > d.TotalBytes {
		t.Errorf("implausible: %+v", d)
	}
}

func TestMissingProcIsUnsupported(t *testing.T) {
	s := System{ProcRoot: t.TempDir()}
	if _, err := s.Load(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("got %v", err)
	}
}
