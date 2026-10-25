package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/mhmdxsadk/latch/internal/netcheck"
)

// fakeNet records what reaches the network layer, so tests can prove that
// rejected input never does.
type fakeNet struct {
	mu    sync.Mutex
	err   error
	calls []string
}

func (f *fakeNet) record(format string, a ...any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf(format, a...))
	return f.err
}

func (f *fakeNet) Ping(_ context.Context, host string, count int) (netcheck.PingResult, error) {
	return netcheck.PingResult{Address: "192.0.2.1", Sent: count, Received: count}, f.record("ping %s %d", host, count)
}
func (f *fakeNet) DNS(_ context.Context, name, typ string) (netcheck.DNSResult, error) {
	return netcheck.DNSResult{Name: name, Type: typ, Answers: []string{"192.0.2.1"}}, f.record("dns %s %s", name, typ)
}
func (f *fakeNet) TCP(_ context.Context, host string, port int) (netcheck.TCPResult, error) {
	return netcheck.TCPResult{Port: port, Open: true, Attempts: []netcheck.TCPAttempt{{Address: "192.0.2.1", State: "open"}}},
		f.record("tcp %s %d", host, port)
}
func (f *fakeNet) Traceroute(_ context.Context, host string, maxHops int) (netcheck.TraceResult, error) {
	return netcheck.TraceResult{Address: "192.0.2.1", Program: "traceroute", Hops: []netcheck.Hop{{Hop: 1, Address: "192.0.2.1"}}},
		f.record("trace %s %d", host, maxHops)
}
func (f *fakeNet) HTTPGet(_ context.Context, rawURL string, insecure bool) (netcheck.HTTPResult, error) {
	return netcheck.HTTPResult{URL: "x", FinalURL: "x", Redirects: []string{}, Headers: map[string]string{}, StatusCode: 200},
		f.record("http %s %v", rawURL, insecure)
}

func TestNetworkToolsResolveThroughPolicy(t *testing.T) {
	fn := &fakeNet{}
	h := newHarnessWith(t, func(d *Deps) { d.Net = fn })

	if r := structured[netcheck.PingResult](t, h, "net_ping", map[string]any{"host": "Example.COM."}); r.Sent != 3 {
		t.Errorf("default count: %+v", r)
	}
	structured[netcheck.PingResult](t, h, "net_ping", map[string]any{"host": "::ffff:192.0.2.9", "count": 5})
	structured[netcheck.DNSResult](t, h, "net_dns", map[string]any{"name": "_dmarc.Example.com"})
	structured[netcheck.DNSResult](t, h, "net_dns", map[string]any{"name": "example.com", "type": "txt"})
	structured[netcheck.DNSResult](t, h, "net_dns", map[string]any{"name": "192.0.2.1", "type": "PTR"})
	structured[netcheck.TCPResult](t, h, "net_tcp", map[string]any{"host": "localhost", "port": 5432})
	structured[netcheck.TraceResult](t, h, "net_traceroute", map[string]any{"host": "example.com"})
	structured[netcheck.TraceResult](t, h, "net_traceroute", map[string]any{"host": "example.com", "max_hops": 30})
	structured[netcheck.HTTPResult](t, h, "net_http_get", map[string]any{"url": "https://example.com/a?token=s3cr3t"})
	structured[netcheck.HTTPResult](t, h, "net_http_get", map[string]any{"url": "https://example.com/", "insecure_tls": true})

	want := []string{
		"ping example.com 3", "ping 192.0.2.9 5",
		"dns _dmarc.example.com A", "dns example.com TXT", "dns 192.0.2.1 PTR",
		"tcp localhost 5432", "trace example.com 20", "trace example.com 30",
		"http https://example.com/a?token=s3cr3t false", "http https://example.com/ true",
	}
	if strings.Join(fn.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("network layer saw:\n%s", strings.Join(fn.calls, "\n"))
	}
	a := h.audit.String()
	if strings.Contains(a, "s3cr3t") || strings.Contains(a, "https://") {
		t.Errorf("audit must label HTTP calls by host only: %s", a)
	}
	if !strings.Contains(a, `"tool":"net_http_get","resource":"example.com"`) {
		t.Errorf("audit label missing: %s", a)
	}
}

