package netcheck

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"
)

// MaxWriteBody bounds the body http_request sends.
const MaxWriteBody = 64 << 10

var (
	// WriteMethods are the methods http_request may use.
	WriteMethods = []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
	// WriteContentTypes are the request content types it may send.
	WriteContentTypes = []string{"application/json", "text/plain", "application/x-www-form-urlencoded"}
)

// Sender sends HTTP requests that change something. Every request has
// been approved by the owner; the target is checked again when it is sent.
type Sender interface {
	// CheckHTTPTarget checks rawURL like net_http_get does, including the
	// addresses its host resolves to, and returns the host (audit label).
	CheckHTTPTarget(ctx context.Context, rawURL string) (string, error)
	// HTTPRequest sends one request and never follows a redirect.
	HTTPRequest(ctx context.Context, method, rawURL, contentType string, body []byte) (HTTPResult, error)
}

var _ Sender = (*Prober)(nil)

// DisplayURL formats an agent URL the way results show it: user info and
// query values hidden, sanitized. It returns "" for an unparsable URL.
func DisplayURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return displayURL(u)
}

func defaultPort(u *url.URL) uint16 {
	if p := u.Port(); p != "" {
		n, _ := strconv.ParseUint(p, 10, 16)
		return uint16(n)
	}
	if u.Scheme == "https" {
		return 443
	}
	return 80
}

// CheckHTTPTarget checks the URL and, when the host resolves, every address
// it resolves to. A name that does not resolve now is not refused: the
// address is checked again when the connection is made.
func (p *Prober) CheckHTTPTarget(ctx context.Context, rawURL string) (string, error) {
	u, host, err := ParseURL(p.rules, rawURL)
	if err != nil {
		return "", err
	}
	g, err := p.guard(ctx)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	if _, _, err := g.resolve(ctx, u.Hostname(), defaultPort(u)); err != nil {
		return "", err
	}
	return host, nil
}

// HTTPRequest sends method to rawURL with body, a fixed User-Agent and the
// given Content-Type, and no other header. A redirect is reported (status
// and Location) but not followed. Certificates are always verified.
func (p *Prober) HTTPRequest(ctx context.Context, method, rawURL, contentType string, body []byte) (HTTPResult, error) {
	if !slices.Contains(WriteMethods, method) || len(body) > MaxWriteBody ||
		(contentType != "" && !slices.Contains(WriteContentTypes, contentType)) {
		return HTTPResult{}, ErrInvalid
	}
	u, _, err := ParseURL(p.rules, rawURL)
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
	client, tr := p.client(g, false)
	defer tr.CloseIdleConnections()

	res := HTTPResult{URL: displayURL(u), FinalURL: displayURL(u), Redirects: []string{}, Headers: map[string]string{}}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return HTTPResult{}, ErrInvalid
	}
	req.Header = http.Header{"User-Agent": {p.userAgent}}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		if isNotAllowed(err) {
			return HTTPResult{}, ErrNotAllowed
		}
		res.Failure, res.TLS = classifyHTTP(err, u.Scheme == "https")
		res.DurationMS = time.Since(start).Milliseconds()
		return res, nil
	}
	defer resp.Body.Close()
	p.fill(&res, resp, u, false)
	res.DurationMS = time.Since(start).Milliseconds()
	return res, nil
}
