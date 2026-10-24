package docker

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mhmdxsadk/latch/internal/redact"
)

const (
	maxLabels      = 64
	maxMounts      = 64
	maxPorts       = 64
	maxNetworks    = 16
	maxSettingText = 512
)

// Settings is the projected configuration of a container. It never contains
// environment variables or the command line. Label values and the health
// check command are redacted.
type Settings struct {
	Image         string            `json:"image"`
	RestartPolicy string            `json:"restart_policy"` // "no", "always", "unless-stopped", "on-failure[:max]"
	NetworkMode   string            `json:"network_mode"`
	Ports         []Port            `json:"ports"` // published ports only
	Mounts        []Mount           `json:"mounts"`
	Networks      []Network         `json:"networks"`
	Labels        map[string]string `json:"labels"` // compose labels show where the container is defined
	HealthCheck   *HealthCheck      `json:"health_check,omitempty"`
	Truncated     bool              `json:"truncated"` // true if a list or text was cut to its limit
}

// Port is one published port.
type Port struct {
	Container string `json:"container"` // for example "3000/tcp"
	HostIP    string `json:"host_ip"`
	HostPort  string `json:"host_port"`
}

// Mount is one bind mount or volume.
type Mount struct {
	Type        string `json:"type"` // bind, volume, tmpfs
	Source      string `json:"source"`
	Destination string `json:"destination"`
	ReadOnly    bool   `json:"read_only"`
}

// Network is one network the container is attached to.
type Network struct {
	Name        string `json:"name"`
	IPAddress   string `json:"ip_address,omitempty"`
	IPv6Address string `json:"ipv6_address,omitempty"`
}

// HealthCheck is the configured health check, which is where the image or the
// compose file defines it.
type HealthCheck struct {
	Kind        string `json:"kind"` // CMD, CMD-SHELL or NONE
	Command     string `json:"command"`
	Interval    string `json:"interval,omitempty"`
	Timeout     string `json:"timeout,omitempty"`
	StartPeriod string `json:"start_period,omitempty"`
	Retries     int    `json:"retries,omitempty"`
}

// Settings returns the configuration of the container.
func (e *Engine) Settings(ctx context.Context, container string) (Settings, error) {
	in, err := e.inspect(ctx, container)
	if err != nil {
		return Settings{}, err
	}
	var truncated bool
	text := func(s string) string {
		s = sanitize(s)
		if len(s) > maxSettingText {
			truncated = true
			return headBytes(s, maxSettingText)
		}
		return s
	}
	st := Settings{
		Image:       text(in.Config.Image),
		NetworkMode: text(in.HostConfig.NetworkMode),
		Ports:       []Port{},
		Mounts:      []Mount{},
		Networks:    []Network{},
		Labels:      map[string]string{},
	}
	st.RestartPolicy = text(in.HostConfig.RestartPolicy.Name)
	if st.RestartPolicy == "" {
		st.RestartPolicy = "no"
	}
	if n := in.HostConfig.RestartPolicy.MaximumRetryCount; n > 0 {
		st.RestartPolicy += ":" + strconv.Itoa(n)
	}

	for _, key := range sortedKeys(in.NetworkSettings.Ports) {
		for _, b := range in.NetworkSettings.Ports[key] {
			if len(st.Ports) == maxPorts {
				truncated = true
				break
			}
			st.Ports = append(st.Ports, Port{Container: text(key), HostIP: text(b.HostIp), HostPort: text(b.HostPort)})
		}
	}
	for _, m := range in.Mounts {
		if len(st.Mounts) == maxMounts {
			truncated = true
			break
		}
		src := m.Source
		if m.Type == "volume" && m.Name != "" {
			src = m.Name
		}
		st.Mounts = append(st.Mounts, Mount{Type: text(m.Type), Source: text(src), Destination: text(m.Destination), ReadOnly: !m.RW})
	}
	for _, name := range sortedKeys(in.NetworkSettings.Networks) {
		if len(st.Networks) == maxNetworks {
			truncated = true
			break
		}
		n := in.NetworkSettings.Networks[name]
		st.Networks = append(st.Networks, Network{Name: text(name), IPAddress: text(n.IPAddress), IPv6Address: text(n.GlobalIPv6Address)})
	}
	for _, raw := range sortedKeys(in.Config.Labels) {
		if len(st.Labels) == maxLabels {
			truncated = true
			break
		}
		k := sanitize(raw)
		st.Labels[text(k)] = text(redact.Value(k, sanitize(in.Config.Labels[raw])))
	}
	if h := in.Config.Healthcheck; h != nil && len(h.Test) > 0 {
		hc := &HealthCheck{
			Kind:        text(h.Test[0]),
			Command:     text(redact.Text(sanitize(strings.Join(h.Test[1:], " ")))),
			Interval:    duration(h.Interval),
			Timeout:     duration(h.Timeout),
			StartPeriod: duration(h.StartPeriod),
			Retries:     h.Retries,
		}
		st.HealthCheck = hc
	}
	st.Truncated = truncated
	return st, nil
}

// duration formats Docker's nanosecond count; zero means "not set" and the
// daemon default applies.
func duration(ns int64) string {
	if ns <= 0 {
		return ""
	}
	return time.Duration(ns).String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
