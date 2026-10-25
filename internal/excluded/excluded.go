// Package excluded tracks what Latch knows about excluded Docker containers
// (their addresses, published ports and bind-mounted folders), so tools that
// are not about containers, such as network checks and file reads, can keep
// away from them too.
//
// The snapshot is cached briefly. If Docker cannot be asked, the last good
// snapshot is used; if there has never been one, Get fails and callers must
// refuse (fail closed).
package excluded

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/mhmdxsadk/latch/internal/docker"
	"github.com/mhmdxsadk/latch/internal/policy"
	"github.com/mhmdxsadk/latch/internal/ports"
)

// TTL is how long a snapshot is reused before Docker is asked again.
const TTL = 10 * time.Second

// ErrUnknown means no snapshot could be taken yet; callers must refuse.
var ErrUnknown = errors.New("excluded containers unknown")

// Snapshot describes the excluded containers at one point in time.
type Snapshot struct {
	// Addrs are the containers' network addresses (IPv4 and IPv6).
	Addrs []netip.Addr
	// Ports are host ports published by excluded containers, plus the ports
	// that excluded containers on the host network listen on.
	Ports []uint16
	// Folders are the clean absolute host paths bind-mounted into excluded
	// containers, including local volumes that bind a host folder. Other
	// volumes live under /var/lib/docker, which is always blocked.
	Folders []string
}

// HasAddr reports whether a belongs to an excluded container. IPv4-mapped
// IPv6 addresses are compared as IPv4.
func (s Snapshot) HasAddr(a netip.Addr) bool {
	return slices.Contains(s.Addrs, a.Unmap())
}

// HasPort reports whether port is published by an excluded container.
func (s Snapshot) HasPort(port uint16) bool { return slices.Contains(s.Ports, port) }

// Tracker builds and caches snapshots.
type Tracker struct {
	Docker docker.Client
	Policy *policy.Policy
	// Ports finds the listeners of excluded containers on the host network.
	// Required when such a container exists (otherwise Get fails closed).
	Ports ports.Scanner
	// Now is the clock; nil means time.Now. Tests set it.
	Now func() time.Time

	mu    sync.Mutex
	snap  Snapshot
	taken time.Time
	ok    bool
}

func (t *Tracker) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Get returns a snapshot no older than TTL when Docker answers, otherwise the
// last good one, otherwise ErrUnknown.
func (t *Tracker) Get(ctx context.Context) (Snapshot, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ok && t.now().Sub(t.taken) < TTL {
		return t.snap, nil
	}
	snap, err := t.build(ctx)
	if err != nil {
		if t.ok {
			return t.snap, nil
		}
		return Snapshot{}, ErrUnknown
	}
	t.snap, t.taken, t.ok = snap, t.now(), true
	return snap, nil
}

func (t *Tracker) build(ctx context.Context) (Snapshot, error) {
	names, err := t.Docker.List(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	var s Snapshot
	hostNet := map[string]bool{} // IDs of excluded containers on the host network
	for _, name := range names {
		if t.Policy.ContainerVisible(name) {
			continue
		}
		x, err := t.Docker.Exposure(ctx, name)
		if errors.Is(err, docker.ErrNotFound) {
			continue // removed since List
		}
		if err != nil {
			// A container we cannot inspect could be hiding anything.
			return Snapshot{}, err
		}
		for _, ip := range x.Addrs {
			if a, err := netip.ParseAddr(ip); err == nil {
				s.Addrs = append(s.Addrs, a.Unmap())
			}
		}
		s.Ports = append(s.Ports, x.HostPorts...)
		s.Folders = append(s.Folders, x.Folders...)
		if x.NetworkMode == "host" {
			hostNet[x.ID] = true
		}
	}
	if len(hostNet) > 0 {
		// A container on the host network listens on the host's own ports
		// without publishing them, so take its ports from the socket table.
		if t.Ports == nil {
			return Snapshot{}, errors.New("excluded: host-network container but no port scanner")
		}
		ls, err := t.Ports.Listening(ctx)
		if err != nil {
			return Snapshot{}, err
		}
		for _, l := range ls {
			if hostNet[l.ContainerID] {
				s.Ports = append(s.Ports, l.Port)
			}
		}
	}
	slices.SortFunc(s.Addrs, func(a, b netip.Addr) int { return a.Compare(b) })
	s.Addrs = slices.Compact(s.Addrs)
	slices.Sort(s.Ports)
	s.Ports = slices.Compact(s.Ports)
	slices.Sort(s.Folders)
	s.Folders = slices.Compact(s.Folders)
	return s, nil
}
