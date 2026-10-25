package ports

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/mhmdxsadk/latch/internal/host"
)

const (
	header  = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	dockID  = "4f3c2b1a4f3c2b1a4f3c2b1a4f3c2b1a4f3c2b1a4f3c2b1a4f3c2b1a4f3c2b1a"
	zero6   = "00000000000000000000000000000000:0000"
	dockID2 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func row(local, remote, state string, inode int) string {
	return "   0: " + local + " " + remote + " " + state + " 00000000:00000000 00:00000000 00000000     0        0 " +
		strconv.Itoa(inode) + " 1 0000000000000000 100 0 0 10 0\n"
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// addProc creates /proc/pid with comm, cgroup and one socket fd per inode.
func addProc(t *testing.T, root string, pid int, comm, cgroup string, inodes ...int) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	write(t, filepath.Join(dir, "comm"), comm+"\n")
	write(t, filepath.Join(dir, "cgroup"), cgroup)
	if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i, in := range inodes {
		if err := os.Symlink("socket:["+strconv.Itoa(in)+"]", filepath.Join(dir, "fd", strconv.Itoa(i+3))); err != nil {
			t.Fatal(err)
		}
	}
	// A non-socket fd must be ignored.
	_ = os.Symlink("/dev/null", filepath.Join(dir, "fd", "0"))
}

func fixtureRoot(t *testing.T) string {
	root := t.TempDir()
	write(t, filepath.Join(root, "net/tcp"), header+
		row("00000000:0016", "00000000:0000", "0A", 101)+ // 0.0.0.0:22 listening
		row("0100007F:1F90", "00000000:0000", "0A", 102)+ // 127.0.0.1:8080
		row("0100007F:1F90", "0100007F:D000", "01", 103)+ // established: ignored
		row("0100007F:0050", "00000000:0000", "0A", 999)+ // no owner found
		"garbage line\n"+
		row("0100007F:ZZZZ", "00000000:0000", "0A", 104))
	write(t, filepath.Join(root, "net/tcp6"), header+
		row("00000000000000000000000001000000:01BB", zero6, "0A", 105)+
		row("0000000000000000FFFF00000100007F:01BC", zero6, "0A", 106))
	write(t, filepath.Join(root, "net/udp"), header+
		row("00000000:0035", "00000000:0000", "07", 107)+ // bound
		row("0100007F:0044", "0100007F:0035", "01", 108)) // connected: ignored
	// udp6 is absent, as when IPv6 is off.

	addProc(t, root, 20, "sshd", "0::/system.slice/ssh.service\n", 101)
	addProc(t, root, 10, "app", "12:memory:/x\n1:name=systemd:/system.slice/app.service/sub\n0::/system.slice/app.service/sub\n", 102, 101)
	addProc(t, root, 30, "dockerd-child", "0::/system.slice/docker-"+dockID+".scope\n", 105)
	addProc(t, root, 40, "cg", "12:cpu:/docker/"+dockID2+"\n", 106)
	addProc(t, root, 50, "resolved", "0::/system.slice/systemd-resolved.service\n", 107)
	write(t, filepath.Join(root, "net/not-a-pid"), "")
	return root
}

func TestListening(t *testing.T) {
	got, err := Proc{Root: fixtureRoot(t)}.Listening(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	type k struct {
		proto, addr string
		port        uint16
	}
	by := map[k]Listener{}
	for _, l := range got {
		by[k{l.Protocol, l.Address, l.Port}] = l
	}
	if len(got) != 6 {
		t.Fatalf("got %d listeners: %+v", len(got), got)
	}
	// Order: protocol, then port.
	if got[0].Port != 22 || got[1].Port != 80 || got[2].Port != 8080 || got[len(got)-1].Protocol != "udp" {
		t.Errorf("order: %+v", got)
	}
	// Socket 101 is held by pids 10 and 20: the lowest wins.
	if l := by[k{"tcp", "0.0.0.0", 22}]; l.PID != 10 || l.Program != "app" || l.Unit != "app.service" {
		t.Errorf("ssh: %+v", l)
	}
	if l := by[k{"tcp", "127.0.0.1", 8080}]; l.PID != 10 || l.Unit != "app.service" || strings.Join(l.Units, ",") != "system.slice,app.service" {
		t.Errorf("app: %+v", l)
	}
	if l := by[k{"tcp", "127.0.0.1", 80}]; l.PID != 0 || l.Program != "" || l.Units == nil {
		t.Errorf("unowned: %+v", l)
	}
	if l := by[k{"tcp6", "::1", 443}]; l.PID != 30 || l.ContainerID != dockID {
		t.Errorf("tcp6: %+v", l)
	}
	if l := by[k{"udp", "0.0.0.0", 53}]; l.PID != 50 || l.Unit != "systemd-resolved.service" {
		t.Errorf("udp: %+v", l)
	}
}

func TestListeningIPv6AndContainers(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "net/tcp"), header)
	write(t, filepath.Join(root, "net/tcp6"), header+
		row("00000000000000000000000001000000:0050", zero6, "0A", 1)+
		row("0000000000000000FFFF00000100007F:0051", zero6, "0A", 2)+
		row("00000000000000000000000000000000:0052", zero6, "0A", 3))
	write(t, filepath.Join(root, "net/udp"), header)
	addProc(t, root, 30, "dockerd-child", "0::/system.slice/docker-"+dockID+".scope\n", 1)
	addProc(t, root, 40, "cg", "12:cpu:/docker/"+dockID2+"\n11:name=systemd:/docker/"+dockID2+"\n", 2)
	addProc(t, root, 50, "x", "0::/system.slice/a.service/b.scope\n", 3)
	got, err := Proc{Root: root}.Listening(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("%+v", got)
	}
	if l := got[0]; l.Address != "::1" || l.Port != 0x50 || l.ContainerID != dockID || l.Unit != "" || len(l.Units) != 1 || l.Units[0] != "system.slice" {
		t.Errorf("docker scope: %+v", l)
	}
	if l := got[1]; l.Address != "::ffff:127.0.0.1" || l.ContainerID != dockID2 || l.Unit != "" {
		t.Errorf("cgroupfs docker: %+v", l)
	}
	if l := got[2]; l.Address != "::" || l.Unit != "b.scope" || strings.Join(l.Units, ",") != "system.slice,a.service,b.scope" {
		t.Errorf("nested: %+v", l)
	}
}

