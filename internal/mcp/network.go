package mcp

import (
	"context"
	"net/netip"
	"slices"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mhmdxsadk/latch/internal/netcheck"
)

type pingIn struct {
	Host  string `json:"host" jsonschema:"hostname or IP address"`
	Count int    `json:"count,omitempty" jsonschema:"echo requests to send, 1 to 5; default 3"`
}

type dnsIn struct {
	Name string `json:"name" jsonschema:"name to look up; for PTR an IP address"`
	Type string `json:"type,omitempty" jsonschema:"record type: A, AAAA, CNAME, MX, TXT, NS, SRV or PTR; default A"`
}

type tcpIn struct {
	Host string `json:"host" jsonschema:"hostname or IP address"`
	Port int    `json:"port" jsonschema:"TCP port, 1 to 65535"`
}

type tracerouteIn struct {
	Host    string `json:"host" jsonschema:"hostname or IP address"`
	MaxHops int    `json:"max_hops,omitempty" jsonschema:"maximum hops, 1 to 30; default 20"`
}

type httpGetIn struct {
	URL         string `json:"url" jsonschema:"http or https URL, without user name or password"`
	InsecureTLS bool   `json:"insecure_tls,omitempty" jsonschema:"skip certificate verification (the result says it was skipped); default false"`
}

// Messages for out-of-range bounds. They do not echo input.
var (
	errPingCount = userError{"count must be between 1 and 5"}
	errMaxHops   = userError{"max_hops must be between 1 and 30"}
	errPort      = userError{"port must be between 1 and 65535"}
	errDNSType   = userError{"type must be one of A, AAAA, CNAME, MX, TXT, NS, SRV, PTR"}
)

// networkTool marks tools that reach outside this server. They only read.
var networkTool = &sdk.ToolAnnotations{
	ReadOnlyHint:  true,
	OpenWorldHint: ptr(true),
}

// networkHost passes an agent host through policy.
func (d *Deps) networkHost(s string) (string, error) {
	h, err := d.Policy.NetworkHost(s)
	return h, netcheck.PolicyError(err)
}

func registerNetwork(s *sdk.Server, d *Deps) {
	sdk.AddTool(s, &sdk.Tool{
		Name: "net_ping",
		Description: "Ping a host from this server (ICMP echo) and return packet loss and round-trip times. " +
			"A hostname is resolved first and its first address is pinged.",
		Annotations: networkTool,
	}, wrap(d, "net_ping", func(ctx context.Context, in pingIn) (netcheck.PingResult, string, error) {
		count := in.Count
		if count == 0 {
			count = netcheck.DefaultPingCount
		}
		if count < 1 || count > netcheck.MaxPingCount {
			return netcheck.PingResult{}, "", errPingCount
		}
		host, err := d.networkHost(in.Host)
		if err != nil {
			return netcheck.PingResult{}, "", err
		}
		r, err := d.Net.Ping(ctx, host, count)
		return r, host, err
	}))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "net_dns",
		Description: "Look up a DNS name with this server's resolver. Up to 64 answers are returned.",
		Annotations: networkTool,
	}, wrap(d, "net_dns", func(ctx context.Context, in dnsIn) (netcheck.DNSResult, string, error) {
		typ := strings.ToUpper(in.Type)
		if typ == "" {
			typ = "A"
		}
		if !slices.Contains(netcheck.RecordTypes, typ) {
			return netcheck.DNSResult{}, "", errDNSType
		}
		var name string
		var err error
		if typ == "PTR" {
			name, err = d.networkHost(in.Name)
			if err == nil {
				if _, perr := netip.ParseAddr(name); perr != nil {
					err = netcheck.ErrInvalid // PTR takes an IP address
				}
			}
		} else {
			name, err = d.Policy.DNSQueryName(in.Name)
			err = netcheck.PolicyError(err)
		}
		if err != nil {
			return netcheck.DNSResult{}, "", err
		}
		r, err := d.Net.DNS(ctx, name, typ)
		return r, name, err
	}))

	sdk.AddTool(s, &sdk.Tool{
		Name: "net_tcp",
		Description: "Check whether a TCP port accepts connections from this server. Nothing is sent. " +
			"Reports open, refused, timeout or unreachable for each address tried.",
		Annotations: networkTool,
	}, wrap(d, "net_tcp", func(ctx context.Context, in tcpIn) (netcheck.TCPResult, string, error) {
		if in.Port < 1 || in.Port > 65535 {
			return netcheck.TCPResult{}, "", errPort
		}
		host, err := d.networkHost(in.Host)
		if err != nil {
			return netcheck.TCPResult{}, "", err
		}
		r, err := d.Net.TCP(ctx, host, in.Port)
		return r, host, err
	}))

	sdk.AddTool(s, &sdk.Tool{
		Name: "net_traceroute",
		Description: "Trace the route from this server to a host and return each hop's address and round-trip time. " +
			"Takes up to 30 seconds.",
		Annotations: networkTool,
	}, wrap(d, "net_traceroute", func(ctx context.Context, in tracerouteIn) (netcheck.TraceResult, string, error) {
		hops := in.MaxHops
		if hops == 0 {
			hops = netcheck.DefaultMaxHops
		}
		if hops < 1 || hops > netcheck.MaxHops {
			return netcheck.TraceResult{}, "", errMaxHops
		}
		host, err := d.networkHost(in.Host)
		if err != nil {
			return netcheck.TraceResult{}, "", err
		}
		r, err := d.Net.Traceroute(ctx, host, hops)
		return r, host, err
	}))

	sdk.AddTool(s, &sdk.Tool{
		Name: "net_http_get",
		Description: "Fetch a URL from this server with an HTTP GET and return the status, a few headers and up to 8 KiB of a text body. " +
			"Follows up to 5 redirects. No custom headers, cookies or body can be sent.",
		Annotations: networkTool,
	}, wrap(d, "net_http_get", func(ctx context.Context, in httpGetIn) (netcheck.HTTPResult, string, error) {
		// The audit label is the checked host only, never the URL.
		_, host, err := netcheck.ParseURL(d.Policy, in.URL)
		if err != nil {
			return netcheck.HTTPResult{}, "", err
		}
		r, err := d.Net.HTTPGet(ctx, in.URL, in.InsecureTLS)
		return r, host, err
	}))
}
