// Package host reports basic system metrics (load, memory, disk usage).
//
// It only reads fixed kernel interfaces (/proc/loadavg, /proc/meminfo,
// /proc/self/mounts, statfs) and never accepts a path from a caller.
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

// Disk reports the usage of one mounted filesystem in bytes and inodes.
type Disk struct {
	Mount       string `json:"mount"`
	Type        string `json:"type"`
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
	// Disks lists the mounted filesystems that hold data, in mount order.
	Disks(ctx context.Context) ([]Disk, error)
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

// MaxDisks bounds the Disks list.
const MaxDisks = 64

// pseudoFS are filesystem types that hold no data of their own: kernel
// interfaces, memory-backed mounts and container layers.
var pseudoFS = map[string]bool{
	"autofs": true, "binfmt_misc": true, "bpf": true, "cgroup": true, "cgroup2": true,
	"configfs": true, "debugfs": true, "devpts": true, "devtmpfs": true, "efivarfs": true,
	"fusectl": true, "fuse.lxcfs": true, "fuse.portal": true, "hugetlbfs": true,
	"mqueue": true, "nsfs": true, "overlay": true, "proc": true, "pstore": true,
	"ramfs": true, "rpc_pipefs": true, "securityfs": true, "selinuxfs": true,
	"squashfs": true, "sysfs": true, "tmpfs": true, "tracefs": true,
}

// Disks lists every mounted filesystem that holds data, skipping pseudo
// filesystems and further mounts of a device already listed (bind mounts).
func (s System) Disks(context.Context) ([]Disk, error) {
	b, err := os.ReadFile(s.proc("self/mounts"))
	if err != nil {
		return nil, unsupportedOr(err)
	}
	out := []Disk{}
	seen := map[string]bool{}
	for _, m := range ParseMounts(string(b)) {
		if pseudoFS[m.Type] || seen[m.Source] || len(out) >= MaxDisks {
			continue
		}
		d, err := statfs(m.Mount)
		if err != nil {
			continue // gone, or not readable: leave it out
		}
		seen[m.Source] = true
		d.Mount, d.Type = m.Mount, m.Type
		out = append(out, d)
	}
	return out, nil
}

// Mount is one line of /proc/self/mounts.
type Mount struct {
	Source string
	Mount  string
	Type   string
}

// ParseMounts parses /proc/self/mounts, decoding the octal escapes the
// kernel uses for spaces and other special characters in paths.
func ParseMounts(s string) []Mount {
	var out []Mount
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		out = append(out, Mount{Source: unescapeMount(f[0]), Mount: unescapeMount(f[1]), Type: f[2]})
	}
	return out
}

func unescapeMount(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				sb.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

func statfs(path string) (Disk, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return Disk{}, fmt.Errorf("statfs: %w", err)
	}
	if st.Bsize <= 0 {
		return Disk{}, errors.New("statfs: invalid block size")
	}
	bs := uint64(st.Bsize) //nolint:gosec // checked positive above
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
