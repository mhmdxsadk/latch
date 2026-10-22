package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mhmdxsadk/latch/internal/systemd"
)

type unitIn struct {
	Unit string `json:"unit" jsonschema:"full name of a configured systemd unit (for example ssh.service), from list_resources"`
}

type unitStatusOut struct {
	Unit   string             `json:"unit"`
	Status systemd.UnitStatus `json:"status"`
}

func registerSystemd(s *sdk.Server, d *Deps) {
	sdk.AddTool(s, &sdk.Tool{
		Name:        "systemd_unit_status",
		Description: "Return the load, active and sub state, enablement and (for services) last result and restart count of a configured systemd unit.",
		Annotations: readOnly,
	}, wrap(d, "systemd_unit_status", func(ctx context.Context, in unitIn) (unitStatusOut, string, error) {
		unit, err := d.Policy.SystemdUnit(in.Unit)
		if err != nil {
			return unitStatusOut{}, "", err
		}
		st, err := d.Systemd.UnitStatus(ctx, unit)
		return unitStatusOut{Unit: unit, Status: st}, unit, err
	}))
}
