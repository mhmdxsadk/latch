package mcp

import (
	"context"
	"errors"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mhmdxsadk/latch/internal/files"
	"github.com/mhmdxsadk/latch/internal/host"
)

type noInput struct{}

type disksOut struct {
	Disks []host.Disk `json:"disks"`
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
		Description: "Return the usage of every mounted filesystem that holds data, in bytes and inodes.",
		Annotations: readOnly,
	}, wrap(d, "host_disk", func(ctx context.Context, _ noInput) (disksOut, string, error) {
		all, err := d.Host.Disks(ctx)
		if err != nil {
			return disksOut{}, "", err
		}
		// A mount point inside a blocked folder is left out like the
		// folder itself.
		out := disksOut{Disks: []host.Disk{}}
		for _, disk := range all {
			err := d.Files.Visible(ctx, disk.Mount)
			if errors.Is(err, files.ErrUnavailable) {
				return disksOut{}, "", err
			}
			if err == nil {
				out.Disks = append(out.Disks, disk)
			}
		}
		return out, "", nil
	}))
}
