package netcheck

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhmdxsadk/latch/internal/config"
	"github.com/mhmdxsadk/latch/internal/excluded"
	"github.com/mhmdxsadk/latch/internal/policy"
)

// Addresses used by the tests. The excluded container addresses are never
// dialed: the point is that they are refused first.
var (
	containerV4 = netip.MustParseAddr("172.18.0.5")
	containerV6 = netip.MustParseAddr("fd00:dead::5")
	hostIface   = netip.MustParseAddr("192.0.2.10")
	hostIface6  = netip.MustParseAddr("fe80::1")
	remote      = netip.MustParseAddr("198.51.100.7")
)

const excludedPort = 5432

type fakeExcluded struct {
	snap excluded.Snapshot
	err  error
}

func (f *fakeExcluded) Get(context.Context) (excluded.Snapshot, error) { return f.snap, f.err }

// fakeResolver answers from tables. ip holds successive answers per name;
// the last one repeats, so a test can make a name change between lookups.
type fakeResolver struct {
	mu    sync.Mutex
	ip    map[string][][]netip.Addr
	seen  map[string]int
	txt   []string
	ptr   []string
	err   error
	calls atomic.Int32
}

func (f *fakeResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	seq, ok := f.ip[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	if f.seen == nil {
		f.seen = map[string]int{}
	}
	n := min(f.seen[host], len(seq)-1)
	f.seen[host]++
	var out []netip.Addr
	for _, a := range seq[n] {
		if network == "ip" || (network == "ip4") == a.Unmap().Is4() {
			out = append(out, a)
		}
	}
	return out, nil
}
func (f *fakeResolver) LookupCNAME(context.Context, string) (string, error) {
	f.calls.Add(1)
	return "target.example.", f.err
}
func (f *fakeResolver) LookupMX(context.Context, string) ([]*net.MX, error) {
	f.calls.Add(1)
	return []*net.MX{{Host: "mx.example.", Pref: 10}}, f.err
}
func (f *fakeResolver) LookupTXT(context.Context, string) ([]string, error) {
	f.calls.Add(1)
	return f.txt, f.err
}
func (f *fakeResolver) LookupNS(context.Context, string) ([]*net.NS, error) {
	f.calls.Add(1)
	return []*net.NS{{Host: "ns1.example."}}, f.err
}
func (f *fakeResolver) LookupSRV(context.Context, string, string, string) (string, []*net.SRV, error) {
	f.calls.Add(1)
	return "", []*net.SRV{{Target: "sip.example.", Port: 5060, Priority: 1, Weight: 2}}, f.err
}
func (f *fakeResolver) LookupAddr(context.Context, string) ([]string, error) {
	f.calls.Add(1)
	return f.ptr, f.err
}

// fakeRun records program runs.
type fakeRun struct {
	mu     sync.Mutex
	calls  [][]string
	stdout string
	stderr string
	err    error
}

func (f *fakeRun) run(_ context.Context, path string, args []string) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{path}, args...))
	return []byte(f.stdout), []byte(f.stderr), f.err
}

type testEnv struct {
	p   *Prober
	res *fakeResolver
	ex  *fakeExcluded
	run *fakeRun
	dir string
}

func testPolicy(t *testing.T) *policy.Policy {
	t.Helper()
	c, err := config.Parse([]byte(`
server: {socket: /run/l.sock, socket_group: latch-agents}
network:
  exclude: [blocked.example, 10.9.9.9]
`))
	if err != nil {
		t.Fatal(err)
	}
	return policy.New(c)
}

// fakeProgram creates an executable file that is never run (run is faked)
// but satisfies findProgram.
func fakeProgram(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/false\n"), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	return p
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	e := &testEnv{
		res: &fakeResolver{ip: map[string][][]netip.Addr{}},
		ex: &fakeExcluded{snap: excluded.Snapshot{
			Addrs: []netip.Addr{containerV4, containerV6},
			Ports: []uint16{excludedPort},
		}},
		run: &fakeRun{},
		dir: dir,
	}
	e.p = New(testPolicy(t), e.ex, "test")
	e.p.resolver = e.res
	e.p.hostAddrs = func() ([]netip.Addr, error) { return []netip.Addr{hostIface, hostIface6}, nil }
	e.p.run = e.run.run
	e.p.pingPaths = []string{filepath.Join(dir, "missing"), fakeProgram(t, dir, "ping")}
	e.p.tracePaths = []string{fakeProgram(t, dir, "traceroute")}
	return e
}

