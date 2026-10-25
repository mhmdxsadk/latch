// Package netcheck runs network diagnostics for the agent: ping, DNS
// lookups, TCP connect checks, traceroute and HTTP GET.
//
// Any address may be checked, including this host and the LAN, except:
//
//   - addresses of excluded containers;
//   - ports published by excluded containers, when the target is one of this
//     host's own addresses (loopback, unspecified, or any interface address);
//   - hostnames and IP addresses listed in network.exclude.
//
// Names are resolved here, every resolved address is checked, and the check
// runs again on the socket address actually connected to (Dialer.Control),
// so a second DNS answer cannot bypass it. Programs (ping, traceroute) are
// given the checked IP literal, never the name. If the excluded containers
// are unknown, every check is refused.
package netcheck

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/mhmdxsadk/latch/internal/excluded"
	"github.com/mhmdxsadk/latch/internal/policy"
)

var (
	// ErrNotAllowed means the target, or an address it resolves to, is
	// blocked. It is the only message a blocked target gets.
	ErrNotAllowed = errors.New("target is not allowed")
	// ErrInvalid means the host, name, URL or a bound is malformed.
	ErrInvalid = errors.New("invalid target")
	// ErrUnknown means the excluded containers could not be determined, so
	// nothing is checked (fail closed).
	ErrUnknown = errors.New("excluded containers unknown")
	// ErrUnsupported means this host cannot run the check (no program, no
	// permission for ICMP).
	ErrUnsupported = errors.New("not supported on this host")
)

// Checker is the interface the MCP layer uses; tests substitute a fake.
// Hosts and names must already have passed policy (Policy.NetworkHost or
// Policy.DNSQueryName); HTTPGet takes the agent's URL and checks it itself.
type Checker interface {
	Ping(ctx context.Context, host string, count int) (PingResult, error)
	DNS(ctx context.Context, name, recordType string) (DNSResult, error)
	TCP(ctx context.Context, host string, port int) (TCPResult, error)
	Traceroute(ctx context.Context, host string, maxHops int) (TraceResult, error)
	HTTPGet(ctx context.Context, rawURL string, insecureTLS bool) (HTTPResult, error)
}

// Rules is the part of policy netcheck needs; *policy.Policy implements it.
type Rules interface {
	NetworkHost(host string) (string, error)
	NetworkAddrExcluded(a netip.Addr) bool
	// NetworkExcludedNames are the excluded hostnames; their addresses are
	// blocked too, so another name or the bare IP cannot reach them.
	NetworkExcludedNames() []string
}

// Excluded supplies the excluded containers; *excluded.Tracker implements it.
type Excluded interface {
	Get(ctx context.Context) (excluded.Snapshot, error)
}

// Resolver is the system resolver; *net.Resolver implements it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupNS(ctx context.Context, name string) ([]*net.NS, error)
	LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
	LookupAddr(ctx context.Context, addr string) ([]string, error)
}

// Failure is a fixed, agent-safe description of why a check did not succeed.
type Failure string

const (
	FailureNone        Failure = ""
	FailureTimeout     Failure = "timeout"
	FailureRefused     Failure = "refused"
	FailureUnreachable Failure = "unreachable"
	FailureNotFound    Failure = "dns_not_found"
	FailureDNS         Failure = "dns_error"
	FailureConnection  Failure = "connection_error"
	FailureTLS         Failure = "tls_error"
	FailureRedirects   Failure = "too_many_redirects"
	FailureBadRedirect Failure = "invalid_redirect"
	FailureProtocol    Failure = "protocol_error"
)

const (
	// hostAddrTTL is how long this host's interface addresses are cached.
	hostAddrTTL = 5 * time.Second
	// excludedNameTTL is how long the addresses of excluded hostnames are
	// reused; on a failed lookup the last good answer is kept.
	excludedNameTTL     = time.Minute
	excludedNameTimeout = 2 * time.Second
)

// Prober is the real Checker.
type Prober struct {
	rules     Rules
	excluded  Excluded
	resolver  Resolver
	userAgent string

	// The fields below exist for tests.
	hostAddrs  func() ([]netip.Addr, error)
	run        runFunc
	pingPaths  []string
	tracePaths []string
	roots      *x509.CertPool

	mu      sync.Mutex
	cached  []netip.Addr
	cacheAt time.Time

	nameMu    sync.Mutex
	nameAddrs map[string][]netip.Addr // last good answer per excluded name
	nameAt    time.Time
}

