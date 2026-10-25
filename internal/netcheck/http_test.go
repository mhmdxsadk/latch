package netcheck

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// server starts an HTTP server on 127.0.0.1 and counts its requests.
func server(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		h(w, r)
	}))
	t.Cleanup(s.Close)
	return s, &n
}

func portOf(t *testing.T, s *httptest.Server) int {
	t.Helper()
	u, _ := url.Parse(s.URL)
	p, _ := strconv.Atoi(u.Port())
	return p
}

func get(t *testing.T, e *testEnv, u string) HTTPResult {
	t.Helper()
	r, err := e.p.HTTPGet(context.Background(), u, false)
	if err != nil {
		t.Fatalf("HTTPGet(%s): %v", u, err)
	}
	return r
}

func TestHTTPGetSendsNothingButUserAgent(t *testing.T) {
	e := newEnv(t)
	var got *http.Request
	s, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Server", "te\u202est")
		w.Header().Set("Set-Cookie", "session=abc123")
		w.Header().Set("WWW-Authenticate", "Basic realm=x")
		w.Header().Set("X-Api-Key", "k")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, "hello\n")
	})
	r := get(t, e, s.URL+"/path?q=1#frag")
	if got.Method != http.MethodGet || got.Header.Get("User-Agent") != "latch/test" {
		t.Errorf("request %s %v", got.Method, got.Header)
	}
	for k := range got.Header {
		if k != "User-Agent" && k != "Connection" {
			t.Errorf("unexpected request header %q", k)
		}
	}
	if got.ContentLength != 0 || got.URL.RawQuery != "q=1" {
		t.Errorf("request body/query: %d %q", got.ContentLength, got.URL.RawQuery)
	}
	if r.StatusCode != 200 || r.Body != "hello\n" || r.BodyBytes != 6 || r.Truncated || r.BodyWithheld {
		t.Errorf("got %+v", r)
	}
	for k := range r.Headers {
		switch k {
		case "Content-Type", "Content-Length", "Server", "Date", "Cache-Control":
		default:
			t.Errorf("header %q must not be returned", k)
		}
	}
	if r.Headers["Server"] != "test" || r.Headers["Content-Length"] != "6" || r.Headers["Cache-Control"] != "no-store" {
		t.Errorf("headers %v", r.Headers)
	}
	if !strings.HasSuffix(r.URL, "/path?q=[hidden]") || r.FinalURL != r.URL || len(r.Redirects) != 0 {
		t.Errorf("urls %q %q %v", r.URL, r.FinalURL, r.Redirects)
	}
	if r.TLS != nil {
		t.Errorf("tls on http: %+v", r.TLS)
	}
}

func TestHTTPBodyHandling(t *testing.T) {
	e := newEnv(t)
	s, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/secret":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"password": "hunter2", "token": "ghp_abcdefghijklmnopqrstuvwxyz0123456789"}`+"\n\x1b]0;title\x07ok\u202e")
		case "/big":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, strings.Repeat("a", 20<<10))
		case "/straddle":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, strings.Repeat("a", MaxBodyBytes-20)+" password=hunter2hunter2hunter2 tail")
		case "/png":
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"))
		case "/nul":
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("text\x00more"))
		case "/binary-untyped":
			w.Header()["Content-Type"] = nil
			w.Write([]byte{0xff, 0xfe, 0xfd, 0x80, 0x81})
		case "/text-untyped":
			w.Header()["Content-Type"] = nil
			fmt.Fprint(w, "plain words")
		}
	})

	r := get(t, e, s.URL+"/secret")
	if strings.Contains(r.Body, "hunter2") || strings.Contains(r.Body, "ghp_") || !strings.Contains(r.Body, "[hidden]") {
		t.Errorf("secrets not redacted: %q", r.Body)
	}
	if strings.ContainsAny(r.Body, "\x1b\x07\u202e") {
		t.Errorf("escapes not removed: %q", r.Body)
	}

	r = get(t, e, s.URL+"/big")
	if !r.Truncated || r.BodyBytes != MaxBodyBytes || len(r.Body) != MaxBodyBytes {
		t.Errorf("big: truncated=%v bytes=%d len=%d", r.Truncated, r.BodyBytes, len(r.Body))
	}

	r = get(t, e, s.URL+"/straddle")
	if strings.Contains(r.Body, "hunter2") {
		t.Errorf("secret straddling the limit leaked: %q", r.Body[len(r.Body)-40:])
	}

	for _, p := range []string{"/png", "/nul", "/binary-untyped"} {
		r = get(t, e, s.URL+p)
		if r.Body != "" || !r.BodyWithheld || r.BodyBytes == 0 {
			t.Errorf("%s: body %q withheld=%v bytes=%d", p, r.Body, r.BodyWithheld, r.BodyBytes)
		}
	}
	if r = get(t, e, s.URL+"/text-untyped"); r.Body != "plain words" {
		t.Errorf("untyped text: %+v", r)
	}
}

