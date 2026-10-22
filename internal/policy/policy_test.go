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
server: {socket: /run/l.sock, allowed_uids: [1]}
limits: {max_log_lines: 50}
docker:
  services:
    - {name: web, container: my-web}
systemd:
  units: [ssh.service]
host:
  disks:
    - {name: root, path: /}
health:
  http:
    - {name: site, url: "https://example.com/"}
  dns:
    - {name: ex, host: example.com}
`))
	if err != nil {
		t.Fatal(err)
	}
	return New(c)
}

func TestResolvesConfiguredNames(t *testing.T) {
	p := testPolicy(t)
	if s, err := p.DockerService("web"); err != nil || s.Container != "my-web" {
		t.Errorf("docker: %+v %v", s, err)
	}
	if u, err := p.SystemdUnit("ssh.service"); err != nil || u != "ssh.service" {
		t.Errorf("unit: %q %v", u, err)
	}
	if d, err := p.Disk("root"); err != nil || d.Path != "/" {
		t.Errorf("disk: %+v %v", d, err)
	}
	if _, err := p.HTTPTarget("site"); err != nil {
		t.Error(err)
	}
	if _, err := p.DNSTarget("ex"); err != nil {
		t.Error(err)
	}
}

func TestUnknownResourcesAreRejected(t *testing.T) {
	p := testPolicy(t)
	for name, fn := range map[string]func(string) error{
		"docker":  func(n string) error { _, e := p.DockerService(n); return e },
		"disk":    func(n string) error { _, e := p.Disk(n); return e },
		"http":    func(n string) error { _, e := p.HTTPTarget(n); return e },
		"dns":     func(n string) error { _, e := p.DNSTarget(n); return e },
		"systemd": func(n string) error { _, e := p.SystemdUnit(n); return e },
	} {
		if err := fn("nope"); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("%s: unknown well-formed name: got %v, want ErrNotAllowed", name, err)
		}
	}
}

// Anything that tries to reach outside the allowlist by manipulating the name
// must be rejected, whether as malformed or as unknown. It must never resolve.
func TestEscapeAttemptsNeverResolve(t *testing.T) {
	p := testPolicy(t)
	attempts := []string{
		"", " ", "web ", " web", "WEB", "web\n", "web\x00", "web/../x", "../web", "/web",
		"web;id", "web && id", "$(id)", "`id`", "web|cat", "--privileged", "-v", "web=1",
		"my-web",                // the real container name is not an agent-visible name
		"ssh",                   // bare unit name must not be auto-suffixed
		"web\u202e", "w\u0435b", // RTL override, Cyrillic 'е' lookalike
		strings.Repeat("a", 5000),
	}
	for _, a := range attempts {
		if s, err := p.DockerService(a); err == nil {
			t.Errorf("DockerService(%q) resolved to %+v", a, s)
		}
		if d, err := p.Disk(a); err == nil {
			t.Errorf("Disk(%q) resolved to %+v", a, d)
		}
		if _, err := p.HTTPTarget(a); err == nil {
			t.Errorf("HTTPTarget(%q) resolved", a)
		}
		if _, err := p.DNSTarget(a); err == nil {
			t.Errorf("DNSTarget(%q) resolved", a)
		}
	}
	for _, a := range []string{"ssh", "SSH.service", "ssh.service ", "ssh.service\n", "../ssh.service", "ssh.service/../x", "", "docker.service"} {
		if u, err := p.SystemdUnit(a); err == nil {
			t.Errorf("SystemdUnit(%q) resolved to %q", a, u)
		}
	}
}

func TestMalformedNamesReportInvalid(t *testing.T) {
	p := testPolicy(t)
	if _, err := p.DockerService("web;id"); !errors.Is(err, ErrInvalidName) {
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
	n := p.Names(KindDocker)
	n[0] = "tampered"
	if p.Names(KindDocker)[0] != "web" {
		t.Error("Names must return a copy")
	}
}
