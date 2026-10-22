package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mhmdxsadk/latch/internal/policy"
)

type listResourcesIn struct{}

type listResourcesOut struct {
	DockerServices []string `json:"docker_services"`
	SystemdUnits   []string `json:"systemd_units"`
	Disks          []string `json:"disks"`
	HTTPChecks     []string `json:"http_checks"`
	DNSChecks      []string `json:"dns_checks"`
}

func registerResources(s *sdk.Server, d *Deps) {
	sdk.AddTool(s, &sdk.Tool{
		Name:        "list_resources",
		Description: "List the names of every resource this Latch instance allows you to query.",
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
			DockerServices: nz(policy.KindDocker),
			SystemdUnits:   nz(policy.KindSystemd),
			Disks:          nz(policy.KindDisk),
			HTTPChecks:     nz(policy.KindHTTP),
			DNSChecks:      nz(policy.KindDNS),
		}, "", nil
	}))
}
