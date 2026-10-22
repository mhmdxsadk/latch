package health

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhmdxsadk/latch/internal/config"
)

func TestAllowed(t *testing.T) {
	tests := []struct {
		addr         string
		public, priv bool // allowed without / with allow_private
	}{
		{"93.184.216.34", true, true},
		{"2606:2800:220:1:248:1893:25c8:1946", true, true},
		{"127.0.0.1", false, true},
		{"127.1.2.3", false, true},
		{"::1", false, true},
		{"10.0.0.1", false, true},
		{"172.16.5.4", false, true},
		{"192.168.1.1", false, true},
		{"fd12::1", false, true},
		{"169.254.1.1", false, true},
		{"fe80::1", false, true},
		{"fe80::1%eth0", false, true},
		{"100.64.0.1", false, true},
		{"198.18.0.1", false, true},
		{"::ffff:127.0.0.1", false, true},
		{"::ffff:10.0.0.1", false, true},
		{"64:ff9b::a00:1", false, true},
		{"2002:a00:1::", false, true},
		// Never allowed.
		{"169.254.169.254", false, false},
		{"::ffff:169.254.169.254", false, false},
		{"fd00:ec2::254", false, false},
		{"100.100.100.200", false, false},
		{"0.0.0.0", false, false},
		{"0.1.2.3", false, false},
		{"::", false, false},
		{"255.255.255.255", false, false},
		{"240.0.0.1", false, false},
		{"224.0.0.1", false, false},
		{"ff02::1", false, false},
	}
	for _, tt := range tests {
		a := netip.MustParseAddr(tt.addr)
		if got := Allowed(a, false); got != tt.public {
			t.Errorf("Allowed(%s, false) = %v, want %v", tt.addr, got, tt.public)
		}
		if got := Allowed(a, true); got != tt.priv {
			t.Errorf("Allowed(%s, true) = %v, want %v", tt.addr, got, tt.priv)
		}
	}
	if Allowed(netip.Addr{}, true) {
		t.Error("zero Addr must not be allowed")
	}
}

// fakeResolver returns fixed answers and counts lookups.
type fakeResolver struct {
	addrs []netip.Addr
	err   error
	calls atomic.Int32
}

func (f *fakeResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	f.calls.Add(1)
	return f.addrs, f.err
}

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}

// serverPort starts srv and returns its port.
func serverPort(t *testing.T, h http.Handler) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	return srv, port
}

func target(url string, allowPrivate bool) config.HTTPTarget {
	return config.HTTPTarget{Name: "t", URL: url, ExpectStatus: 200, Timeout: 2 * time.Second, AllowPrivate: allowPrivate}
}

func TestHTTPHappyPathAndStatus(t *testing.T) {
	var ua string
	_, port := serverPort(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.UserAgent()
		if r.URL.Path == "/down" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		w.Write([]byte(strings.Repeat("x", 1<<20))) // body is drained, never returned
	}))
	p := Prober{}
	res, err := p.HTTP(context.Background(), target("http://127.0.0.1:"+port+"/ok", true))
	if err != nil || !res.OK || res.StatusCode != 200 || res.Failure != "" {
		t.Fatalf("got %+v, %v", res, err)
	}
	if ua != userAgent {
		t.Errorf("user agent %q", ua)
	}
	res, _ = p.HTTP(context.Background(), target("http://127.0.0.1:"+port+"/down", true))
	if res.OK || res.StatusCode != 503 || res.Failure != FailureUnexpectedStatus {
		t.Errorf("got %+v", res)
	}
}

func TestHTTPBlocksPrivateWithoutOptIn(t *testing.T) {
	var hits atomic.Int32
	_, port := serverPort(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	for _, url := range []string{
		"http://127.0.0.1:" + port + "/",
		"http://[::1]:" + port + "/",
		"http://169.254.169.254/latest/meta-data/",
		"http://0.0.0.0:" + port + "/",
	} {
		res, err := Prober{}.HTTP(context.Background(), target(url, false))
		if err != nil || res.OK || res.Failure != FailureBlockedAddress {
			t.Errorf("%s: got %+v, %v", url, res, err)
		}
	}
	// Metadata stays blocked even with allow_private.
	res, _ := Prober{}.HTTP(context.Background(), target("http://169.254.169.254/", true))
	if res.Failure != FailureBlockedAddress {
		t.Errorf("metadata with allow_private: %+v", res)
	}
	if hits.Load() != 0 {
		t.Errorf("blocked targets were contacted %d times", hits.Load())
	}
}

// A hostname that resolves to any blocked address is refused outright, even
// if other answers are public (a classic rebinding setup).
func TestHTTPHostnameResolvingToPrivateIsBlocked(t *testing.T) {
	var dials atomic.Int32
	p := Prober{
		Resolver: &fakeResolver{addrs: addrs("93.184.216.34", "10.0.0.5")},
		Dial: func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("must not dial")
		},
	}
	res, _ := p.HTTP(context.Background(), target("http://example.test/", false))
	if res.Failure != FailureBlockedAddress || dials.Load() != 0 {
		t.Errorf("got %+v, dials=%d", res, dials.Load())
	}
}