func TestHTTPRedirects(t *testing.T) {
	e := newEnv(t)
	e.name("srv.test", addrs("127.0.0.1"))
	var s *httptest.Server
	s, hits := server(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/middle?token=abc&page=2", http.StatusFound)
		case "/middle":
			w.Header().Set("Location", "http://srv.test:"+strconv.Itoa(portOf(t, s))+"/final")
			w.WriteHeader(http.StatusMovedPermanently)
		case "/final":
			fmt.Fprint(w, "done")
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusTemporaryRedirect)
		case "/page":
			w.Header().Set("Location", "/elsewhere?code=xyz")
			w.WriteHeader(http.StatusCreated)
		}
	})
	r := get(t, e, s.URL+"/start")
	if r.StatusCode != 200 || r.Body != "done" || len(r.Redirects) != 2 {
		t.Fatalf("got %+v", r)
	}
	if !strings.HasSuffix(r.Redirects[0], "/middle?token=[hidden]&page=[hidden]") ||
		!strings.HasPrefix(r.FinalURL, "http://srv.test:") || r.Redirects[1] != r.FinalURL {
		t.Errorf("chain %v final %q", r.Redirects, r.FinalURL)
	}

	hits.Store(0)
	r = get(t, e, s.URL+"/loop")
	if r.Failure != FailureRedirects || len(r.Redirects) != MaxRedirects || hits.Load() != MaxRedirects+1 || r.StatusCode != 307 {
		t.Errorf("loop: %+v hits=%d", r, hits.Load())
	}

	r = get(t, e, s.URL+"/page")
	if r.StatusCode != 201 || !strings.HasSuffix(r.Headers["Location"], "/elsewhere?code=[hidden]") {
		t.Errorf("location header: %v", r.Headers)
	}
}

