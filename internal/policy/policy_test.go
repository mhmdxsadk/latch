package policy

import (
	"errors"
	"strings"
	"testing"

	"github.com/mhmdxsadk/latch/internal/config"
)

func testPolicy(t *testing.T) *Policy {
	t.Helper()
	c, err := config.Parse([]byte(`
server: {socket: /run/l.sock, socket_group: latch-agents}
limits: {max_log_lines: 50}
docker:
  exclude: [secrets-broker, 'infisical*']
systemd:
  exclude: [vault.service, 'getty@*.service']
host:
  disks:
    - {name: root, path: /}
health:
  dns:
    - {name: ex, host: example.com}
`))
	if err != nil {
		t.Fatal(err)
	}
	return New(c)
}

func TestOpenByDefaultExceptExcluded(t *testing.T) {
	p := testPolicy(t)
	for _, n := range []string{"web", "my-web", "jellyfin", "Infisical", "a.b_c-d", strings.Repeat("a", 128)} {
		if got, err := p.Container(n); err != nil || got != n {
			t.Errorf("Container(%q) = %q, %v", n, got, err)
		}
	}
	for _, n := range []string{"ssh.service", "docker.service", "getty-pre.target", "user@1000.service", "a\\x2db.mount"} {
		if got, err := p.SystemdUnit(n); err != nil || got != n {
			t.Errorf("SystemdUnit(%q) = %q, %v", n, got, err)
		}
	}
	if d, err := p.Disk("root"); err != nil || d.Path != "/" {
		t.Errorf("disk: %+v %v", d, err)
	}
	if _, err := p.DNSTarget("ex"); err != nil {
		t.Error(err)
	}
}

func TestExcludedResourcesAreRefused(t *testing.T) {
	p := testPolicy(t)
	for _, n := range []string{"secrets-broker", "infisical", "infisical-redis", "infisical-postgres"} {
		if _, err := p.Container(n); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Container(%q): got %v, want ErrNotAllowed", n, err)
		}
		if p.ContainerVisible(n) {
			t.Errorf("ContainerVisible(%q) = true", n)
		}
	}
	for _, n := range []string{"vault.service", "getty@tty1.service", "getty@.service"} {
		if _, err := p.SystemdUnit(n); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("SystemdUnit(%q): got %v, want ErrNotAllowed", n, err)
		}
		if p.UnitVisible(n) {
			t.Errorf("UnitVisible(%q) = true", n)
		}
	}
	for _, n := range []string{"secrets-broker2", "my-secrets-broker", "vault.socket"} {
		if !p.ContainerVisible(n) && !p.UnitVisible(n) {
			t.Errorf("%q should not be excluded", n)
		}
	}
}

func TestUnknownDisksAndDNSAreRejected(t *testing.T) {
	p := testPolicy(t)
	if _, err := p.Disk("nope"); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("disk: %v", err)
	}
	if _, err := p.DNSTarget("nope"); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("dns: %v", err)
	}
}

// Malformed names must never resolve, so an exclude pattern is never the only
// thing between the agent and an injected argument or path.
func TestEscapeAttemptsNeverResolve(t *testing.T) {
	p := testPolicy(t)
	attempts := []string{
		"", " ", "web ", " web", "web\n", "web\x00", "web/../x", "../web", "/web",
		"web;id", "web && id", "$(id)", "`id`", "web|cat", "--privileged", "-v", "web=1",
		"web\u202e", "w\u0435b", // RTL override, Cyrillic 'е' lookalike
		strings.Repeat("a", 5000),
	}
	for _, a := range attempts {
		if s, err := p.Container(a); err == nil {
			t.Errorf("Container(%q) resolved to %q", a, s)
		}
		if d, err := p.Disk(a); err == nil {
			t.Errorf("Disk(%q) resolved to %+v", a, d)
		}
		if _, err := p.DNSTarget(a); err == nil {
			t.Errorf("DNSTarget(%q) resolved", a)
		}
	}
	for _, a := range []string{"ssh", "ssh.service ", "ssh.service\n", "../ssh.service", "ssh.service/../x", "", "*.service", "ssh.*", "a b.service"} {
		if u, err := p.SystemdUnit(a); err == nil {
			t.Errorf("SystemdUnit(%q) resolved to %q", a, u)
		}
	}
}

func TestMalformedNamesReportInvalid(t *testing.T) {
	p := testPolicy(t)
	if _, err := p.Container("web;id"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("got %v", err)
	}
	if _, err := p.SystemdUnit("ssh"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("got %v", err)
	}
}

func TestLogLimits(t *testing.T) {
	p := testPolicy(t) // max 50
	if n, _, err := p.LogLimits(0); err != nil || n != 50 {
		t.Errorf("default should clamp to config max: %d %v", n, err)
	}
	if n, b, err := p.LogLimits(10); err != nil || n != 10 || b != config.DefaultMaxLogBytes {
		t.Errorf("got %d %d %v", n, b, err)
	}
	for _, bad := range []int{-1, 51, 1 << 30} {
		if _, _, err := p.LogLimits(bad); err == nil {
			t.Errorf("LogLimits(%d) should fail", bad)
		}
	}
}

func TestNamesIsACopy(t *testing.T) {
	p := testPolicy(t)
	n := p.Names(KindDisk)
	n[0] = "tampered"
	if p.Names(KindDisk)[0] != "root" {
		t.Error("Names must return a copy")
	}
}
