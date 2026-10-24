package netcheck

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mhmdxsadk/latch/internal/redact"
	"github.com/mhmdxsadk/latch/internal/textsafe"
)

// HTTP bounds.
const (
	MaxURLBytes     = 2048
	MaxRedirects    = 5
	MaxBodyBytes    = 8 << 10
	httpTimeout     = 10 * time.Second
	maxHeaderBytes  = 16 << 10
	maxHeaderValue  = 512
	maxCertNames    = 32
	maxURLOut       = 2048
	redactLookahead = 1 << 10
)

// returnedHeaders are the only response headers the agent sees. Set-Cookie
// and anything about authentication are never among them.
var returnedHeaders = []string{"Content-Type", "Content-Length", "Location", "Server", "Date", "Cache-Control", "Last-Modified"}

// HTTPResult is the outcome of net_http_get.
type HTTPResult struct {
	URL      string `json:"url"`
	FinalURL string `json:"final_url"`
	// Redirects lists each URL redirected to, in order.
	Redirects  []string          `json:"redirects"`
	StatusCode int               `json:"status_code,omitempty"`
	Headers    map[string]string `json:"headers"`
	// Body is set only when the response is text. BodyBytes is how much
	// was read (at most MaxBodyBytes); Truncated means there was more.
	Body         string   `json:"body,omitempty"`
	BodyBytes    int      `json:"body_bytes"`
	BodyWithheld bool     `json:"body_withheld,omitempty"`
	Truncated    bool     `json:"truncated"`
	TLS          *TLSInfo `json:"tls,omitempty"`
	Failure      Failure  `json:"failure,omitempty"`
	DurationMS   int64    `json:"duration_ms"`
}

// TLSInfo describes an HTTPS connection.
type TLSInfo struct {
	Verified            bool      `json:"verified"`
	VerificationSkipped bool      `json:"verification_skipped,omitempty"`
	Error               string    `json:"error,omitempty"`
	Version             string    `json:"version,omitempty"`
	Certificate         *CertInfo `json:"certificate,omitempty"`
}

// CertInfo is basic information about the server's certificate.
type CertInfo struct {
	SubjectCN string   `json:"subject_cn"`
	IssuerCN  string   `json:"issuer_cn"`
	NotAfter  string   `json:"not_after"`
	DNSNames  []string `json:"dns_names"`
}

// Fixed TLS error categories; library error text never reaches the agent.
const (
	TLSUnknownAuthority = "certificate signed by unknown authority"
	TLSExpired          = "certificate expired or not yet valid"
	TLSHostname         = "hostname mismatch"
	TLSInvalid          = "certificate invalid"
	TLSHandshake        = "tls handshake failed"
)

// ParseURL checks an agent-supplied URL for net_http_get: http or https, no
// user info, a host that passes policy, an optional port 1-65535. It returns
// the URL with the host normalized and the fragment removed, and the host
// (the audit label).
func ParseURL(rules Rules, raw string) (*url.URL, string, error) {
	if raw == "" || len(raw) > MaxURLBytes {
		return nil, "", ErrInvalid
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", ErrInvalid
	}
	return checkURL(rules, u)
}

func checkURL(rules Rules, u *url.URL) (*url.URL, string, error) {
	if (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Opaque != "" || u.Host == "" {
		return nil, "", ErrInvalid
	}
	host, err := rules.NetworkHost(u.Hostname())
	if err != nil {
		return nil, "", PolicyError(err)
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return nil, "", ErrInvalid
		}
	}
	out := *u
	out.Fragment, out.RawFragment = "", ""
	out.Host = host
	if strings.Contains(host, ":") {
		out.Host = "[" + host + "]"
	}
	if port != "" {
		out.Host += ":" + port
	}
	return &out, host, nil
}

// displayURL formats u for the agent: query values hidden, sanitized.
func displayURL(u *url.URL) string {
	c := *u
	c.User, c.Fragment, c.RawFragment = nil, "", ""
	if c.RawQuery != "" {
		parts := strings.Split(c.RawQuery, "&")
		for i, part := range parts {
			if k, v, ok := strings.Cut(part, "="); ok && v != "" {
				parts[i] = k + "=" + redact.Hidden
			}
		}
		c.RawQuery = strings.Join(parts, "&")
	}
	return safeText(c.String(), maxURLOut)
}