func (e *testEnv) name(host string, answers ...[]netip.Addr) {
	e.res.ip[host] = answers
}

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}

func TestAllowedAddresses(t *testing.T) {
	e := newEnv(t)
	g, err := e.p.guard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		addr string
		port uint16
		want bool
	}{
		// Excluded containers, on any port or none.
		{"172.18.0.5", 0, false},
		{"172.18.0.5", 80, false},
		{"fd00:dead::5", 443, false},
		{"::ffff:172.18.0.5", 80, false},
		{"fd00:dead::5%eth0", 0, false},
		// network.exclude IP literal.
		{"10.9.9.9", 0, false},
		{"::ffff:10.9.9.9", 22, false},
		// Excluded published port on this host's own addresses.
		{"127.0.0.1", excludedPort, false},
		{"127.0.0.2", excludedPort, false},
		{"::1", excludedPort, false},
		{"::ffff:127.0.0.1", excludedPort, false},
		{"0.0.0.0", excludedPort, false},
		{"0.1.2.3", excludedPort, false},
		{"::", excludedPort, false},
		{"192.0.2.10", excludedPort, false},
		{"::ffff:192.0.2.10", excludedPort, false},
		{"fe80::1", excludedPort, false},
		{"fe80::1%eth0", excludedPort, false},
		// IPv6 forms that embed an excluded IPv4 address.
		{"64:ff9b::172.18.0.5", 80, false},
		{"64:ff9b:1::ac12:5", 80, false},
		{"::172.18.0.5", 80, false},
		{"2002:ac12:5::1", 80, false},
		{"2001:0:ac12:5::", 80, false},
		{"2001:0:0:0:0:0:53ed:fffa", 80, false}, // Teredo client 172.18.0.5
		{"64:ff9b::10.9.9.9", 0, false},
		{"64:ff9b::127.0.0.1", excludedPort, false},
		{"64:ff9b::198.51.100.7", 80, true},
		// Allowed: private, loopback, LAN, and the excluded port elsewhere.
		{"127.0.0.1", 80, true},
		{"::1", 22, true},
		{"192.0.2.10", 80, true},
		{"10.0.0.1", excludedPort, true},
		{"198.51.100.7", excludedPort, true},
		{"172.18.0.6", 80, true},
		{"127.0.0.1", 0, true},
	}
	for _, tt := range tests {
		if got := g.allowed(netip.MustParseAddr(tt.addr), tt.port); got != tt.want {
			t.Errorf("allowed(%s, %d) = %v, want %v", tt.addr, tt.port, got, tt.want)
		}
	}
}

func TestUnknownHostAddressesRefuseExcludedPortOnly(t *testing.T) {
	e := newEnv(t)
	e.p.hostAddrs = func() ([]netip.Addr, error) { return nil, errors.New("netlink refused") }
	g, err := e.p.guard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if g.allowed(remote, excludedPort) {
		t.Error("excluded port must be refused when this host's addresses are unknown")
	}
	if !g.allowed(remote, 80) {
		t.Error("other ports stay allowed")
	}
}

