package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mhmdxsadk/latch/internal/health"
)

type checkIn struct {
	Name string `json:"name" jsonschema:"name of a configured check, from list_resources"`
}

type httpCheckOut struct {
	Name   string            `json:"name"`
	Result health.HTTPResult `json:"result"`
}

type dnsCheckOut struct {
	Name   string           `json:"name"`
	Result health.DNSResult `json:"result"`
}

func registerHealth(s *sdk.Server, d *Deps) {
	// These tools reach the network, but only configured targets, so they
	// are still annotated as closed-world.
	sdk.AddTool(s, &sdk.Tool{
		Name: "http_check",
		Description: "Probe a configured HTTP health-check target and report whether it returned the expected status. " +
			"Redirects are not followed. A failed probe is a normal result with a 'failure' reason, not an error.",
		Annotations: readOnly,
	}, wrap(d, "http_check", func(ctx context.Context, in checkIn) (httpCheckOut, string, error) {
		t, err := d.Policy.HTTPTarget(in.Name)
		if err != nil {
			return httpCheckOut{}, "", err
		}
		r, err := d.Health.HTTP(ctx, t)
		return httpCheckOut{Name: t.Name, Result: r}, t.Name, err
	}))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "dns_check",
		Description: "Resolve a configured DNS check target and return its addresses.",
		Annotations: readOnly,
	}, wrap(d, "dns_check", func(ctx context.Context, in checkIn) (dnsCheckOut, string, error) {
		t, err := d.Policy.DNSTarget(in.Name)
		if err != nil {
			return dnsCheckOut{}, "", err
		}
		r, err := d.Health.DNS(ctx, t)
		if r.Addresses == nil {
			r.Addresses = []string{}
		}
		return dnsCheckOut{Name: t.Name, Result: r}, t.Name, err
	}))
}