func TestNetworkToolRejections(t *testing.T) {
	fn := &fakeNet{}
	h := newHarnessWith(t, func(d *Deps) { d.Net = fn })
	tests := []struct {
		tool string
		args any
		want string // substring of the message, "" for a schema rejection
	}{
		// Bounds.
		{"net_ping", map[string]any{"host": "example.com", "count": 6}, "count must be"},
		{"net_ping", map[string]any{"host": "example.com", "count": -1}, "count must be"},
		{"net_traceroute", map[string]any{"host": "example.com", "max_hops": 31}, "max_hops must be"},
		{"net_traceroute", map[string]any{"host": "example.com", "max_hops": -5}, "max_hops must be"},
		{"net_tcp", map[string]any{"host": "example.com", "port": 0}, "port must be"},
		{"net_tcp", map[string]any{"host": "example.com", "port": 65536}, "port must be"},
		{"net_dns", map[string]any{"name": "example.com", "type": "AXFR"}, "type must be"},
		{"net_dns", map[string]any{"name": "example.com", "type": "ANY"}, "type must be"},
		// Invalid hosts, before any network activity.
		{"net_ping", map[string]any{"host": "-c 1000 example.com"}, "invalid host"},
		{"net_ping", map[string]any{"host": "--flood"}, "invalid host"},
		{"net_ping", map[string]any{"host": "a;id"}, "invalid host"},
		{"net_tcp", map[string]any{"host": "example.com:22", "port": 22}, "invalid host"},
		{"net_tcp", map[string]any{"host": "[::1]", "port": 22}, "invalid host"},
		{"net_tcp", map[string]any{"host": "fe80::1%eth0", "port": 22}, "invalid host"},
		{"net_traceroute", map[string]any{"host": "*.example.com"}, "invalid host"},
		{"net_traceroute", map[string]any{"host": ""}, "invalid host"},
		{"net_dns", map[string]any{"name": "example.com", "type": "PTR"}, "invalid host"},
		{"net_dns", map[string]any{"name": "a b"}, "invalid host"},
		{"net_http_get", map[string]any{"url": "http://user:pw@example.com/"}, "invalid host"},
		{"net_http_get", map[string]any{"url": "file:///etc/shadow"}, "invalid host"},
		{"net_http_get", map[string]any{"url": "gopher://example.com/"}, "invalid host"},
		// Excluded names and addresses, in any spelling.
		{"net_ping", map[string]any{"host": "vault.internal"}, "target is not allowed"},
		{"net_ping", map[string]any{"host": "VAULT.Internal."}, "target is not allowed"},
		{"net_tcp", map[string]any{"host": "10.9.9.9", "port": 80}, "target is not allowed"},
		{"net_tcp", map[string]any{"host": "::ffff:10.9.9.9", "port": 80}, "target is not allowed"},
		{"net_dns", map[string]any{"name": "Vault.internal"}, "target is not allowed"},
		{"net_dns", map[string]any{"name": "10.9.9.9", "type": "PTR"}, "target is not allowed"},
		{"net_traceroute", map[string]any{"host": "vault.internal."}, "target is not allowed"},
		{"net_http_get", map[string]any{"url": "https://VAULT.INTERNAL./"}, "target is not allowed"},
		{"net_http_get", map[string]any{"url": "http://10.9.9.9:8200/v1/secret"}, "target is not allowed"},
		// Fields the schema does not have: no headers, method, body,
		// cookies, auth or resolver choice.
		{"net_http_get", map[string]any{"url": "https://example.com/", "headers": map[string]any{"Authorization": "x"}}, ""},
		{"net_http_get", map[string]any{"url": "https://example.com/", "method": "POST"}, ""},
		{"net_http_get", map[string]any{"url": "https://example.com/", "body": "x"}, ""},
		{"net_http_get", map[string]any{"url": "https://example.com/", "cookies": "a=b"}, ""},
		{"net_http_get", map[string]any{"url": "https://example.com/", "proxy": "http://p/"}, ""},
		{"net_http_get", map[string]any{"url": "https://example.com/", "insecure_tls": "yes"}, ""},
		{"net_http_get", map[string]any{}, ""},
		{"net_dns", map[string]any{"name": "example.com", "server": "8.8.8.8"}, ""},
		{"net_ping", map[string]any{"host": "example.com", "interface": "eth0"}, ""},
		{"net_ping", map[string]any{"host": "example.com", "size": 65000}, ""},
		{"net_tcp", map[string]any{"host": "example.com", "port": 80, "data": "GET /"}, ""},
		{"net_tcp", map[string]any{"host": "example.com", "port": "80"}, ""},
		{"net_traceroute", map[string]any{"host": "example.com", "args": []string{"-I"}}, ""},
	}
	for _, tt := range tests {
		res, err := h.call(tt.tool, tt.args)
		var text string
		switch {
		case err != nil:
			text = err.Error()
		case res.IsError:
			text = resultText(res)
		default:
			t.Errorf("%s %v: accepted", tt.tool, tt.args)
			continue
		}
		if !strings.Contains(text, tt.want) {
			t.Errorf("%s %v: message %q, want %q", tt.tool, tt.args, text, tt.want)
		}
		if strings.Contains(text, "vault") || strings.Contains(text, "10.9.9.9") {
			t.Errorf("message echoes input: %q", text)
		}
	}
	if len(fn.calls) != 0 {
		t.Errorf("rejected input reached the network layer: %v", fn.calls)
	}
	a := h.audit.String()
	if strings.Contains(a, "vault") || strings.Contains(a, "10.9.9.9") {
		t.Errorf("audit echoes rejected input: %s", a)
	}
	if !strings.Contains(a, `"reason":"not_allowed"`) || !strings.Contains(a, `"reason":"invalid_name"`) {
		t.Errorf("expected denial reasons: %s", a)
	}
}

