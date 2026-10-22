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

	"github.com/mhmdxsadk/latch/internal/config"
)

// Kind identifies a class of resource.
type Kind string

const (
	KindDocker  Kind = "docker"
	KindSystemd Kind = "systemd"
	KindDisk    Kind = "disk"
	KindHTTP    Kind = "http"
	KindDNS     Kind = "dns"
)

var (
	// ErrInvalidName means the supplied name is syntactically unacceptable.
	ErrInvalidName = errors.New("invalid resource name")
	// ErrNotAllowed means the name is well-formed but not configured.
	ErrNotAllowed = errors.New("resource not allowed")
)

// Policy is an immutable view over a validated configuration.
type Policy struct {
	docker  map[string]config.DockerService
	units   map[string]string
	disks   map[string]config.Disk
	http    map[string]config.HTTPTarget
	dns     map[string]config.DNSTarget
	limits  config.Limits
	ordered map[Kind][]string
}

// New builds a Policy from a config that has already passed Validate.
func New(c *config.Config) *Policy {
	p := &Policy{
		docker:  map[string]config.DockerService{},
		units:   map[string]string{},
		disks:   map[string]config.Disk{},
		http:    map[string]config.HTTPTarget{},
		dns:     map[string]config.DNSTarget{},
		limits:  c.Limits,
		ordered: map[Kind][]string{},
	}
	for _, s := range c.Docker.Services {
		p.docker[s.Name] = s
		p.ordered[KindDocker] = append(p.ordered[KindDocker], s.Name)
	}
	for _, u := range c.Systemd.Units {
		p.units[u] = u
		p.ordered[KindSystemd] = append(p.ordered[KindSystemd], u)
	}
	for _, d := range c.Host.Disks {
		p.disks[d.Name] = d
		p.ordered[KindDisk] = append(p.ordered[KindDisk], d.Name)
	}
	for _, t := range c.Health.HTTP {
		p.http[t.Name] = t
		p.ordered[KindHTTP] = append(p.ordered[KindHTTP], t.Name)
	}
	for _, t := range c.Health.DNS {
		p.dns[t.Name] = t
		p.ordered[KindDNS] = append(p.ordered[KindDNS], t.Name)
	}
	return p
}

// Names returns the configured names for a kind, in configuration order.
func (p *Policy) Names(k Kind) []string {
	return append([]string(nil), p.ordered[k]...)
}

// DockerService resolves an agent-supplied service name.
func (p *Policy) DockerService(name string) (config.DockerService, error) {
	if !config.ValidResourceName(name) {
		return config.DockerService{}, ErrInvalidName
	}
	s, ok := p.docker[name]
	if !ok {
		return config.DockerService{}, ErrNotAllowed
	}
	return s, nil
}

// SystemdUnit resolves an agent-supplied unit name. Unit names are matched
// verbatim against the allowlist; nothing is normalised or suffixed, so
// "ssh" does not silently become "ssh.service".
func (p *Policy) SystemdUnit(name string) (string, error) {
	if len(name) == 0 || len(name) > 255 {
		return "", ErrInvalidName
	}
	u, ok := p.units[name]
	if !ok {
		return "", ErrNotAllowed
	}
	return u, nil
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

// HTTPTarget resolves an agent-supplied HTTP check name.
func (p *Policy) HTTPTarget(name string) (config.HTTPTarget, error) {
	if !config.ValidResourceName(name) {
		return config.HTTPTarget{}, ErrInvalidName
	}
	t, ok := p.http[name]
	if !ok {
		return config.HTTPTarget{}, ErrNotAllowed
	}
	return t, nil
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
