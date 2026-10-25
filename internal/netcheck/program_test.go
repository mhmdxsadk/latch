package netcheck

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const iputilsOutput = `PING 192.0.2.50 (192.0.2.50) 56(84) bytes of data.

--- 192.0.2.50 ping statistics ---
3 packets transmitted, 2 received, 33.3333% packet loss, time 2003ms
rtt min/avg/max/mdev = 9.955/10.338/10.682/0.298 ms
`

func TestPingRunsWithCheckedLiteral(t *testing.T) {
	e := newEnv(t)
	e.name("box.test", addrs("192.0.2.50", "2001:db8::50"))
	e.run.stdout = iputilsOutput
	r, err := e.p.Ping(context.Background(), "box.test", 3)
	if err != nil {
		t.Fatal(err)
	}
	want := PingResult{Address: "192.0.2.50", Sent: 3, Received: 2, LossPercent: 33.3, MinMS: 9.955, AvgMS: 10.338, MaxMS: 10.682}
	if r != want {
		t.Errorf("got %+v\nwant %+v", r, want)
	}
	if len(e.run.calls) != 1 {
		t.Fatalf("runs: %v", e.run.calls)
	}
	got := e.run.calls[0]
	wantArgs := []string{filepath.Join(e.dir, "ping"), "-4", "-n", "-q", "-c", "3", "-W", "2", "-w", "5", "--", "192.0.2.50"}
	if !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("args %q\nwant %q", got, wantArgs)
	}
	if slices.Contains(got, "box.test") {
		t.Error("the hostname must never reach the program")
	}

	e.name("v6.test", addrs("2001:db8::50"))
	if _, err := e.p.Ping(context.Background(), "v6.test", 1); err != nil {
		t.Fatal(err)
	}
	if got := e.run.calls[1]; got[1] != "-6" || got[len(got)-1] != "2001:db8::50" || got[5] != "1" {
		t.Errorf("IPv6 args %q", got)
	}
}

func TestPingLimitsAndRefusals(t *testing.T) {
	e := newEnv(t)
	e.name("container.test", addrs("172.18.0.5"))
	ctx := context.Background()
	for _, n := range []int{0, -1, 6, 1000} {
		if _, err := e.p.Ping(ctx, "127.0.0.1", n); !errors.Is(err, ErrInvalid) {
			t.Errorf("count %d: %v", n, err)
		}
	}
	for _, h := range []string{"172.18.0.5", "fd00:dead::5", "container.test", "10.9.9.9"} {
		if _, err := e.p.Ping(ctx, h, 3); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("%s: %v", h, err)
		}
	}
	if len(e.run.calls) != 0 {
		t.Errorf("ping ran for a refused request: %v", e.run.calls)
	}
	r, err := e.p.Ping(ctx, "nxdomain.test", 3)
	if err != nil || r.Failure != FailureNotFound || len(e.run.calls) != 0 {
		t.Errorf("unknown name: %+v %v", r, err)
	}
}

func TestPingUnsupported(t *testing.T) {
	e := newEnv(t)
	e.p.pingPaths = []string{filepath.Join(e.dir, "nope")}
	if _, err := e.p.Ping(context.Background(), "127.0.0.1", 3); !errors.Is(err, ErrUnsupported) {
		t.Errorf("no program: %v", err)
	}
	e = newEnv(t)
	e.run.stderr = "ping: socktype: SOCK_RAW\nping: socket: Operation not permitted\n"
	e.run.err = errors.New("exit status 2")
	if _, err := e.p.Ping(context.Background(), "127.0.0.1", 3); !errors.Is(err, ErrUnsupported) {
		t.Errorf("no permission: %v", err)
	}
	e.run.stderr = "ping: connect: Network is unreachable\n"
	r, err := e.p.Ping(context.Background(), "127.0.0.1", 3)
	if err != nil || r.Failure != FailureUnreachable {
		t.Errorf("unreachable: %+v %v", r, err)
	}
	// stderr text never reaches the agent, even in an error.
	e.run.stderr = "ping: something odd /etc/secret"
	_, err = e.p.Ping(context.Background(), "127.0.0.1", 3)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("odd failure: %v", err)
	}
}