func TestNetworkErrorsMapToFixedMessages(t *testing.T) {
	for _, tt := range []struct {
		err    error
		msg    string
		reason string
	}{
		{netcheck.ErrNotAllowed, "target is not allowed", "not_allowed"},
		{netcheck.ErrUnknown, "operation failed", "excluded_unknown"},
		{netcheck.ErrUnsupported, "not supported on this host", "unsupported"},
		{fmt.Errorf("exec /usr/bin/ping: secret detail"), "operation failed", "integration_error"},
	} {
		fn := &fakeNet{err: tt.err}
		h := newHarnessWith(t, func(d *Deps) { d.Net = fn })
		for _, c := range []struct {
			tool string
			args any
		}{
			{"net_ping", map[string]any{"host": "example.com"}},
			{"net_dns", map[string]any{"name": "example.com"}},
			{"net_tcp", map[string]any{"host": "example.com", "port": 443}},
			{"net_traceroute", map[string]any{"host": "example.com"}},
			{"net_http_get", map[string]any{"url": "https://example.com/"}},
		} {
			res, err := h.call(c.tool, c.args)
			if err != nil || !res.IsError {
				t.Errorf("%s with %v: expected a tool error, got %v %+v", c.tool, tt.err, err, res)
				continue
			}
			if text := resultText(res); !strings.Contains(text, tt.msg) || strings.Contains(text, "secret") {
				t.Errorf("%s with %v: message %q", c.tool, tt.err, text)
			}
		}
		if a := h.audit.String(); !strings.Contains(a, `"reason":"`+tt.reason+`"`) || strings.Contains(a, "secret") {
			t.Errorf("audit for %v: %s", tt.err, a)
		}
	}
}