// HTTPGet fetches rawURL with a GET, following at most MaxRedirects
// redirects, each checked like the first URL. Latch sends only a fixed
// User-Agent; the agent cannot add headers, cookies or a body.
func (p *Prober) HTTPGet(ctx context.Context, rawURL string, insecureTLS bool) (HTTPResult, error) {
	cur, _, err := ParseURL(p.rules, rawURL)
	if err != nil {
		return HTTPResult{}, err
	}
	g, err := p.guard(ctx)
	if err != nil {
		return HTTPResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	start := time.Now()

	tr := &http.Transport{
		Proxy:                  nil,
		DialContext:            g.dialHTTP,
		TLSClientConfig:        &tls.Config{InsecureSkipVerify: insecureTLS, RootCAs: p.roots}, //nolint:gosec // opt-in per call, reported in the result
		TLSHandshakeTimeout:    httpTimeout,
		ResponseHeaderTimeout:  httpTimeout,
		MaxResponseHeaderBytes: maxHeaderBytes,
		DisableKeepAlives:      true,
		DisableCompression:     true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	res := HTTPResult{URL: displayURL(cur), Redirects: []string{}, Headers: map[string]string{}}
	done := func() (HTTPResult, error) {
		res.DurationMS = time.Since(start).Milliseconds()
		return res, nil
	}
	for hop := 0; ; hop++ {
		res.FinalURL = displayURL(cur)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cur.String(), nil)
		if err != nil {
			return HTTPResult{}, ErrInvalid
		}
		req.Header = http.Header{"User-Agent": {p.userAgent}}
		resp, err := client.Do(req)
		if err != nil {
			if isNotAllowed(err) {
				return HTTPResult{}, ErrNotAllowed
			}
			res.Failure, res.TLS = classifyHTTP(err, cur.Scheme == "https")
			return done()
		}
		loc := resp.Header.Get("Location")
		if isRedirect(resp.StatusCode) && loc != "" && hop < MaxRedirects {
			resp.Body.Close()
			next, err := cur.Parse(loc)
			if err != nil {
				res.Failure = FailureBadRedirect
				res.StatusCode = resp.StatusCode
				return done()
			}
			// Every hop gets the same checks as the first URL; the
			// address check happens again when it is dialed.
			if next, _, err = checkURL(p.rules, next); err != nil {
				return HTTPResult{}, ErrNotAllowed
			}
			cur = next
			res.Redirects = append(res.Redirects, displayURL(cur))
			continue
		}
		if isRedirect(resp.StatusCode) && loc != "" {
			res.Failure = FailureRedirects
		}
		p.fill(&res, resp, cur, insecureTLS)
		resp.Body.Close()
		return done()
	}
}

func isRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// dialHTTP resolves the transport's host itself, checks every address with
// the port, and dials checked IP literals only, checking each again at
// connect time.
func (g *guard) dialHTTP(ctx context.Context, _, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, ErrNotAllowed
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return nil, ErrNotAllowed
	}
	addrs, failure, err := g.resolve(ctx, host, uint16(port))
	if err != nil {
		return nil, err
	}
	if failure != FailureNone {
		return nil, dnsFailure{failure}
	}
	if len(addrs) > maxTCPAddrs {
		addrs = addrs[:maxTCPAddrs]
	}
	var last error
	for _, a := range addrs {
		c, err := g.dial(ctx, a, uint16(port), tcpAttemptTime)
		if err == nil {
			return c, nil
		}
		if isNotAllowed(err) {
			return nil, ErrNotAllowed
		}
		last = err
	}
	return nil, last
}

// dnsFailure carries a resolver failure through the HTTP client.
type dnsFailure struct{ f Failure }

func (d dnsFailure) Error() string { return string(d.f) }

// classifyHTTP maps a client error to a failure and, for TLS, a fixed
// category.
func classifyHTTP(err error, https bool) (Failure, *TLSInfo) {
	var df dnsFailure
	if errors.As(err, &df) {
		return df.f, nil
	}
	if cat := tlsCategory(err); cat != "" && https {
		return FailureTLS, &TLSInfo{Error: cat}
	}
	if f := classifyConn(err); f != FailureConnection {
		return f, nil
	}
	// The text is only matched, never returned.
	if strings.Contains(err.Error(), "malformed") {
		return FailureProtocol, nil
	}
	return FailureConnection, nil
}

