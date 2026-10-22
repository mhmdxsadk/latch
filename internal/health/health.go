// Package health runs HTTP and DNS checks against operator-configured targets.
//
// The agent never supplies a URL or hostname: it names a target, and policy
// returns the configured value. Even so, a configured hostname can resolve to
// an internal address (by mistake or through DNS rebinding), so HTTP checks:
//
//   - resolve the host once, refuse the probe if any address is in a blocked
//     range, and dial only the addresses that were checked (no second lookup);
//   - never follow redirects and never use a proxy from the environment;
//   - bound time, header size and the amount of body read (the body itself is
//     never returned).
//
// Loopback, private, link-local and similar ranges are blocked unless the
// target sets allow_private. Cloud metadata endpoints, unspecified, multicast
// and reserved addresses are blocked unconditionally.
//
// Probe failures are results, not errors, and are reported as a fixed
// vocabulary so no network error text (which can carry internal detail)
// reaches the agent.
package health

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"syscall"
	"time"

	"github.com/mhmdxsadk/latch/internal/config"
)

// Failure is a fixed, agent-safe description of why a check did not pass.
type Failure string

const (
	FailureNone              Failure = ""
	FailureUnexpectedStatus  Failure = "unexpected_status"
	FailureTimeout           Failure = "timeout"
	FailureNotFound          Failure = "not_found"
	FailureDNS               Failure = "dns_error"
	FailureBlockedAddress    Failure = "blocked_address"
	FailureConnectionRefused Failure = "connection_refused"
	FailureConnection        Failure = "connection_error"
	FailureTLS               Failure = "tls_error"
)

const (
	maxHeaderBytes = 64 << 10
	maxBodyBytes   = 64 << 10
	userAgent      = "latch-health/1"
)

// HTTPResult is the outcome of an HTTP check.
type HTTPResult struct {
	OK         bool    `json:"ok"`
	StatusCode int     `json:"status_code,omitempty"`
	Failure    Failure `json:"failure,omitempty"`
	DurationMS int64   `json:"duration_ms"`
	// TLSNotAfter is the leaf certificate's expiry (RFC3339) for https targets.
	TLSNotAfter string `json:"tls_not_after,omitempty"`
}

// DNSResult is the outcome of a DNS check.
type DNSResult struct {
	OK         bool     `json:"ok"`
	Addresses  []string `json:"addresses"`
	Failure    Failure  `json:"failure,omitempty"`
	DurationMS int64    `json:"duration_ms"`
}

// Checker is the interface the MCP layer uses; tests substitute a fake.
// Targets have already been resolved by policy.
type Checker interface {
	HTTP(ctx context.Context, t config.HTTPTarget) (HTTPResult, error)
	DNS(ctx context.Context, t config.DNSTarget) (DNSResult, error)
}

// Resolver looks up the addresses of a host.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Prober is the real Checker. The zero value uses the system resolver and
// dialer; the fields exist for tests.
type Prober struct {
	Resolver Resolver
	// Dial connects to an already-checked address. Defaults to net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// RootCAs overrides the system roots (tests only).
	RootCAs *x509.CertPool
}

var _ Checker = Prober{}

var errBlocked = errors.New("address blocked by policy")

func (p Prober) resolver() Resolver {
	if p.Resolver != nil {
		return p.Resolver
	}
	return net.DefaultResolver
}

