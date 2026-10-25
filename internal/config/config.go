// Package config loads and validates Latch's configuration.
//
// The configuration only declares WHICH resources Latch may expose or hide. It
// has no way to express commands or arguments: every field is either a
// resource name, a name pattern, a fixed identifier (mount point), a path or
// file name to hide, or a bound. Files are readable by default and the config
// can only take paths away. Docker containers and systemd units are exposed by default and the
// operator lists the ones to exclude. Unknown keys are rejected so a typo can
// never silently widen or drop policy.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
)

var (
	// resourceName is the shape of every agent-visible resource name.
	resourceName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	// containerName matches Docker's own container name rule.
	containerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	// unitName matches a fully qualified systemd unit name. Template
	// instances ("foo@bar.service") are allowed; path separators are not.
	unitName = regexp.MustCompile(`^[A-Za-z0-9:_.\\@-]{1,200}\.(service|socket|timer|mount|target|path)$`)
	// excludePattern is a name pattern: name characters plus the * and ?
	// wildcards. Backslash and brackets are not allowed, so path.Match can
	// never see an escape or a character class.
	excludePattern = regexp.MustCompile(`^[A-Za-z0-9:_.@*?-]{1,200}$`)
	// fileNamePattern is a file name pattern for files.exclude: name
	// characters plus the * and ? wildcards. Slash, backslash and brackets are
	// not allowed, so path.Match can never see an escape or a character class.
	fileNamePattern = regexp.MustCompile(`^[A-Za-z0-9:_.@*?+=,~-]{1,255}$`)
	// groupName is a conservative Unix group name.
	groupName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	// dnsName is a conservative hostname check (no wildcards, no underscores).
	dnsName = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)*[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.?$`)
	// queryName is dnsName with underscores, for DNS lookups of names such
	// as _dmarc.example.com or _imap._tcp.example.com.
	queryName = regexp.MustCompile(`^([A-Za-z0-9_]([A-Za-z0-9_-]{0,61}[A-Za-z0-9_])?\.)*[A-Za-z0-9_]([A-Za-z0-9_-]{0,61}[A-Za-z0-9_])?\.?$`)
)

// MaxHostnameLen is the longest hostname accepted anywhere.
const MaxHostnameLen = 253

// ValidHostname reports whether s is a conservative hostname: letters, digits
// and hyphens in dot-separated labels, an optional trailing dot, at most 253
// bytes. No wildcards or underscores.
func ValidHostname(s string) bool { return len(s) <= MaxHostnameLen && dnsName.MatchString(s) }

// ValidQueryName is ValidHostname that also allows underscores, for DNS
// lookups only. Names with underscores are never connected to.
func ValidQueryName(s string) bool { return len(s) <= MaxHostnameLen && queryName.MatchString(s) }

// NormalizeHost lowercases a hostname and removes one trailing dot, so
// "Example.COM." and "example.com" compare equal. IP literals are returned in
// canonical form (IPv4-mapped IPv6 as IPv4).
func NormalizeHost(s string) string {
	s = strings.TrimSuffix(strings.ToLower(s), ".")
	if a, err := netip.ParseAddr(s); err == nil && a.Zone() == "" {
		return a.Unmap().String()
	}
	return s
}

// ValidResourceName reports whether s is a syntactically valid resource name.
func ValidResourceName(s string) bool { return resourceName.MatchString(s) }

// ValidContainerName reports whether s is a syntactically valid container name.
func ValidContainerName(s string) bool { return containerName.MatchString(s) }

// ValidUnitName reports whether s is a syntactically valid full unit name.
func ValidUnitName(s string) bool { return unitName.MatchString(s) }

// MatchesAny reports whether name matches any of the exclude patterns. A
// pattern is a plain name, optionally with * (any run of characters) and ?
// (one character). Patterns are validated by Config.Validate.
func MatchesAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// Config is the root of the configuration file.
type Config struct {
	Server  Server  `yaml:"server"`
	Limits  Limits  `yaml:"limits"`
	Audit   Audit   `yaml:"audit"`
	Docker  Docker  `yaml:"docker"`
	Systemd Systemd `yaml:"systemd"`
	Host    Host    `yaml:"host"`
	Health  Health  `yaml:"health"`
	Files   Files   `yaml:"files"`
	Network Network `yaml:"network"`
}

// Network configures the network diagnostic tools. Any address may be
// checked except excluded containers (blocked automatically) and the names
// listed in Exclude.
type Network struct {
	// Exclude lists exact hostnames (no wildcards) or IP addresses the
	// network tools must never reach.
	Exclude []string `yaml:"exclude"`
}

// Server configures the agent-facing Unix socket.
type Server struct {
	// Socket is the absolute path of the Unix domain socket.
	Socket string `yaml:"socket"`
	// SocketGroup owns the socket (mode is fixed 0660) and is the only group
	// whose processes may connect, verified with kernel-supplied peer
	// credentials. Required.
	SocketGroup string `yaml:"socket_group"`
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

// Docker configures container access. Every container may be inspected and its
// logs read unless its name matches an Exclude pattern.
type Docker struct {
	Socket  string   `yaml:"socket"`
	Exclude []string `yaml:"exclude"`
}

// Systemd configures unit access. Every unit may be inspected unless its full
// name matches an Exclude pattern.
type Systemd struct {
	Exclude []string `yaml:"exclude"`
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

// Files configures the read-only file tools. The whole disk is readable except
// a built-in block list (see package files) and the entries in Exclude.
type Files struct {
	// Exclude entries are either a clean absolute path, which blocks that file
	// or directory and everything below it, or a name pattern with no slash
	// (* and ? wildcards), which blocks every file or directory with a
	// matching name and everything below it.
	Exclude []string `yaml:"exclude"`
}

// Health lists the fixed DNS check targets.
type Health struct {
	DNS []DNSTarget `yaml:"dns"`
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

// Problem is one thing wrong with a config file. Line is 0 when the problem is
// not tied to a line.
type Problem struct {
	Line int
	Text string
}

var yamlFieldError = regexp.MustCompile(`^line (\d+): field (\S+) not found in type `)
var yamlLineError = regexp.MustCompile(`^line (\d+): (.*)$`)

// Problems splits an error from Load or Parse into one Problem per line of
// output, so callers can print "file:line: message" like other tools do.
func Problems(err error) []Problem {
	if err == nil {
		return nil
	}
	var te *yaml.TypeError
	if errors.As(err, &te) {
		var out []Problem
		for _, e := range te.Errors {
			if m := yamlFieldError.FindStringSubmatch(e); m != nil {
				out = append(out, Problem{Line: atoi(m[1]), Text: fmt.Sprintf("unknown field %q", m[2])})
			} else if m := yamlLineError.FindStringSubmatch(e); m != nil {
				out = append(out, Problem{Line: atoi(m[1]), Text: m[2]})
			} else {
				out = append(out, Problem{Text: e})
			}
		}
		return out
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var out []Problem
		for _, e := range j.Unwrap() {
			out = append(out, Problems(e)...)
		}
		return out
	}
	return []Problem{{Text: err.Error()}}
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
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
	if !groupName.MatchString(c.Server.SocketGroup) {
		add("server.socket_group must be a valid group name")
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
	checkPatterns(add, "docker.exclude", c.Docker.Exclude)

	// systemd
	checkPatterns(add, "systemd.exclude", c.Systemd.Exclude)

	seen := map[string]bool{}
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
	for i, t := range c.Health.DNS {
		if !resourceName.MatchString(t.Name) {
			add("health.dns[%d].name is not a valid resource name", i)
		} else if seen["dns/"+t.Name] {
			add("health.dns: duplicate name %q", t.Name)
		}
		seen["dns/"+t.Name] = true
		if !ValidHostname(t.Host) {
			add("health.dns[%d].host is not a valid hostname", i)
		}
	}

	// files
	checkFileExcludes(add, c.Files.Exclude)

	// network
	seenHost := map[string]bool{}
	for i, h := range c.Network.Exclude {
		a, err := netip.ParseAddr(h)
		switch {
		case err == nil && a.Zone() != "":
			add("network.exclude[%d] must not have an IPv6 zone", i)
		case err != nil && !ValidHostname(h):
			add("network.exclude[%d] must be an exact hostname or IP address (no wildcards)", i)
		case seenHost[NormalizeHost(h)]:
			add("network.exclude: duplicate entry %q", h)
		}
		seenHost[NormalizeHost(h)] = true
	}

	return errors.Join(errs...)
}

// MaxFilePathLen is the longest path accepted in files.exclude and by the file
// tools.
const MaxFilePathLen = 4096

// ValidFilePath reports whether p is a clean absolute path of at most
// MaxFilePathLen bytes, valid UTF-8, with no control characters.
func ValidFilePath(p string) bool {
	if p == "" || len(p) > MaxFilePathLen || p[0] != '/' || !utf8.ValidString(p) || filepath.Clean(p) != p {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] == 0x7f {
			return false
		}
	}
	return true
}

// checkFileExcludes validates files.exclude: each entry is a clean absolute
// path or a file name pattern.
func checkFileExcludes(add func(string, ...any), patterns []string) {
	seen := map[string]bool{}
	for i, p := range patterns {
		switch {
		case strings.Contains(p, ".."):
			add("files.exclude[%d] must not contain \"..\"", i)
		case strings.HasPrefix(p, "/"):
			if !ValidFilePath(p) {
				add("files.exclude[%d] must be a clean absolute path", i)
			}
		case p == ".":
			add("files.exclude[%d] must be an absolute path or a file name pattern", i)
		case !fileNamePattern.MatchString(p):
			add("files.exclude[%d] must be an absolute path or a file name, optionally with * or ? wildcards", i)
		}
		if seen[p] {
			add("files.exclude: duplicate entry %q", p)
		}
		seen[p] = true
	}
}

// checkPatterns validates a list of exclude patterns.
func checkPatterns(add func(string, ...any), field string, patterns []string) {
	seen := map[string]bool{}
	for i, p := range patterns {
		switch {
		case !excludePattern.MatchString(p):
			add("%s[%d] must be a name, optionally with * or ? wildcards", field, i)
		case seen[p]:
			add("%s: duplicate pattern %q", field, p)
		}
		seen[p] = true
	}
}