func tlsCategory(err error) string {
	var (
		unknown  x509.UnknownAuthorityError
		hostname x509.HostnameError
		invalid  x509.CertificateInvalidError
		verify   *tls.CertificateVerificationError
		record   tls.RecordHeaderError
		alert    tls.AlertError
	)
	switch {
	case errors.As(err, &unknown):
		return TLSUnknownAuthority
	case errors.As(err, &hostname):
		return TLSHostname
	case errors.As(err, &invalid) && invalid.Reason == x509.Expired:
		return TLSExpired
	case errors.As(err, &invalid):
		return TLSInvalid
	case errors.As(err, &verify):
		return TLSInvalid
	case errors.As(err, &record), errors.As(err, &alert):
		return TLSHandshake
	}
	return ""
}

// fill copies the safe parts of resp into res.
func (p *Prober) fill(res *HTTPResult, resp *http.Response, cur *url.URL, insecure bool) {
	res.StatusCode = resp.StatusCode
	for _, k := range returnedHeaders {
		vs := resp.Header.Values(k)
		switch {
		case k == "Content-Length" && resp.ContentLength >= 0:
			res.Headers[k] = strconv.FormatInt(resp.ContentLength, 10)
		case len(vs) == 0:
		case k == "Location":
			if u, err := cur.Parse(vs[0]); err == nil {
				res.Headers[k] = displayURL(u)
			}
		default:
			res.Headers[k] = safeText(strings.Join(vs, ", "), maxHeaderValue)
		}
	}
	if resp.TLS != nil {
		res.TLS = tlsInfo(resp.TLS, insecure)
	}

	// Read a little past the limit so redaction sees a secret that
	// straddles it, then cut.
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+redactLookahead))
	if err != nil && len(data) == 0 {
		res.Failure = classifyConn(err)
		return
	}
	res.Truncated = len(data) > MaxBodyBytes
	res.BodyBytes = min(len(data), MaxBodyBytes)
	if res.BodyBytes == 0 {
		return
	}
	if !isText(resp.Header.Get("Content-Type"), data[:res.BodyBytes]) {
		res.BodyWithheld = true
		return
	}
	body := redact.Text(textsafe.Lines(string(data)))
	if res.Truncated {
		body = textsafe.Head(body, MaxBodyBytes)
	}
	res.Body = body
}

// isText reports whether a body may be shown: never with a NUL byte; then
// either a textual Content-Type or valid UTF-8 (allowing a rune cut at the
// end).
func isText(contentType string, b []byte) bool {
	if bytes.IndexByte(b, 0) >= 0 {
		return false
	}
	mt, _, _ := mime.ParseMediaType(contentType)
	if strings.HasPrefix(mt, "text/") || strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "+xml") {
		return true
	}
	switch mt {
	case "application/json", "application/xml", "application/javascript", "application/x-www-form-urlencoded",
		"application/yaml", "application/x-yaml", "application/toml":
		return true
	}
	for i := 0; i < utf8.UTFMax && len(b) > 0 && !utf8.Valid(b); i++ {
		b = b[:len(b)-1]
	}
	return utf8.Valid(b)
}

func tlsInfo(cs *tls.ConnectionState, insecure bool) *TLSInfo {
	info := &TLSInfo{Verified: !insecure, VerificationSkipped: insecure, Version: tls.VersionName(cs.Version)}
	if len(cs.PeerCertificates) == 0 {
		return info
	}
	c := cs.PeerCertificates[0]
	ci := &CertInfo{
		SubjectCN: safeText(c.Subject.CommonName, maxHeaderValue),
		IssuerCN:  safeText(c.Issuer.CommonName, maxHeaderValue),
		NotAfter:  c.NotAfter.UTC().Format(time.RFC3339),
		DNSNames:  []string{},
	}
	for i, n := range c.DNSNames {
		if i == maxCertNames {
			break
		}
		ci.DNSNames = append(ci.DNSNames, safeText(n, 255))
	}
	info.Certificate = ci
	return info
}