// The resolved address is pinned: the transport dials exactly what was
// checked and performs no second lookup.
func TestHTTPDialsOnlyCheckedAddresses(t *testing.T) {
	_, port := serverPort(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r := &fakeResolver{addrs: addrs("127.0.0.1")}
	var dialed []string
	p := Prober{
		Resolver: r,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialed = append(dialed, addr)
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
	res, _ := p.HTTP(context.Background(), target("http://svc.internal.test:"+port+"/", true))
	if !res.OK {
		t.Fatalf("got %+v", res)
	}
	if r.calls.Load() != 1 || len(dialed) != 1 || dialed[0] != "127.0.0.1:"+port {
		t.Errorf("lookups=%d dialed=%v", r.calls.Load(), dialed)
	}
}

func TestHTTPDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Bool
	_, port := serverPort(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			followed.Store(true)
			return
		}
		http.Redirect(w, r, "/target", http.StatusFound)
	}))
	res, _ := Prober{}.HTTP(context.Background(), target("http://127.0.0.1:"+port+"/", true))
	if followed.Load() || res.StatusCode != http.StatusFound || res.OK {
		t.Errorf("redirect followed or misreported: %+v", res)
	}
}

func TestHTTPIgnoresProxyEnvironment(t *testing.T) {
	var proxied atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxied.Store(true) }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	_, port := serverPort(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	res, _ := Prober{}.HTTP(context.Background(), target("http://127.0.0.1:"+port+"/", true))
	if proxied.Load() || !res.OK {
		t.Errorf("proxy used or probe failed: %+v", res)
	}
}

func TestHTTPFailuresAreClassified(t *testing.T) {
	// Timeout: server that never answers.
	block := make(chan struct{})
	_, slowPort := serverPort(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	t.Cleanup(func() { close(block) }) // runs before the server's Close
	tt := target("http://127.0.0.1:"+slowPort+"/", true)
	tt.Timeout = 200 * time.Millisecond
	if res, _ := (Prober{}).HTTP(context.Background(), tt); res.Failure != FailureTimeout {
		t.Errorf("timeout: %+v", res)
	}

	// Connection refused: a port nobody listens on.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closedAddr := ln.Addr().String()
	ln.Close()
	if res, _ := (Prober{}).HTTP(context.Background(), target("http://"+closedAddr+"/", true)); res.Failure != FailureConnectionRefused {
		t.Errorf("refused: %+v", res)
	}

	// TLS: self-signed certificate not in the trust store.
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer tlsSrv.Close()
	if res, _ := (Prober{}).HTTP(context.Background(), target(tlsSrv.URL+"/", true)); res.Failure != FailureTLS {
		t.Errorf("tls: %+v", res)
	}

	// DNS failures.
	nx := Prober{Resolver: &fakeResolver{err: &net.DNSError{Err: "no such host", Name: "x", IsNotFound: true}}}
	if res, _ := nx.HTTP(context.Background(), target("http://missing.test/", false)); res.Failure != FailureNotFound {
		t.Errorf("nxdomain: %+v", res)
	}
	empty := Prober{Resolver: &fakeResolver{}}
	if res, _ := empty.HTTP(context.Background(), target("http://empty.test/", false)); res.Failure != FailureNotFound {
		t.Errorf("no addresses: %+v", res)
	}
}

func TestHTTPSReportsCertificateExpiry(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	p := Prober{RootCAs: poolFor(srv)}
	res, _ := p.HTTP(context.Background(), target(srv.URL+"/", true))
	if !res.OK || res.TLSNotAfter == "" {
		t.Fatalf("got %+v", res)
	}
	if _, err := time.Parse(time.RFC3339, res.TLSNotAfter); err != nil {
		t.Error(err)
	}
}

// Failure results must not carry raw error text, which can include internal
// hostnames or addresses.
func TestResultsCarryNoErrorText(t *testing.T) {
	p := Prober{Resolver: &fakeResolver{err: errors.New("lookup SECRET-internal.corp on 10.9.8.7:53: boom")}}
	res, err := p.HTTP(context.Background(), target("http://x.test/", false))
	b, _ := json.Marshal(res)
	if err != nil || strings.Contains(string(b), "SECRET") || strings.Contains(string(b), "10.9.8.7") {
		t.Errorf("leaked: %s %v", b, err)
	}
	d, err := p.DNS(context.Background(), config.DNSTarget{Name: "x", Host: "x.test"})
	b, _ = json.Marshal(d)
	if err != nil || d.Failure != FailureDNS || strings.Contains(string(b), "SECRET") {
		t.Errorf("leaked: %s %v", b, err)
	}
}

func TestDNS(t *testing.T) {
	p := Prober{Resolver: &fakeResolver{addrs: addrs("2001:db8::1", "192.0.2.1", "::ffff:192.0.2.1", "192.0.2.1")}}
	res, err := p.DNS(context.Background(), config.DNSTarget{Name: "x", Host: "x.test"})
	if err != nil || !res.OK || strings.Join(res.Addresses, ",") != "192.0.2.1,2001:db8::1" {
		t.Errorf("got %+v, %v", res, err)
	}

	nx := Prober{Resolver: &fakeResolver{err: &net.DNSError{IsNotFound: true}}}
	res, _ = nx.DNS(context.Background(), config.DNSTarget{Host: "x.test"})
	if res.OK || res.Failure != FailureNotFound || res.Addresses == nil {
		t.Errorf("nxdomain: %+v", res)
	}
	to := Prober{Resolver: &fakeResolver{err: &net.DNSError{IsTimeout: true}}}
	if res, _ = to.DNS(context.Background(), config.DNSTarget{Host: "x.test"}); res.Failure != FailureTimeout {
		t.Errorf("timeout: %+v", res)
	}
}

func poolFor(srv *httptest.Server) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(srv.Certificate())
	return p
}
