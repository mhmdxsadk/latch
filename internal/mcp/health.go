package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mhmdxsadk/latch/internal/health"
)

type checkIn struct {
	Name string `json:"name" jsonschema:"name of a configured check, from list_resources"`
}

type dnsCheckOut struct {
	Name   string           `json:"name"`
	Result health.DNSResult `json:"result"`
}

func registerHealth(s *sdk.Server, d *Deps) {
	// dns_check reaches the network, but only for configured names.
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