var _ Checker = (*Prober)(nil)

// New returns a Prober using the system resolver. version goes into the
// HTTP User-Agent.
func New(rules Rules, ex Excluded, version string) *Prober {
	return &Prober{
		rules:      rules,
		excluded:   ex,
		resolver:   net.DefaultResolver,
		userAgent:  "latch/" + version,
		hostAddrs:  interfaceAddrs,
		run:        runProgram,
		pingPaths:  defaultPingPaths,
		tracePaths: defaultTracePaths,
	}
}

// interfaceAddrs returns every address on this host's interfaces.
func interfaceAddrs() ([]netip.Addr, error) {
	as, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range as {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if x, ok := netip.AddrFromSlice(ip); ok {
			out = append(out, x.Unmap().WithZone(""))
		}
	}
	return out, nil
}

// localAddrs returns this host's interface addresses, cached briefly. Errors
// are not cached.
func (p *Prober) localAddrs() ([]netip.Addr, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached != nil && time.Since(p.cacheAt) < hostAddrTTL {
		return p.cached, nil
	}
	addrs, err := p.hostAddrs()
	if err != nil {
		return nil, err
	}
	if addrs == nil {
		addrs = []netip.Addr{}
	}
	p.cached, p.cacheAt = addrs, time.Now()
	return addrs, nil
}

// guard holds what one call needs to decide whether an address may be
// reached. It is built once per call so every hop and dial sees the same
// snapshot.
type guard struct {
	p         *Prober
	snap      excluded.Snapshot
	local     []netip.Addr
	localOK   bool
	nameAddrs []netip.Addr // addresses of excluded hostnames
}

func (p *Prober) guard(ctx context.Context) (*guard, error) {
	snap, err := p.excluded.Get(ctx)
	if err != nil {
		return nil, ErrUnknown
	}
	g := &guard{p: p, snap: snap, nameAddrs: p.excludedNameAddrs(ctx)}
	// Interface addresses only matter for excluded published ports.
	if len(snap.Ports) > 0 {
		if local, err := p.localAddrs(); err == nil {
			g.local, g.localOK = local, true
		}
	}
	return g, nil
}

// excludedNameAddrs resolves the excluded hostnames, at most once per
// excludedNameTTL. A name that fails to resolve keeps its last good answer.
func (p *Prober) excludedNameAddrs(ctx context.Context) []netip.Addr {
	names := p.rules.NetworkExcludedNames()
	if len(names) == 0 {
		return nil
	}
	p.nameMu.Lock()
	defer p.nameMu.Unlock()
	if p.nameAddrs == nil {
		p.nameAddrs = map[string][]netip.Addr{}
	}
	if p.nameAt.IsZero() || time.Since(p.nameAt) >= excludedNameTTL {
		for _, n := range names {
			lctx, cancel := context.WithTimeout(ctx, excludedNameTimeout)
			as, err := p.resolver.LookupNetIP(lctx, "ip", n)
			cancel()
			if err != nil {
				continue
			}
			for i, a := range as {
				as[i] = a.Unmap().WithZone("")
			}
			p.nameAddrs[n] = as
		}
		p.nameAt = time.Now()
	}
	var out []netip.Addr
	for _, as := range p.nameAddrs {
		out = append(out, as...)
	}
	return out
}

// thisNetwork is 0.0.0.0/8; connecting to it reaches this host on Linux.
var thisNetwork = netip.MustParsePrefix("0.0.0.0/8")