func TestParsePing(t *testing.T) {
	var r PingResult
	if !parsePing([]byte("3 packets transmitted, 0 received, +3 errors, 100% packet loss, time 2040ms\n"), &r) ||
		r.Sent != 3 || r.Received != 0 || r.LossPercent != 100 || r.AvgMS != 0 {
		t.Errorf("all lost: %+v", r)
	}
	for _, bad := range []string{"", "garbage", "3 packets transmitted, 9 received", "99 packets transmitted, 1 received"} {
		var r PingResult
		if parsePing([]byte(bad), &r) {
			t.Errorf("parsed %q as %+v", bad, r)
		}
	}
}

const tracerouteOutput = "traceroute to 192.0.2.50 (192.0.2.50), 20 hops max, 60 byte packets\n" +
	" 1  192.168.1.1  0.512 ms\n" +
	" 2  *\n" +
	" 3  10.0.0.1  5.123 ms !H\n" +
	" 4  \x1b[31mevil  1 ms\n" +
	" 5  192.0.2.50  9.9 ms\n" +
	"25  203.0.113.1  1 ms\n"

const tracepathOutput = ` 1?: [LOCALHOST]                      pmtu 1500
 1:  192.168.1.1                                           0.512ms
 1:  192.168.1.1                                           0.400ms
 2:  no reply
 3:  2001:db8::1                                           5.1ms asymm  4
     Resume: pmtu 1500 hops 3 back 3
`

func TestTraceroute(t *testing.T) {
	e := newEnv(t)
	e.name("box.test", addrs("192.0.2.50"))
	e.run.stdout = tracerouteOutput
	r, err := e.p.Traceroute(context.Background(), "box.test", 20)
	if err != nil {
		t.Fatal(err)
	}
	want := []Hop{{1, "192.168.1.1", 0.512}, {2, "", 0}, {3, "10.0.0.1", 5.123}, {5, "192.0.2.50", 9.9}}
	if !reflect.DeepEqual(r.Hops, want) || r.Address != "192.0.2.50" || r.Program != "traceroute" {
		t.Errorf("got %+v", r)
	}
	wantArgs := []string{filepath.Join(e.dir, "traceroute"), "-4", "-n", "-q", "1", "-w", "2", "-m", "20", "192.0.2.50"}
	if !reflect.DeepEqual(e.run.calls[0], wantArgs) {
		t.Errorf("args %q", e.run.calls[0])
	}
}

func TestTracepath(t *testing.T) {
	e := newEnv(t)
	e.p.tracePaths = []string{filepath.Join(e.dir, "missing"), fakeProgram(t, e.dir, "tracepath")}
	e.name("box.test", addrs("2001:db8::50"))
	e.run.stdout = tracepathOutput
	r, err := e.p.Traceroute(context.Background(), "box.test", 30)
	if err != nil {
		t.Fatal(err)
	}
	want := []Hop{{1, "192.168.1.1", 0.512}, {2, "", 0}, {3, "2001:db8::1", 5.1}}
	if !reflect.DeepEqual(r.Hops, want) || r.Program != "tracepath" {
		t.Errorf("got %+v", r)
	}
	wantArgs := []string{filepath.Join(e.dir, "tracepath"), "-6", "-n", "-m", "30", "2001:db8::50"}
	if !reflect.DeepEqual(e.run.calls[0], wantArgs) {
		t.Errorf("args %q", e.run.calls[0])
	}
}

func TestTracerouteLimitsAndRefusals(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, n := range []int{0, -1, 31, 255} {
		if _, err := e.p.Traceroute(ctx, "127.0.0.1", n); !errors.Is(err, ErrInvalid) {
			t.Errorf("max_hops %d: %v", n, err)
		}
	}
	if _, err := e.p.Traceroute(ctx, "172.18.0.5", 20); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("container: %v", err)
	}
	if len(e.run.calls) != 0 {
		t.Errorf("ran: %v", e.run.calls)
	}
	e.p.tracePaths = nil
	if _, err := e.p.Traceroute(ctx, "127.0.0.1", 20); !errors.Is(err, ErrUnsupported) {
		t.Errorf("no program: %v", err)
	}
	// Hops beyond the limit are dropped even if a program printed them.
	if hops := parseHops([]byte(tracerouteOutput), tracerouteLine, 2); len(hops) != 2 {
		t.Errorf("hops %+v", hops)
	}
}

func TestCappedWriter(t *testing.T) {
	c := &capped{max: 4}
	for range 3 {
		if n, err := c.Write([]byte("abc")); n != 3 || err != nil {
			t.Fatal(n, err)
		}
	}
	if c.buf.String() != "abca" {
		t.Errorf("got %q", c.buf.String())
	}
}
