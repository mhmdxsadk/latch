// Package policy is the single place where agent-supplied names are turned
// into concrete infrastructure identifiers.
//
// Everything an agent sends is untrusted. Integrations (docker, systemd, ...)
// never receive a raw agent string: they receive a value returned by Policy,
// which can only come from the operator's configuration.
package policy

import (
	"errors"
	"fmt"
	"slices"

	"github.com/mhmdxsadk/latch/internal/config"
)

// Kind identifies a class of resource.
type Kind string

const (
	KindDisk Kind = "disk"
	KindDNS  Kind = "dns"
)

var (
	// ErrInvalidName means the supplied name is syntactically unacceptable.
	ErrInvalidName = errors.New("invalid resource name")
	// ErrNotAllowed means the name is well-formed but excluded or not configured.
	ErrNotAllowed = errors.New("resource not allowed")
)

// Policy is an immutable view over a validated configuration.
//
// Docker containers and systemd units are open by default: any well-formed name
// is allowed unless it matches an exclude pattern. Disks and DNS checks are
// still an explicit allowlist, because they need a path or hostname.
type Policy struct {
	dockerExclude  []string
	systemdExclude []string
	dockerApprove  []string
	systemdApprove []string
	disks          map[string]config.Disk
	dns            map[string]config.DNSTarget
	limits         config.Limits
	ordered        map[Kind][]string
}

// BuiltinApproveUnits are units whose every change needs the owner's
// approval, whatever the config says: losing one of them can cut the owner
// off from the server or take Latch itself down.
var BuiltinApproveUnits = []string{
	"docker.service",
	"ssh.service",
	"sshd.service",
	"systemd-networkd.service",
	"networking.service",
	"latch.service",
}

// New builds a Policy from a config that has already passed Validate.
func New(c *config.Config) *Policy {
	p := &Policy{
		dockerExclude:  slices.Clone(c.Docker.Exclude),
		systemdExclude: slices.Clone(c.Systemd.Exclude),
		dockerApprove:  slices.Clone(c.Docker.Approve),
		systemdApprove: append(slices.Clone(BuiltinApproveUnits), c.Systemd.Approve...),
		disks:          map[string]config.Disk{},
		dns:            map[string]config.DNSTarget{},
		limits:         c.Limits,
		ordered:        map[Kind][]string{},
	}
	for _, d := range c.Host.Disks {
		p.disks[d.Name] = d
		p.ordered[KindDisk] = append(p.ordered[KindDisk], d.Name)
	}
	for _, t := range c.Health.DNS {
		p.dns[t.Name] = t
		p.ordered[KindDNS] = append(p.ordered[KindDNS], t.Name)
	}
	return p
}

// Names returns the configured names for a kind (disks or DNS checks), in
// configuration order.
func (p *Policy) Names(k Kind) []string {
	return append([]string(nil), p.ordered[k]...)
}

// Container validates an agent-supplied container name and returns it if it
// is not excluded. The name is matched exactly, never as an ID prefix.
func (p *Policy) Container(name string) (string, error) {
	if !config.ValidContainerName(name) {
		return "", ErrInvalidName
	}
	if !p.ContainerVisible(name) {
		return "", ErrNotAllowed
	}
	return name, nil
}

// ContainerVisible reports whether a container is not excluded. It is used to
// filter listings; lookups go through Container.
func (p *Policy) ContainerVisible(name string) bool {
	return !config.MatchesAny(p.dockerExclude, name)
}

// SystemdUnit validates an agent-supplied full unit name and returns it if it
// is not excluded. Names are matched verbatim; nothing is normalised or
// suffixed, so "ssh" does not silently become "ssh.service".
func (p *Policy) SystemdUnit(name string) (string, error) {
	if !config.ValidUnitName(name) {
		return "", ErrInvalidName
	}
	if !p.UnitVisible(name) {
		return "", ErrNotAllowed
	}
	return name, nil
}

// UnitVisible reports whether a unit is not excluded. It is used to filter
// listings; lookups go through SystemdUnit.
func (p *Policy) UnitVisible(name string) bool {
	return !config.MatchesAny(p.systemdExclude, name)
}

// ContainerNeedsApproval reports whether every change to the container, even
// a restart, must wait for the owner's approval (docker.approve).
func (p *Policy) ContainerNeedsApproval(name string) bool {
	return config.MatchesAny(p.dockerApprove, name)
}

// UnitNeedsApproval reports whether every change to the unit, even a restart,
// must wait for the owner's approval (systemd.approve or the built-in list).
func (p *Policy) UnitNeedsApproval(name string) bool {
	return config.MatchesAny(p.systemdApprove, name)
}

// Disk resolves an agent-supplied disk name.
func (p *Policy) Disk(name string) (config.Disk, error) {
	if !config.ValidResourceName(name) {
		return config.Disk{}, ErrInvalidName
	}
	d, ok := p.disks[name]
	if !ok {
		return config.Disk{}, ErrNotAllowed
	}
	return d, nil
}

// DNSTarget resolves an agent-supplied DNS check name.
func (p *Policy) DNSTarget(name string) (config.DNSTarget, error) {
	if !config.ValidResourceName(name) {
		return config.DNSTarget{}, ErrInvalidName
	}
	t, ok := p.dns[name]
	if !ok {
		return config.DNSTarget{}, ErrNotAllowed
	}
	return t, nil
}

// LogLimits clamps a requested log window to the configured maximum.
// A request for zero (or fewer) lines yields a safe default; a request above
// the cap is rejected rather than silently truncated so the agent knows.
func (p *Policy) LogLimits(requestedLines int) (lines, maxBytes int, err error) {
	switch {
	case requestedLines < 0:
		return 0, 0, fmt.Errorf("lines must not be negative")
	case requestedLines == 0:
		lines = min(100, p.limits.MaxLogLines)
	case requestedLines > p.limits.MaxLogLines:
		return 0, 0, fmt.Errorf("lines exceeds the maximum of %d", p.limits.MaxLogLines)
	default:
		lines = requestedLines
	}
	return lines, p.limits.MaxLogBytes, nil
}
