// Package config loads and validates Latch's configuration.
//
// The configuration only declares WHICH resources Latch may expose. It has no
// way to express commands, arguments, or paths to read: every field is either a
// resource name, a fixed identifier (container, unit, mount point), or a bound.
// Unknown keys are rejected so a typo can never silently widen or drop policy.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// MaxConfigBytes bounds the size of a config file we are willing to parse.
	MaxConfigBytes = 1 << 20

	// HardMaxLogLines and HardMaxLogBytes are absolute ceilings that no
	// configuration can exceed.
	HardMaxLogLines = 1000
	HardMaxLogBytes = 256 << 10

	DefaultMaxLogLines = 200
	DefaultMaxLogBytes = 64 << 10

	DefaultDockerSocket = "/var/run/docker.sock"
	DefaultCheckTimeout = 5 * time.Second
	MaxCheckTimeout     = 30 * time.Second
)

var (
	// resourceName is the shape of every agent-visible resource name.
	resourceName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	// containerName matches Docker's own container name rule.
	containerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	// unitName matches a fully qualified systemd unit name. Template
	// instances ("foo@bar.service") are allowed; path separators are not.
	unitName = regexp.MustCompile(`^[A-Za-z0-9:_.\\@-]{1,200}\.(service|socket|timer|mount|target|path)$`)
	// dnsName is a conservative hostname check (no wildcards, no underscores).
	dnsName = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)*[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.?$`)
)

// ValidResourceName reports whether s is a syntactically valid resource name.
func ValidResourceName(s string) bool { return resourceName.MatchString(s) }

// Config is the root of the configuration file.
type Config struct {
	Server  Server  `yaml:"server"`
	Limits  Limits  `yaml:"limits"`
	Audit   Audit   `yaml:"audit"`
	Docker  Docker  `yaml:"docker"`
	Systemd Systemd `yaml:"systemd"`
	Host    Host    `yaml:"host"`
	Health  Health  `yaml:"health"`
}

// Server configures the agent-facing Unix socket.
type Server struct {
	// Socket is the absolute path of the Unix domain socket.
	Socket string `yaml:"socket"`
	// SocketGroup is the group name that owns the socket (mode is fixed 0660).
	SocketGroup string `yaml:"socket_group"`
	// AllowedUIDs / AllowedGIDs restrict which peers may connect, verified with
	// kernel-supplied peer credentials. At least one entry is required.
	AllowedUIDs []uint32 `yaml:"allowed_uids"`
	AllowedGIDs []uint32 `yaml:"allowed_gids"`
}

// Limits bounds what a single call may return.
type Limits struct {
	MaxLogLines int `yaml:"max_log_lines"`
	MaxLogBytes int `yaml:"max_log_bytes"`
}

// Audit configures the audit log.
type Audit struct {
	// File is an optional path; empty means stderr.
	File string `yaml:"file"`
}

// Docker lists the containers Latch may inspect.
type Docker struct {
	Socket   string          `yaml:"socket"`
	Services []DockerService `yaml:"services"`
}

// DockerService maps an agent-visible name to a container name.
type DockerService struct {
	Name      string `yaml:"name"`
	Container string `yaml:"container"`
}

// Systemd lists the units Latch may inspect.
type Systemd struct {
	Units []string `yaml:"units"`
}

// Host lists the filesystems whose usage may be reported.
type Host struct {
	Disks []Disk `yaml:"disks"`
}

// Disk maps an agent-visible name to a mount point.
type Disk struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

// Health lists the fixed health-check targets.
type Health struct {
	HTTP []HTTPTarget `yaml:"http"`
	DNS  []DNSTarget  `yaml:"dns"`
}

// HTTPTarget is a fixed URL the agent can ask Latch to probe by name.
type HTTPTarget struct {
	Name         string        `yaml:"name"`
	URL          string        `yaml:"url"`
	ExpectStatus int           `yaml:"expect_status"`
	Timeout      time.Duration `yaml:"timeout"`
	// AllowPrivate permits the target to resolve to loopback/private/link-local
	// addresses. It is false by default so a public-looking URL cannot be
	// pointed at internal services through DNS.
	AllowPrivate bool `yaml:"allow_private"`
}

// DNSTarget is a fixed hostname the agent can ask Latch to resolve by name.
type DNSTarget struct {
	Name string `yaml:"name"`
	Host string `yaml:"host"`
}

// Load reads, parses and validates the file at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path) //nolint:gosec // path is the operator-supplied config location
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(data)
}

// Parse parses and validates config bytes.
func Parse(data []byte) (*Config, error) {
	if len(data) > MaxConfigBytes {
		return nil, fmt.Errorf("config exceeds %d bytes", MaxConfigBytes)
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Limits.MaxLogLines == 0 {
		c.Limits.MaxLogLines = DefaultMaxLogLines
	}
	if c.Limits.MaxLogBytes == 0 {
		c.Limits.MaxLogBytes = DefaultMaxLogBytes
	}
	if c.Docker.Socket == "" {
		c.Docker.Socket = DefaultDockerSocket
	}
	for i := range c.Health.HTTP {
		t := &c.Health.HTTP[i]
		if t.ExpectStatus == 0 {
			t.ExpectStatus = 200
		}
		if t.Timeout == 0 {
			t.Timeout = DefaultCheckTimeout
		}
	}
}

// Validate checks every field. It returns all problems found, not just the
// first, so operators can fix a config in one pass.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	// server
	if !filepath.IsAbs(c.Server.Socket) || filepath.Clean(c.Server.Socket) != c.Server.Socket {
		add("server.socket must be a clean absolute path")
	}
	if len(c.Server.AllowedUIDs)+len(c.Server.AllowedGIDs) == 0 {
		add("server: at least one of allowed_uids or allowed_gids is required")
	}

	// limits
	if c.Limits.MaxLogLines < 1 || c.Limits.MaxLogLines > HardMaxLogLines {
		add("limits.max_log_lines must be between 1 and %d", HardMaxLogLines)
	}
	if c.Limits.MaxLogBytes < 1 || c.Limits.MaxLogBytes > HardMaxLogBytes {
		add("limits.max_log_bytes must be between 1 and %d", HardMaxLogBytes)
	}

	// audit
	if c.Audit.File != "" && !filepath.IsAbs(c.Audit.File) {
		add("audit.file must be an absolute path")
	}

	// docker
	if !filepath.IsAbs(c.Docker.Socket) {
		add("docker.socket must be an absolute path")
	}
	seen := map[string]bool{}
	for i, s := range c.Docker.Services {
		if !resourceName.MatchString(s.Name) {
			add("docker.services[%d].name is not a valid resource name", i)
		} else if seen["docker/"+s.Name] {
			add("docker.services: duplicate name %q", s.Name)
		}
		seen["docker/"+s.Name] = true
		if !containerName.MatchString(s.Container) {
			add("docker.services[%d].container is not a valid container name", i)
		}
	}

	// systemd
	for i, u := range c.Systemd.Units {
		if !unitName.MatchString(u) {
			add("systemd.units[%d] must be a full unit name such as \"ssh.service\"", i)
		} else if seen["unit/"+u] {
			add("systemd.units: duplicate unit %q", u)
		}
		seen["unit/"+u] = true
	}

	// host
	for i, d := range c.Host.Disks {
		if !resourceName.MatchString(d.Name) {
			add("host.disks[%d].name is not a valid resource name", i)
		} else if seen["disk/"+d.Name] {
			add("host.disks: duplicate name %q", d.Name)
		}
		seen["disk/"+d.Name] = true
		if !filepath.IsAbs(d.Path) || filepath.Clean(d.Path) != d.Path {
			add("host.disks[%d].path must be a clean absolute path", i)
		}
	}

	// health
	for i, t := range c.Health.HTTP {
		if !resourceName.MatchString(t.Name) {
			add("health.http[%d].name is not a valid resource name", i)
		} else if seen["http/"+t.Name] {
			add("health.http: duplicate name %q", t.Name)
		}
		seen["http/"+t.Name] = true
		if err := validateHTTPURL(t.URL); err != nil {
			add("health.http[%d].url: %v", i, err)
		}
		if t.ExpectStatus < 100 || t.ExpectStatus > 599 {
			add("health.http[%d].expect_status must be 100-599", i)
		}
		if t.Timeout <= 0 || t.Timeout > MaxCheckTimeout {
			add("health.http[%d].timeout must be in (0, %s]", i, MaxCheckTimeout)
		}
	}
	for i, t := range c.Health.DNS {
		if !resourceName.MatchString(t.Name) {
			add("health.dns[%d].name is not a valid resource name", i)
		} else if seen["dns/"+t.Name] {
			add("health.dns: duplicate name %q", t.Name)
		}
		seen["dns/"+t.Name] = true
		if len(t.Host) > 253 || !dnsName.MatchString(t.Host) {
			add("health.dns[%d].host is not a valid hostname", i)
		}
	}

	return errors.Join(errs...)
}

// validateHTTPURL requires an http(s) URL with a host and no embedded
// credentials, query string, or fragment (any of which could carry secrets
// that would then show up in audit logs and errors).
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("scheme must be http or https")
	}
	if u.Hostname() == "" {
		return errors.New("host is required")
	}
	if u.User != nil {
		return errors.New("must not contain credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" || strings.Contains(raw, "?") || strings.Contains(raw, "#") {
		return errors.New("must not contain a query string or fragment")
	}
	return nil
}