func (p Prober) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if p.Dial != nil {
		return p.Dial(ctx, network, addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// HTTP performs a GET against the target URL.
func (p Prober) HTTP(ctx context.Context, t config.HTTPTarget) (HTTPResult, error) {
	u, err := url.Parse(t.URL)
	if err != nil {
		return HTTPResult{}, errors.New("invalid target URL")
	}
	timeout := t.Timeout
	if timeout <= 0 || timeout > config.MaxCheckTimeout {
		timeout = config.DefaultCheckTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	res := p.probeHTTP(ctx, u, t)
	res.DurationMS = time.Since(start).Milliseconds()
	return res, nil
}

func (p Prober) probeHTTP(ctx context.Context, u *url.URL, t config.HTTPTarget) HTTPResult {
	addrs, err := p.resolve(ctx, u.Hostname())
	if err != nil {
		return HTTPResult{Failure: classifyDNS(err)}
	}
	for _, a := range addrs {
		if !Allowed(a, t.AllowPrivate) {
			return HTTPResult{Failure: FailureBlockedAddress}
		}
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}

	tr := &http.Transport{
		Proxy: nil,
		// Ignore the address the transport asks for and dial only the
		// addresses checked above, so a second DNS answer cannot redirect us.
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			last := errBlocked
			for _, a := range addrs {
				if !Allowed(a, t.AllowPrivate) {
					continue
				}
				c, err := p.dial(ctx, network, net.JoinHostPort(a.String(), port))
				if err == nil {
					return c, nil
				}
				last = err
			}
			return nil, last
		},
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: p.RootCAs},
		TLSHandshakeTimeout:    t.Timeout,
		ResponseHeaderTimeout:  t.Timeout,
		MaxResponseHeaderBytes: maxHeaderBytes,
		DisableKeepAlives:      true,
		DisableCompression:     true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return HTTPResult{Failure: FailureConnection}
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return HTTPResult{Failure: classifyConn(err)}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))

	res := HTTPResult{StatusCode: resp.StatusCode, OK: resp.StatusCode == t.ExpectStatus}
	if !res.OK {
		res.Failure = FailureUnexpectedStatus
	}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		res.TLSNotAfter = resp.TLS.PeerCertificates[0].NotAfter.UTC().Format(time.RFC3339)
	}
	return res
}

// resolve returns the addresses for host. IP literals are used as-is.
func (p Prober) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{a}, nil
	}
	addrs, err := p.resolver().LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}
	}
	return addrs, nil
}

// DNS resolves the target host and reports its addresses.
func (p Prober) DNS(ctx context.Context, t config.DNSTarget) (DNSResult, error) {
	ctx, cancel := context.WithTimeout(ctx, config.DefaultCheckTimeout)
	defer cancel()
	start := time.Now()
	addrs, err := p.resolver().LookupNetIP(ctx, "ip", t.Host)
	res := DNSResult{Addresses: []string{}, DurationMS: time.Since(start).Milliseconds()}
	if err == nil && len(addrs) == 0 {
		err = &net.DNSError{Err: "no addresses", Name: t.Host, IsNotFound: true}
	}
	if err != nil {
		res.Failure = classifyDNS(err)
		return res, nil
	}
	for _, a := range addrs {
		res.Addresses = append(res.Addresses, a.Unmap().String())
	}
	slices.Sort(res.Addresses)
	res.Addresses = slices.Compact(res.Addresses)
	res.OK = true
	return res, nil
}

func classifyDNS(err error) Failure {
	var de *net.DNSError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return FailureTimeout
	case errors.As(err, &de) && de.IsTimeout:
		return FailureTimeout
	case errors.As(err, &de) && de.IsNotFound:
		return FailureNotFound
	default:
		return FailureDNS
	}
}

func classifyConn(err error) Failure {
	var (
		ne         net.Error
		certErr    *tls.CertificateVerificationError
		recErr     tls.RecordHeaderError
		alertErr   tls.AlertError
		unknownErr x509.UnknownAuthorityError
		hostErr    x509.HostnameError
		invalidErr x509.CertificateInvalidError
	)
	switch {
	case errors.Is(err, errBlocked):
		return FailureBlockedAddress
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return FailureTimeout
	case errors.Is(err, syscall.ECONNREFUSED):
		return FailureConnectionRefused
	case errors.As(err, &certErr), errors.As(err, &recErr), errors.As(err, &alertErr),
		errors.As(err, &unknownErr), errors.As(err, &hostErr), errors.As(err, &invalidErr):
		return FailureTLS
	default:
		return FailureConnection
	}
}