func TestHTTPRedirectsAreChecked(t *testing.T) {
	e := newEnv(t)
	// The second server's port is published by an excluded container, so
	// reaching it on this host must fail even through a redirect.
	other, otherHits := server(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "secret") })
	e.ex.snap.Ports = append(e.ex.snap.Ports, uint16(portOf(t, other)))
	e.name("rebind.test", addrs("127.0.0.1"), addrs("172.18.0.5"))
	e.name("container.test", addrs("::ffff:172.18.0.5"))
	e.name("localhost", addrs("127.0.0.1", "::1"))

	var target atomic.Value
	s, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			fmt.Fprint(w, "ok")
			return
		}
		w.Header().Set("Location", target.Load().(string))
		w.WriteHeader(http.StatusFound)
	})
	port := strconv.Itoa(portOf(t, s))
	otherPort := strconv.Itoa(portOf(t, other))
	for _, loc := range []string{
		"http://127.0.0.1:" + otherPort + "/", // excluded port on loopback
		"http://localhost:" + otherPort + "/", // a name for it
		"http://[::ffff:127.0.0.1]:" + otherPort + "/",
		"http://0.0.0.0:" + otherPort + "/",
		"http://172.18.0.5/",          // excluded container
		"http://[fd00:dead::5]/",      // excluded container, IPv6
		"http://[::ffff:172.18.0.5]/", // mapped
		"http://container.test/",      // name for a container
		"http://blocked.example/",     // network.exclude
		"http://BLOCKED.Example./",    // case and trailing dot
		"http://10.9.9.9/",            // network.exclude literal
		"file:///etc/passwd",          // other schemes
		"gopher://127.0.0.1:" + port + "/",
		"ftp://127.0.0.1/",
		"javascript:alert(1)",
		"http://user:pass@127.0.0.1:" + port + "/ok", // user info
		"http://user@127.0.0.1:" + port + "/ok",
		"http://127.0.0.1:0/",
		"http://127.0.0.1:99999/",
		"http://bad_host/",
	} {
		target.Store(loc)
		_, err := e.p.HTTPGet(context.Background(), s.URL+"/", false)
		if !errors.Is(err, ErrNotAllowed) {
			t.Errorf("redirect to %q: err = %v, want ErrNotAllowed", loc, err)
		}
	}
	if otherHits.Load() != 0 {
		t.Errorf("excluded port reached %d times", otherHits.Load())
	}

	// DNS rebinding between hops: the same name later resolves to an
	// excluded container.
	target.Store("http://rebind.test:" + port + "/ok")
	if r, err := e.p.HTTPGet(context.Background(), "http://rebind.test:"+port+"/", false); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("rebinding: %+v %v", r, err)
	}
}

func TestHTTPRejectsBadURLsBeforeNetwork(t *testing.T) {
	e := newEnv(t)
	for _, u := range []string{
		"", "example.com", "//example.com/", "ftp://example.com/", "file:///etc/passwd", "HTTP://",
		"http://user:pass@example.com/", "http://user@example.com/", "http://:pass@example.com/",
		"http://example.com:0/", "http://example.com:65536/", "http://example.com:080/", "http://example.com:x/",
		"http://[fe80::1%25eth0]/", "http://*.example.com/", "http://exa mple.com/", "http://exаmple.com/",
		"http://example.com/\x00", "http://example.com/\r\nX-Injected: 1",
		"http://" + strings.Repeat("a", MaxURLBytes) + ".com/",
		"mailto:a@example.com", "data:text/plain,hi",
	} {
		if _, err := e.p.HTTPGet(context.Background(), u, false); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: err = %v, want ErrInvalid", u, err)
		}
	}
	for _, u := range []string{"http://blocked.example/", "https://BLOCKED.EXAMPLE./x", "http://10.9.9.9:8080/", "http://[::ffff:10.9.9.9]/"} {
		if _, err := e.p.HTTPGet(context.Background(), u, false); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("%q: err = %v, want ErrNotAllowed", u, err)
		}
	}
	if e.res.calls.Load() != 0 {
		t.Errorf("%d lookups for rejected URLs", e.res.calls.Load())
	}
}

func TestParseURLLabel(t *testing.T) {
	p := testPolicy(t)
	for raw, want := range map[string]string{
		"http://Example.COM./a?token=x":   "example.com",
		"https://[::1]:8443/":             "::1",
		"http://[::ffff:127.0.0.1]:80/":   "127.0.0.1",
		"http://127.0.0.1/#secret-anchor": "127.0.0.1",
	} {
		u, host, err := ParseURL(p, raw)
		if err != nil || host != want || u.Fragment != "" {
			t.Errorf("ParseURL(%q) = %v, %q, %v", raw, u, host, err)
		}
	}
}