func TestHostAddressesAreCached(t *testing.T) {
	e := newEnv(t)
	var n atomic.Int32
	e.p.hostAddrs = func() ([]netip.Addr, error) {
		n.Add(1)
		return []netip.Addr{hostIface}, nil
	}
	for range 3 {
		if _, err := e.p.guard(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n.Load() != 1 {
		t.Errorf("interface addresses read %d times", n.Load())
	}
}

// If the excluded containers are unknown, every tool refuses before any
// lookup, program or connection.
func TestTrackerUnknownRefusesEverything(t *testing.T) {
	e := newEnv(t)
	e.ex.err = excluded.ErrUnknown
	e.name("example.com", addrs("198.51.100.7"))
	ctx := context.Background()
	errs := map[string]error{}
	_, errs["ping"] = e.p.Ping(ctx, "example.com", 3)
	_, errs["dns"] = e.p.DNS(ctx, "example.com", "A")
	_, errs["ptr"] = e.p.DNS(ctx, "198.51.100.7", "PTR")
	_, errs["tcp"] = e.p.TCP(ctx, "example.com", 443)
	_, errs["traceroute"] = e.p.Traceroute(ctx, "example.com", 20)
	_, errs["http"] = e.p.HTTPGet(ctx, "http://example.com/", false)
	for tool, err := range errs {
		if !errors.Is(err, ErrUnknown) {
			t.Errorf("%s: err = %v, want ErrUnknown", tool, err)
		}
	}
	if e.res.calls.Load() != 0 || len(e.run.calls) != 0 {
		t.Errorf("network activity with unknown exclusions: %d lookups, %d runs", e.res.calls.Load(), len(e.run.calls))
	}
}

func TestPolicyError(t *testing.T) {
	p := testPolicy(t)
	for _, h := range []string{"blocked.example", "BLOCKED.Example.", "10.9.9.9", "::ffff:10.9.9.9"} {
		_, err := p.NetworkHost(h)
		if !errors.Is(PolicyError(err), ErrNotAllowed) {
			t.Errorf("%q: %v", h, PolicyError(err))
		}
	}
	_, err := p.NetworkHost("bad host")
	if !errors.Is(PolicyError(err), ErrInvalid) {
		t.Errorf("invalid: %v", PolicyError(err))
	}
	if PolicyError(nil) != nil {
		t.Error("nil must stay nil")
	}
}

// listen returns a listener on 127.0.0.1 counting accepted connections.
func listen(t *testing.T) (port int, accepted *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	accepted = &atomic.Int32{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, accepted
}

func TestTCPOpenAndRefused(t *testing.T) {
	e := newEnv(t)
	port, accepted := listen(t)
	e.name("svc.test", addrs("127.0.0.1"))
	for _, host := range []string{"127.0.0.1", "svc.test"} {
		r, err := e.p.TCP(context.Background(), host, port)
		if err != nil || !r.Open || len(r.Attempts) != 1 || r.Attempts[0].Address != "127.0.0.1" || r.Attempts[0].State != StateOpen {
			t.Errorf("%s: %+v %v", host, r, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if accepted.Load() != 2 {
		t.Errorf("accepted %d", accepted.Load())
	}

	// A port nothing listens on.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	r, err := e.p.TCP(context.Background(), "127.0.0.1", closed)
	if err != nil || r.Open || r.Attempts[0].State != StateRefused {
		t.Errorf("closed port: %+v %v", r, err)
	}

	r, err = e.p.TCP(context.Background(), "nxdomain.test", 80)
	if err != nil || r.Failure != FailureNotFound || len(r.Attempts) != 0 {
		t.Errorf("unknown name: %+v %v", r, err)
	}
}

func TestTCPRefusesBlockedTargets(t *testing.T) {
	e := newEnv(t)
	port, accepted := listen(t)
	e.ex.snap.Ports = append(e.ex.snap.Ports, uint16(port))
	e.name("mixed.test", addrs("127.0.0.1", "172.18.0.5"))
	e.name("container.test", addrs("::ffff:172.18.0.5"))
	e.name("local.test", addrs("127.0.0.1"))
	for _, tt := range []struct {
		host string
		port int
	}{
		{"127.0.0.1", port},          // excluded published port on loopback
		{"0.0.0.0", port},            // unspecified reaches this host
		{"::1", port},                // IPv6 loopback
		{"192.0.2.10", port},         // host interface address
		{"local.test", port},         // a name for loopback
		{"172.18.0.5", 80},           // excluded container
		{"fd00:dead::5", 80},         // excluded container, IPv6
		{"container.test", 80},       // name resolving to a mapped container address
		{"mixed.test", 80},           // one of several answers is excluded
		{"10.9.9.9", 80},             // network.exclude literal
		{"::ffff:10.9.9.9", 80},      // the same, mapped
		{"::ffff:127.0.0.1", port},   // mapped loopback
		{"127.0.0.1", excludedPort},  // the other excluded port
		{"::ffff:192.0.2.10", port},  // mapped interface address
		{"fe80::1", excludedPort},    // IPv6 interface address
		{"mixed.test", excludedPort}, // both reasons
		{"container.test", excludedPort},
	} {
		_, err := e.p.TCP(context.Background(), tt.host, tt.port)
		if !errors.Is(err, ErrNotAllowed) {
			t.Errorf("TCP(%s, %d) = %v, want ErrNotAllowed", tt.host, tt.port, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if accepted.Load() != 0 {
		t.Errorf("a blocked target was connected to %d times", accepted.Load())
	}
	if _, err := e.p.TCP(context.Background(), "127.0.0.1", 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("port 0: %v", err)
	}
	if _, err := e.p.TCP(context.Background(), "127.0.0.1", 65536); !errors.Is(err, ErrInvalid) {
		t.Errorf("port 65536: %v", err)
	}
}

// The connect-time check stands on its own: even an address that was never
// resolved or checked is refused by the dialer itself, before any packet.
func TestDialTimeCheck(t *testing.T) {
	e := newEnv(t)
	port, accepted := listen(t)
	e.ex.snap.Addrs = append(e.ex.snap.Addrs, netip.MustParseAddr("127.0.0.1"))
	g, err := e.p.guard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"127.0.0.1", "::ffff:127.0.0.1"} {
		if _, err := g.dial(context.Background(), netip.MustParseAddr(a), uint16(port), time.Second); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("dial %s: %v", a, err)
		}
	}
	if err := g.control("tcp4", "not-an-address", nil); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("unparsable sockaddr: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if accepted.Load() != 0 {
		t.Errorf("connected %d times", accepted.Load())
	}
}

// DNS rebinding: a name that first resolves to an allowed address and then
// to an excluded one is refused on the second lookup.
func TestRebindingRefused(t *testing.T) {
	e := newEnv(t)
	port, accepted := listen(t)
	e.name("rebind.test", addrs("127.0.0.1"), addrs("172.18.0.5"))
	if r, err := e.p.TCP(context.Background(), "rebind.test", port); err != nil || !r.Open {
		t.Fatalf("first lookup: %+v %v", r, err)
	}
	if _, err := e.p.TCP(context.Background(), "rebind.test", port); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("second lookup: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if accepted.Load() != 1 {
		t.Errorf("accepted %d", accepted.Load())
	}
}

func TestDNS(t *testing.T) {
	e := newEnv(t)
	e.name("example.com", addrs("198.51.100.7", "2001:db8::7", "::ffff:198.51.100.8"))
	ctx := context.Background()

	r, err := e.p.DNS(ctx, "example.com", "A")
	if err != nil || strings.Join(r.Answers, ",") != "198.51.100.7,198.51.100.8" {
		t.Errorf("A: %+v %v", r, err)
	}
	r, err = e.p.DNS(ctx, "example.com", "AAAA")
	if err != nil || strings.Join(r.Answers, ",") != "2001:db8::7" {
		t.Errorf("AAAA: %+v %v", r, err)
	}
	for typ, want := range map[string]string{
		"CNAME": "target.example.", "MX": "10 mx.example.", "NS": "ns1.example.", "SRV": "1 2 5060 sip.example.",
	} {
		r, err := e.p.DNS(ctx, "example.com", typ)
		if err != nil || len(r.Answers) != 1 || r.Answers[0] != want {
			t.Errorf("%s: %+v %v", typ, r, err)
		}
	}
	r, err = e.p.DNS(ctx, "nxdomain.test", "A")
	if err != nil || r.Failure != FailureNotFound || r.Answers == nil {
		t.Errorf("not found: %+v %v", r, err)
	}
	for _, typ := range []string{"", "ANY", "AXFR", "a", "TXT "} {
		if _, err := e.p.DNS(ctx, "example.com", typ); !errors.Is(err, ErrInvalid) {
			t.Errorf("type %q: %v", typ, err)
		}
	}
}

func TestDNSAnswersAreSanitized(t *testing.T) {
	e := newEnv(t)
	e.res.txt = []string{
		"v=spf1 -all",
		"api_key=sk-abcdefghijklmnopqrstuvwxyz",
		"\x1b[31mred\x1b[0m\u202eevil\nsecond line",
		strings.Repeat("x", 2000),
	}
	r, err := e.p.DNS(context.Background(), "example.com", "TXT")
	if err != nil {
		t.Fatal(err)
	}
	if r.Answers[0] != "v=spf1 -all" {
		t.Errorf("plain TXT changed: %q", r.Answers[0])
	}
	if strings.Contains(r.Answers[1], "sk-abc") {
		t.Errorf("secret not redacted: %q", r.Answers[1])
	}
	if strings.ContainsAny(r.Answers[2], "\x1b\u202e\n") || r.Answers[2] != "redevilsecond line" {
		t.Errorf("not sanitized: %q", r.Answers[2])
	}
	if len(r.Answers[3]) != MaxAnswerBytes {
		t.Errorf("long answer is %d bytes", len(r.Answers[3]))
	}

	e.res.txt = make([]string, 100)
	r, _ = e.p.DNS(context.Background(), "example.com", "TXT")
	if len(r.Answers) != MaxDNSAnswers || !r.Truncated {
		t.Errorf("%d answers, truncated=%v", len(r.Answers), r.Truncated)
	}
}

func TestDNSPTR(t *testing.T) {
	e := newEnv(t)
	e.res.ptr = []string{"host.example."}
	ctx := context.Background()
	r, err := e.p.DNS(ctx, "198.51.100.7", "PTR")
	if err != nil || len(r.Answers) != 1 || r.Answers[0] != "host.example." {
		t.Errorf("PTR: %+v %v", r, err)
	}
	for _, a := range []string{"172.18.0.5", "::ffff:172.18.0.5", "fd00:dead::5", "10.9.9.9"} {
		if _, err := e.p.DNS(ctx, a, "PTR"); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("PTR %s: %v", a, err)
		}
	}
	if _, err := e.p.DNS(ctx, "example.com", "PTR"); !errors.Is(err, ErrInvalid) {
		t.Errorf("PTR of a name: %v", err)
	}
}

func TestDNSResolverErrorsAreCategories(t *testing.T) {
	e := newEnv(t)
	e.res.err = &net.DNSError{Err: "secret internal resolver 10.1.2.3 said no", Name: "x", IsTimeout: true}
	r, err := e.p.DNS(context.Background(), "example.com", "TXT")
	if err != nil || r.Failure != FailureTimeout {
		t.Errorf("%+v %v", r, err)
	}
	e.res.err = errors.New("boom")
	r, _ = e.p.DNS(context.Background(), "example.com", "MX")
	if r.Failure != FailureDNS {
		t.Errorf("%+v", r)
	}
}

// An excluded hostname blocks its addresses too, so another name for the
// same server or the bare IP cannot reach it. The answer survives a failed
// lookup.
func TestExcludedNameAddressesBlocked(t *testing.T) {
	e := newEnv(t)
	e.name("blocked.example", addrs("198.51.100.7"))
	e.name("alias.test", addrs("198.51.100.7"))
	for _, host := range []string{"alias.test", "198.51.100.7", "::ffff:198.51.100.7"} {
		if _, err := e.p.TCP(context.Background(), host, 80); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("TCP(%s) = %v, want ErrNotAllowed", host, err)
		}
	}
	e.res.mu.Lock()
	delete(e.res.ip, "blocked.example")
	e.res.mu.Unlock()
	e.p.nameAt = time.Now().Add(-2 * excludedNameTTL)
	if _, err := e.p.TCP(context.Background(), "198.51.100.7", 80); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("after a failed lookup: %v", err)
	}
}

// Cloud metadata addresses hand out the server's cloud credentials; no name,
// spelling or tool may reach them.
func TestCloudMetadataBlocked(t *testing.T) {
	e := newEnv(t)
	e.name("metadata.google.internal", addrs("169.254.169.254"))
	for _, host := range []string{
		"169.254.169.254", "::ffff:169.254.169.254", "64:ff9b::a9fe:a9fe", "metadata.google.internal",
		"fd00:ec2::254", "169.254.170.2", "100.100.100.200", "168.63.129.16",
	} {
		if _, err := e.p.TCP(context.Background(), host, 80); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("TCP(%s) = %v, want ErrNotAllowed", host, err)
		}
	}
	if _, err := e.p.HTTPGet(context.Background(), "http://169.254.169.254/latest/meta-data/", false); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("HTTPGet = %v", err)
	}
}