func TestParseCgroup(t *testing.T) {
	cases := []struct {
		in, unit, cid string
		units         string
	}{
		{"0::/system.slice/nginx.service\n", "nginx.service", "", "system.slice,nginx.service"},
		{"0::/\n", "", "", ""},
		{"0::/user.slice/user-1000.slice/user@1000.service/app.slice/x.service\n", "x.service", "", "user.slice,user-1000.slice,user@1000.service,app.slice,x.service"},
		{"0::/system.slice/bad name.service\n", "", "", "system.slice"},
		{"0::/system.slice/../etc.service\n", "etc.service", "", "system.slice,etc.service"},
		{"garbage\n0::\n", "", "", ""},
		{"0::/docker/" + dockID + "\n", "", dockID, ""},
		{"0::/foo/" + dockID + "\n", "", "", ""},
		{"0::/docker/" + dockID[:63] + "\n", "", "", ""},
		{"0::/system.slice/docker-" + dockID + ".scope\n", "", dockID, "system.slice"},
	}
	for _, c := range cases {
		units, unit, cid := parseCgroup(c.in)
		if unit != c.unit || cid != c.cid || strings.Join(units, ",") != c.units || units == nil {
			t.Errorf("%q: unit=%q cid=%q units=%v", c.in, unit, cid, units)
		}
	}
}

func TestProgramIsSanitized(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "net/tcp"), header+row("0100007F:0050", "00000000:0000", "0A", 1))
	addProc(t, root, 5, "ev\x1b[31mil\u202e"+strings.Repeat("x", 200), "0::/a.service\n", 1)
	got, err := Proc{Root: root}.Listening(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatal(err, got)
	}
	if strings.ContainsAny(got[0].Program, "\x1b\u202e") || len(got[0].Program) > maxProgramBytes || !strings.HasPrefix(got[0].Program, "evil") {
		t.Errorf("program %q", got[0].Program)
	}
}

func TestMissingProcIsUnsupported(t *testing.T) {
	_, err := Proc{Root: filepath.Join(t.TempDir(), "nope")}.Listening(context.Background())
	if !errors.Is(err, host.ErrUnsupported) {
		t.Errorf("err = %v", err)
	}
	if runtime.GOOS != "linux" {
		if _, err := (Proc{}).Listening(context.Background()); !errors.Is(err, host.ErrUnsupported) {
			t.Errorf("default root: %v", err)
		}
	}
}

func TestCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (Proc{Root: fixtureRoot(t)}).Listening(ctx); err == nil {
		t.Error("canceled context accepted")
	}
}

func TestParseEndpoint(t *testing.T) {
	for _, bad := range []string{"", "0100007F", "0100007F:80", "0100007F:ZZZZ", "01007F:0050", "0100007G:0050", "0100007F0050"} {
		if _, _, ok := parseEndpoint(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if a, p, ok := parseEndpoint("0100007F:0016"); !ok || a.String() != "127.0.0.1" || p != 22 {
		t.Errorf("%v %v %v", a, p, ok)
	}
}
