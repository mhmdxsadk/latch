// Package ports lists the sockets that are listening on this host and the
// program that owns each.
//
// It only reads /proc: the socket tables (net/tcp, tcp6, udp, udp6), the
// file descriptor links of each process (to find the owner of a socket), and
// each owner's comm and cgroup files. Every read is bounded. Nothing is
// executed. Files that cannot be read (other users' processes when not root)
// leave the owner unknown rather than failing the call.
package ports

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/mhmdxsadk/latch/internal/host"
	"github.com/mhmdxsadk/latch/internal/redact"
	"github.com/mhmdxsadk/latch/internal/textsafe"
)

const (
	maxTableBytes   = 8 << 20 // per socket table
	maxListeners    = 8192
	maxFDsPerProc   = 65536
	maxSmallFile    = 64 << 10 // comm, cgroup
	maxProgramBytes = 64
)

// Listener is one listening socket and what is known about its owner.
type Listener struct {
	Protocol string // tcp, tcp6, udp or udp6
	Address  string
	Port     uint16
	PID      int    // 0 if the owner could not be determined
	Program  string // from /proc/PID/comm
	// Unit is the systemd unit the owner runs in; "" if none or the owner
	// is a container.
	Unit string
	// Units are all systemd units in the owner's cgroup path, for exclusion
	// checks. Unit is one of them.
	Units []string
	// ContainerID is the full Docker container ID of the owner, if any.
	ContainerID string
}

// Scanner is the interface the MCP layer uses; tests substitute a fake.
type Scanner interface {
	// Listening returns the listening sockets, sorted. The caller must
	// filter the result through policy and the excluded-container snapshot.
	Listening(ctx context.Context) ([]Listener, error)
}

// Proc reads from a procfs mounted at Root (default /proc).
type Proc struct{ Root string }

var _ Scanner = Proc{}

func (p Proc) path(parts ...string) string {
	root := p.Root
	if root == "" {
		root = "/proc"
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

type socket struct {
	proto string
	addr  netip.Addr
	port  uint16
	inode uint64
}

var protoTables = []struct{ proto, file string }{
	{"tcp", "net/tcp"}, {"tcp6", "net/tcp6"}, {"udp", "net/udp"}, {"udp6", "net/udp6"},
}

// Listening implements Scanner.
func (p Proc) Listening(ctx context.Context) ([]Listener, error) {
	var socks []socket
	for i, t := range protoTables {
		b, err := readBounded(p.path(t.file), maxTableBytes)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				if i == 0 {
					return nil, host.ErrUnsupported // no procfs
				}
				continue // for example IPv6 disabled
			}
			return nil, errors.New("ports: reading socket table failed")
		}
		socks = append(socks, parseTable(t.proto, b)...)
		if len(socks) > maxListeners {
			socks = socks[:maxListeners]
			break
		}
	}
	owners, err := p.findOwners(ctx, socks)
	if err != nil {
		return nil, err
	}
	info := map[int]procInfo{}
	out := make([]Listener, 0, len(socks))
	for _, s := range socks {
		l := Listener{Protocol: s.proto, Address: s.addr.String(), Port: s.port, Units: []string{}}
		if pid, ok := owners[s.inode]; ok {
			pi, seen := info[pid]
			if !seen {
				pi = p.readProc(pid)
				info[pid] = pi
			}
			l.PID, l.Program, l.Unit, l.ContainerID = pid, pi.program, pi.unit, pi.containerID
			l.Units = pi.units
		}
		out = append(out, l)
	}
	order := map[string]int{"tcp": 0, "tcp6": 1, "udp": 2, "udp6": 3}
	slices.SortStableFunc(out, func(a, b Listener) int {
		if c := order[a.Protocol] - order[b.Protocol]; c != 0 {
			return c
		}
		if a.Port != b.Port {
			return int(a.Port) - int(b.Port)
		}
		return strings.Compare(a.Address, b.Address)
	})
	return out, nil
}

// readBounded reads at most limit bytes of a file.
func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // path is built from the procfs root and fixed names
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

// parseTable parses a /proc/net socket table, keeping listening sockets:
// TCP in state LISTEN (0A) and UDP bound and not connected (state 07 with no
// remote). Malformed lines are skipped.
func parseTable(proto string, b []byte) []socket {
	var out []socket
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	sc.Scan() // header
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 {
			continue
		}
		local, remote, state := f[1], f[2], f[3]
		addr, port, ok := parseEndpoint(local)
		if !ok {
			continue
		}
		if strings.HasPrefix(proto, "tcp") {
			if state != "0A" {
				continue
			}
		} else {
			raddr, rport, ok := parseEndpoint(remote)
			if state != "07" || !ok || rport != 0 || !raddr.IsUnspecified() {
				continue
			}
		}
		inode, err := strconv.ParseUint(f[9], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, socket{proto: proto, addr: addr, port: port, inode: inode})
	}
	return out
}

