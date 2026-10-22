package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mhmdxsadk/latch/internal/host"
)

type noInput struct{}

type diskIn struct {
	Name string `json:"name" jsonschema:"name of a configured disk, from list_resources"`
}

func registerHost(s *sdk.Server, d *Deps) {
	sdk.AddTool(s, &sdk.Tool{
		Name:        "host_load",
		Description: "Return the system load averages and CPU count.",
		Annotations: readOnly,
	}, wrap(d, "host_load", func(ctx context.Context, _ noInput) (host.Load, string, error) {
		l, err := d.Host.Load(ctx)
		return l, "", err
	}))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "host_memory",
		Description: "Return memory and swap usage in bytes.",
		Annotations: readOnly,
	}, wrap(d, "host_memory", func(ctx context.Context, _ noInput) (host.Memory, string, error) {
		m, err := d.Host.Memory(ctx)
		return m, "", err
	}))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "host_disk",
		Description: "Return usage of a configured filesystem, in bytes and inodes.",
		Annotations: readOnly,
	}, wrap(d, "host_disk", func(ctx context.Context, in diskIn) (host.Disk, string, error) {
		disk, err := d.Policy.Disk(in.Name)
		if err != nil {
			return host.Disk{}, "", err
		}
		u, err := d.Host.Disk(ctx, disk.Path)
		return u, disk.Name, err
	}))
}
