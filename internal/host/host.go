// Package host reports basic system metrics (load, memory, disk usage).
//
// It only reads fixed kernel interfaces (/proc/loadavg, /proc/meminfo, statfs)
// and never accepts a path from a caller: disk paths come from policy.
package host

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Load is the system load average.
type Load struct {
	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`
	CPUs   int     `json:"cpus"`
}

// Memory reports memory in bytes.
type Memory struct {
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	UsedBytes      uint64 `json:"used_bytes"`
	SwapTotalBytes uint64 `json:"swap_total_bytes"`
	SwapFreeBytes  uint64 `json:"swap_free_bytes"`
}

// Disk reports filesystem usage in bytes and inodes.
type Disk struct {
	TotalBytes  uint64 `json:"total_bytes"`
	FreeBytes   uint64 `json:"free_bytes"`
	UsedBytes   uint64 `json:"used_bytes"`
	InodesTotal uint64 `json:"inodes_total"`
	InodesFree  uint64 `json:"inodes_free"`
}

// Provider is the host metrics interface; tests substitute a fake.
type Provider interface {
	Load(ctx context.Context) (Load, error)
	Memory(ctx context.Context) (Memory, error)
	// Disk takes a path that has already been resolved by policy.
	Disk(ctx context.Context, path string) (Disk, error)
}

// ErrUnsupported is returned where a metric is unavailable on this OS.
var ErrUnsupported = errors.New("not supported on this platform")

// System reads metrics from the running kernel. ProcRoot defaults to /proc and
// exists so parsing can be tested against fixture files.
type System struct{ ProcRoot string }

func (s System) proc(name string) string {
	root := s.ProcRoot
	if root == "" {
		root = "/proc"
	}
	return filepath.Join(root, name)
}

func (s System) Load(context.Context) (Load, error) {
	b, err := os.ReadFile(s.proc("loadavg"))
	if err != nil {
		return Load{}, unsupportedOr(err)
	}
	l, err := ParseLoadavg(string(b))
	l.CPUs = runtime.NumCPU()
	return l, err
}

func (s System) Memory(context.Context) (Memory, error) {
	f, err := os.Open(s.proc("meminfo"))
	if err != nil {
		return Memory{}, unsupportedOr(err)
	}
	defer f.Close()
	return ParseMeminfo(bufio.NewScanner(f))
}

func (s System) Disk(_ context.Context, path string) (Disk, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return Disk{}, fmt.Errorf("statfs: %w", err)
	}
	bs := uint64(st.Bsize)
	total := st.Blocks * bs
	free := st.Bavail * bs
	return Disk{
		TotalBytes:  total,
		FreeBytes:   free,
		UsedBytes:   total - st.Bfree*bs,
		InodesTotal: st.Files,
		InodesFree:  st.Ffree,
	}, nil
}

func unsupportedOr(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return ErrUnsupported
	}
	return err
}

// ParseLoadavg parses the contents of /proc/loadavg.
func ParseLoadavg(s string) (Load, error) {
	f := strings.Fields(s)
	if len(f) < 3 {
		return Load{}, errors.New("malformed loadavg")
	}
	var l Load
	var err error
	for i, dst := range []*float64{&l.Load1, &l.Load5, &l.Load15} {
		if *dst, err = strconv.ParseFloat(f[i], 64); err != nil {
			return Load{}, errors.New("malformed loadavg")
		}
	}
	return l, nil
}

// ParseMeminfo parses /proc/meminfo lines ("Key:   123 kB").
func ParseMeminfo(sc *bufio.Scanner) (Memory, error) {
	vals := map[string]uint64{}
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		n, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			continue
		}
		if len(f) > 1 && strings.EqualFold(f[1], "kB") {
			n *= 1024
		}
		vals[key] = n
	}
	if err := sc.Err(); err != nil {
		return Memory{}, err
	}
	total, ok1 := vals["MemTotal"]
	avail, ok2 := vals["MemAvailable"]
	if !ok1 || !ok2 || avail > total {
		return Memory{}, errors.New("malformed meminfo")
	}
	return Memory{
		TotalBytes:     total,
		AvailableBytes: avail,
		UsedBytes:      total - avail,
		SwapTotalBytes: vals["SwapTotal"],
		SwapFreeBytes:  vals["SwapFree"],
	}, nil
}
