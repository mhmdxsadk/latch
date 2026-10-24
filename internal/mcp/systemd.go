package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mhmdxsadk/latch/internal/systemd"
)

type unitIn struct {
	Unit string `json:"unit" jsonschema:"full systemd unit name, for example ssh.service"`
}

type unitStatusOut struct {
	Unit   string             `json:"unit"`
	Status systemd.UnitStatus `json:"status"`
}

type unitListOut struct {
	Units []systemd.ListedUnit `json:"units"`
}

func registerSystemd(s *sdk.Server, d *Deps) {
	sdk.AddTool(s, &sdk.Tool{
		Name: "systemd_list",
		Description: "List the loaded systemd services with their active state. " +
			"Other unit types (timers, sockets, ...) are not listed but can be queried by full name with systemd_unit_status.",
		Annotations: readOnly,
	}, wrap(d, "systemd_list", func(ctx context.Context, _ noInput) (unitListOut, string, error) {
		units, err := d.Systemd.List(ctx)
		if err != nil {
			return unitListOut{}, "", err
		}
		out := unitListOut{Units: []systemd.ListedUnit{}}
		for _, u := range units {
			if d.Policy.UnitVisible(u.Name) {
				out.Units = append(out.Units, u)
			}
		}
		return out, "", nil
	}))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "systemd_unit_status",
		Description: "Return the load, active and sub state, enablement and (for services) last result and restart count of a systemd unit.",
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