var (
	ipv4Compat = netip.MustParsePrefix("::/96") // deprecated IPv4-compatible
	nat64      = []netip.Prefix{netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48")}
	sixToFour  = netip.MustParsePrefix("2002::/16")
	teredo     = netip.MustParsePrefix("2001::/32")
)

// embeddedIPv4 returns the IPv4 addresses that translation and tunnelling
// prefixes carry inside an IPv6 address. If this host translates them, they
// lead to the IPv4 address, so they get the same checks.
func embeddedIPv4(a netip.Addr) []netip.Addr {
	if !a.Is6() {
		return nil
	}
	b := a.As16()
	v4 := func(p []byte) netip.Addr { return netip.AddrFrom4([4]byte(p)) }
	switch {
	case nat64[0].Contains(a), nat64[1].Contains(a):
		return []netip.Addr{v4(b[12:16])}
	case ipv4Compat.Contains(a) && !a.IsLoopback() && !a.IsUnspecified():
		return []netip.Addr{v4(b[12:16])}
	case sixToFour.Contains(a):
		return []netip.Addr{v4(b[2:6])}
	case teredo.Contains(a):
		client := [4]byte{b[12] ^ 0xff, b[13] ^ 0xff, b[14] ^ 0xff, b[15] ^ 0xff}
		return []netip.Addr{v4(b[4:8]), netip.AddrFrom4(client)}
	}
	return nil
}

// allowed reports whether a may be reached on port (0 for no port, as for
// ping and traceroute). An IPv4 address embedded in an IPv6 one must be
// allowed too.
func (g *guard) allowed(a netip.Addr, port uint16) bool {
	a = a.Unmap().WithZone("")
	if !g.allowedOne(a, port) {
		return false
	}
	for _, v4 := range embeddedIPv4(a) {
		if !g.allowedOne(v4, port) {
			return false
		}
	}
	return true
}

// cloudMetadata are the addresses where cloud servers hand out their own
// credentials to any program that asks. They are always blocked: on a cloud
// server a plain GET there returns keys to the cloud account.
var cloudMetadata = []netip.Addr{
	netip.MustParseAddr("169.254.169.254"), // AWS, Google Cloud, Azure, Oracle, DigitalOcean, Hetzner and others
	netip.MustParseAddr("fd00:ec2::254"),   // AWS over IPv6
	netip.MustParseAddr("169.254.170.2"),   // AWS ECS task credentials
	netip.MustParseAddr("100.100.100.200"), // Alibaba Cloud
	netip.MustParseAddr("168.63.129.16"),   // Azure WireServer
}

func (g *guard) allowedOne(a netip.Addr, port uint16) bool {
	if !a.IsValid() || slices.Contains(cloudMetadata, a) || g.snap.HasAddr(a) || g.p.rules.NetworkAddrExcluded(a) || slices.Contains(g.nameAddrs, a) {
		return false
	}
	if port == 0 || !g.snap.HasPort(port) {
		return true
	}
	if a.IsLoopback() || a.IsUnspecified() || thisNetwork.Contains(a) {
		return false
	}
	if !g.localOK {
		// Cannot tell whether a is this host: refuse the excluded port.
		return false
	}
	return !slices.Contains(g.local, a)
}

// resolve returns the addresses of host (an IP literal or a name that passed
// policy), unmapped and deduplicated, and refuses if any of them is blocked.
func (g *guard) resolve(ctx context.Context, host string, port uint16) ([]netip.Addr, Failure, error) {
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		got, err := g.p.resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, classifyDNS(err), nil
		}
		addrs = got
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		a = a.Unmap().WithZone("")
		if !g.allowed(a, port) {
			return nil, FailureNone, ErrNotAllowed
		}
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil, FailureNotFound, nil
	}
	return out, FailureNone, nil
}

// control checks the socket address actually being connected to.
func (g *guard) control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil || !g.allowed(ap.Addr(), ap.Port()) {
		return ErrNotAllowed
	}
	return nil
}

// dial connects to a checked address, checking it again at connect time.
func (g *guard) dial(ctx context.Context, a netip.Addr, port uint16, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout, Control: g.control}
	return d.DialContext(ctx, "tcp", netip.AddrPortFrom(a, port).String())
}

func isNotAllowed(err error) bool { return errors.Is(err, ErrNotAllowed) }

func classifyDNS(err error) Failure {
	var de *net.DNSError
	var ae *net.AddrError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return FailureTimeout
	case errors.As(err, &de) && de.IsTimeout:
		return FailureTimeout
	case errors.As(err, &de) && de.IsNotFound, errors.As(err, &ae):
		return FailureNotFound
	default:
		return FailureDNS
	}
}

func classifyConn(err error) Failure {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return FailureTimeout
	case errors.Is(err, syscall.ECONNREFUSED):
		return FailureRefused
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return FailureUnreachable
	default:
		return FailureConnection
	}
}

// PolicyError maps an error from Policy.NetworkHost or Policy.DNSQueryName to
// ErrNotAllowed or ErrInvalid, so every blocked target reads the same.
func PolicyError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, policy.ErrNotAllowed):
		return ErrNotAllowed
	default:
		return ErrInvalid
	}
}