func TestHTTPFailures(t *testing.T) {
	e := newEnv(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().String()
	ln.Close()
	r := get(t, e, "http://"+closed+"/")
	if r.Failure != FailureRefused {
		t.Errorf("refused: %+v", r)
	}
	r = get(t, e, "http://nxdomain.test/")
	if r.Failure != FailureNotFound {
		t.Errorf("nxdomain: %+v", r)
	}
	// A server that is not HTTP.
	raw, _ := net.Listen("tcp", "127.0.0.1:0")
	defer raw.Close()
	go func() {
		c, err := raw.Accept()
		if err != nil {
			return
		}
		c.Write([]byte("SSH-2.0-OpenSSH_9.9 secret-banner\r\n"))
		c.Close()
	}()
	r = get(t, e, "http://"+raw.Addr().String()+"/")
	if r.Failure == FailureNone || strings.Contains(fmt.Sprint(r), "secret-banner") {
		t.Errorf("non-HTTP: %+v", r)
	}
}

func TestHTTPTimeoutIsBounded(t *testing.T) {
	e := newEnv(t)
	s, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r, err := e.p.HTTPGet(ctx, s.URL, false)
	if err != nil || r.Failure != FailureTimeout {
		t.Errorf("%+v %v", r, err)
	}
}

func TestHTTPS(t *testing.T) {
	e := newEnv(t)
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "secure") }))
	s.Config.ErrorLog = log.New(io.Discard, "", 0) // expected handshake failures
	s.StartTLS()
	defer s.Close()
	port := strconv.Itoa(portOf(t, s))
	e.name("wrong-name.test", addrs("127.0.0.1"))

	// Not trusted.
	e.p.roots = x509.NewCertPool()
	r := get(t, e, s.URL)
	if r.Failure != FailureTLS || r.TLS == nil || r.TLS.Error != TLSUnknownAuthority || r.Body != "" {
		t.Errorf("untrusted: %+v %+v", r, r.TLS)
	}

	// Trusted.
	e.p.roots = x509.NewCertPool()
	e.p.roots.AddCert(s.Certificate())
	r = get(t, e, s.URL)
	if r.Failure != FailureNone || r.Body != "secure" || r.TLS == nil || !r.TLS.Verified || r.TLS.VerificationSkipped ||
		r.TLS.Certificate == nil || r.TLS.Certificate.NotAfter == "" {
		t.Errorf("trusted: %+v %+v", r, r.TLS)
	}

	// Trusted, wrong name.
	r = get(t, e, "https://wrong-name.test:"+port+"/")
	if r.Failure != FailureTLS || r.TLS == nil || r.TLS.Error != TLSHostname {
		t.Errorf("hostname: %+v %+v", r, r.TLS)
	}

	// Untrusted but skipped on request.
	e.p.roots = x509.NewCertPool()
	r, err := e.p.HTTPGet(context.Background(), "https://wrong-name.test:"+port+"/", true)
	if err != nil || r.Body != "secure" || r.TLS == nil || r.TLS.Verified || !r.TLS.VerificationSkipped ||
		r.TLS.Certificate == nil || len(r.TLS.Certificate.DNSNames) == 0 {
		t.Errorf("insecure: %+v %+v %v", r, r.TLS, err)
	}
}

func TestTLSCategories(t *testing.T) {
	wrap := func(err error) error {
		return &url.Error{Op: "Get", URL: "x", Err: &tls.CertificateVerificationError{Err: err}}
	}
	for err, want := range map[error]string{
		wrap(x509.UnknownAuthorityError{}):                                   TLSUnknownAuthority,
		wrap(x509.CertificateInvalidError{Reason: x509.Expired}):             TLSExpired,
		wrap(x509.CertificateInvalidError{Reason: x509.NotAuthorizedToSign}): TLSInvalid,
		wrap(x509.HostnameError{Host: "x"}):                                  TLSHostname,
		wrap(errors.New("other")):                                            TLSInvalid,
		tls.RecordHeaderError{Msg: "m"}:                                      TLSHandshake,
		errors.New("plain"):                                                  "",
	} {
		if got := tlsCategory(err); got != want {
			t.Errorf("tlsCategory(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestDisplayURL(t *testing.T) {
	u, _ := url.Parse("https://example.com/p?token=abc&flag&empty=&x=1#frag")
	if got := displayURL(u); got != "https://example.com/p?token=[hidden]&flag&empty=&x=[hidden]" {
		t.Errorf("got %q", got)
	}
}
