package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
)

const maxStatsBody = 1 << 20

// Usage is a one-shot resource usage sample of a container.
type Usage struct {
	CPUPercent       float64 `json:"cpu_percent"`
	MemoryUsedBytes  uint64  `json:"memory_used_bytes"` // without reclaimable file cache, like "docker stats"
	MemoryLimitBytes uint64  `json:"memory_limit_bytes"`
	MemoryPercent    float64 `json:"memory_percent"`
	NetRxBytes       uint64  `json:"net_rx_bytes"` // summed over all networks
	NetTxBytes       uint64  `json:"net_tx_bytes"`
	BlockReadBytes   uint64  `json:"block_read_bytes"`
	BlockWriteBytes  uint64  `json:"block_write_bytes"`
	PIDs             uint64  `json:"pids"`
}

// cpuStats is the part of a stats CPU block that is decoded.
type cpuStats struct {
	CPUUsage struct {
		TotalUsage  uint64   `json:"total_usage"`
		PercpuUsage []uint64 `json:"percpu_usage"`
	} `json:"cpu_usage"`
	SystemCPUUsage uint64 `json:"system_cpu_usage"`
	OnlineCPUs     uint32 `json:"online_cpus"`
}

// statsDoc is the only part of the stats document that is ever decoded.
type statsDoc struct {
	CPUStats    cpuStats `json:"cpu_stats"`
	PreCPUStats cpuStats `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64 `json:"usage"`
		Limit uint64 `json:"limit"`
		Stats struct {
			InactiveFile      uint64 `json:"inactive_file"`       // cgroup v2
			TotalInactiveFile uint64 `json:"total_inactive_file"` // cgroup v1
		} `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
	BlkioStats struct {
		IOServiceBytesRecursive []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
	PidsStats struct {
		Current uint64 `json:"current"`
	} `json:"pids_stats"`
}

// Usage returns one sample of the container's CPU, memory, network, block IO
// and process count. The container is resolved by exact name first, then
// queried by its full ID.
func (e *Engine) Usage(ctx context.Context, container string) (Usage, error) {
	in, err := e.inspect(ctx, container)
	if err != nil {
		return Usage{}, err
	}
	resp, cancel, err := e.get(ctx, "/containers/"+in.ID+"/stats", "stream=false")
	if err != nil {
		return Usage{}, err
	}
	defer cancel()
	defer resp.Body.Close()
	var d statsDoc
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxStatsBody)).Decode(&d); err != nil {
		return Usage{}, errors.New("docker: malformed stats response")
	}
	return buildUsage(&d), nil
}

func buildUsage(d *statsDoc) Usage {
	u := Usage{PIDs: d.PidsStats.Current}

	// Same formula as "docker stats".
	cpuDelta := float64(d.CPUStats.CPUUsage.TotalUsage) - float64(d.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(d.CPUStats.SystemCPUUsage) - float64(d.PreCPUStats.SystemCPUUsage)
	online := float64(d.CPUStats.OnlineCPUs)
	if online == 0 {
		online = float64(len(d.CPUStats.CPUUsage.PercpuUsage))
	}
	if d.PreCPUStats.SystemCPUUsage > 0 && sysDelta > 0 && cpuDelta > 0 && online > 0 {
		u.CPUPercent = round2(cpuDelta / sysDelta * online * 100)
	}

	m := d.MemoryStats
	cache := m.Stats.TotalInactiveFile
	if m.Stats.InactiveFile > 0 {
		cache = m.Stats.InactiveFile
	}
	u.MemoryUsedBytes = m.Usage
	if cache < m.Usage {
		u.MemoryUsedBytes = m.Usage - cache
	}
	u.MemoryLimitBytes = m.Limit
	if m.Limit > 0 {
		u.MemoryPercent = round2(float64(u.MemoryUsedBytes) / float64(m.Limit) * 100)
	}

	for _, n := range d.Networks {
		u.NetRxBytes = satAdd(u.NetRxBytes, n.RxBytes)
		u.NetTxBytes = satAdd(u.NetTxBytes, n.TxBytes)
	}
	for _, b := range d.BlkioStats.IOServiceBytesRecursive {
		switch strings.ToLower(b.Op) {
		case "read":
			u.BlockReadBytes = satAdd(u.BlockReadBytes, b.Value)
		case "write":
			u.BlockWriteBytes = satAdd(u.BlockWriteBytes, b.Value)
		}
	}
	return u
}

func satAdd(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
