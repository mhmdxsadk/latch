package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mhmdxsadk/latch/internal/policy"
)

type listResourcesIn struct{}

type listResourcesOut struct {
	Disks     []string `json:"disks"`
	DNSChecks []string `json:"dns_checks"`
}

func registerResources(s *sdk.Server, d *Deps) {
	sdk.AddTool(s, &sdk.Tool{
		Name: "list_resources",
		Description: "List the named disks and DNS checks this Latch instance allows you to query. " +
			"Docker containers and systemd units are not named in advance: use docker_list and systemd_list.",
		Annotations: readOnly,
	}, wrap(d, "list_resources", func(context.Context, listResourcesIn) (listResourcesOut, string, error) {
		// Slices are never nil so the output schema (array) always validates.
		nz := func(k policy.Kind) []string {
			if n := d.Policy.Names(k); n != nil {
				return n
			}
			return []string{}
		}
		return listResourcesOut{
			Disks:     nz(policy.KindDisk),
			DNSChecks: nz(policy.KindDNS),
		}, "", nil
	}))
}