// parseEndpoint parses "0100007F:0016": the address as hex words in the
// host's byte order (little endian on x86_64 and arm64), then the port.
func parseEndpoint(s string) (netip.Addr, uint16, bool) {
	h, p, ok := strings.Cut(s, ":")
	if !ok || len(p) != 4 {
		return netip.Addr{}, 0, false
	}
	port, err := strconv.ParseUint(p, 16, 16)
	if err != nil {
		return netip.Addr{}, 0, false
	}
	if len(h) != 8 && len(h) != 32 {
		return netip.Addr{}, 0, false
	}
	var raw [16]byte
	n := len(h) / 2
	for i := 0; i < len(h); i += 8 {
		w, err := strconv.ParseUint(h[i:i+8], 16, 32)
		if err != nil {
			return netip.Addr{}, 0, false
		}
		o := i / 2
		binary.LittleEndian.PutUint32(raw[o:], uint32(w))
	}
	if n == 4 {
		return netip.AddrFrom4([4]byte(raw[:4])), uint16(port), true
	}
	return netip.AddrFrom16(raw), uint16(port), true
}

// findOwners maps socket inodes to the lowest process ID holding them. It
// stops as soon as every inode has an owner.
func (p Proc) findOwners(ctx context.Context, socks []socket) (map[uint64]int, error) {
	want := make(map[uint64]bool, len(socks))
	for _, s := range socks {
		want[s.inode] = true
	}
	owners := map[uint64]int{}
	if len(want) == 0 {
		return owners, nil
	}
	dir, err := os.Open(p.path())
	if err != nil {
		return owners, nil //nolint:nilerr // owners stay unknown
	}
	defer dir.Close()
	names, _ := dir.Readdirnames(-1)
	var pids []int
	for _, n := range names {
		if pid, err := strconv.Atoi(n); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fdPath := p.path(strconv.Itoa(pid), "fd")
		d, err := os.Open(fdPath) //nolint:gosec // built from the procfs root and a numeric pid
		if err != nil {
			continue
		}
		fds, _ := d.Readdirnames(maxFDsPerProc)
		d.Close()
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdPath, fd))
			if err != nil {
				continue
			}
			rest, ok := strings.CutPrefix(target, "socket:[")
			if !ok {
				continue
			}
			inode, err := strconv.ParseUint(strings.TrimSuffix(rest, "]"), 10, 64)
			if err != nil || !want[inode] {
				continue
			}
			if _, done := owners[inode]; !done {
				owners[inode] = pid
			}
		}
		if len(owners) == len(want) {
			break
		}
	}
	return owners, nil
}

type procInfo struct {
	program     string
	unit        string
	units       []string
	containerID string
}

func (p Proc) readProc(pid int) procInfo {
	id := strconv.Itoa(pid)
	pi := procInfo{units: []string{}}
	if b, err := readBounded(p.path(id, "comm"), maxSmallFile); err == nil {
		prog := redact.Text(textsafe.Line(strings.TrimSpace(string(b))))
		pi.program = textsafe.Head(prog, maxProgramBytes)
	}
	if b, err := readBounded(p.path(id, "cgroup"), maxSmallFile); err == nil {
		pi.units, pi.unit, pi.containerID = parseCgroup(string(b))
	}
	return pi
}

var (
	unitElem = regexp.MustCompile(`^[A-Za-z0-9:_.\\@-]{1,200}\.(service|socket|timer|mount|path|scope|slice|swap|automount|target)$`)
	// dockerScope is the cgroup of a container under the systemd cgroup driver.
	dockerScope = regexp.MustCompile(`^docker-([0-9a-f]{64})\.scope$`)
	hex64       = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// parseCgroup extracts systemd units and a Docker container ID from the
// contents of /proc/PID/cgroup. unit is the innermost service, scope, socket,
// timer, mount, path, swap or automount; units lists every unit in the path.
func parseCgroup(s string) (units []string, unit, containerID string) {
	units = []string{}
	for line := range strings.SplitSeq(s, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		prev := ""
		for elem := range strings.SplitSeq(parts[2], "/") {
			switch {
			case elem == "":
				continue
			case dockerScope.MatchString(elem):
				if containerID == "" {
					containerID = dockerScope.FindStringSubmatch(elem)[1]
				}
			case prev == "docker" && hex64.MatchString(elem):
				if containerID == "" {
					containerID = elem
				}
			case unitElem.MatchString(elem):
				if !slices.Contains(units, elem) {
					units = append(units, elem)
				}
				if !strings.HasSuffix(elem, ".slice") && !strings.HasSuffix(elem, ".target") && (parts[0] == "0" || parts[1] == "name=systemd") {
					unit = elem
				}
			}
			prev = elem
		}
	}
	if containerID != "" {
		unit = ""
	}
	return units, unit, containerID
}
