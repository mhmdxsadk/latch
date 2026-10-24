package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validBase = `
server:
  socket: /run/latch/latch.sock
  socket_group: latch-agents
`

func TestExampleConfigIsValid(t *testing.T) {
	if _, err := Load("../../config/example.yaml"); err != nil {
		t.Fatalf("config/example.yaml must always validate: %v", err)
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string // substring of the error
	}{
		{"unknown top-level key", validBase + "commands:\n  - rm -rf /\n", "field commands not found"},
		{"unknown nested key", validBase + "docker:\n  exclude: [a]\n  exec: /bin/sh\n", "field exec not found"},
		{"old docker services key", validBase + "docker:\n  services:\n    - {name: a, container: b}\n", "field services not found"},
		{"old systemd units key", validBase + "systemd:\n  units: [ssh.service]\n", "field units not found"},
		{"no socket group", "server:\n  socket: /run/latch.sock\n", "server.socket_group"},
		{"socket group with path", "server:\n  socket: /run/latch.sock\n  socket_group: ../x\n", "server.socket_group"},
		{"socket group numeric", "server:\n  socket: /run/latch.sock\n  socket_group: '0'\n", "server.socket_group"},
		{"old allowed_uids key", validBase + "  allowed_uids: [1000]\n", "field allowed_uids not found"},
		{"old allowed_gids key", validBase + "  allowed_gids: []\n", "field allowed_gids not found"},
		{"relative socket", "server:\n  socket: latch.sock\n  socket_group: latch-agents\n", "server.socket"},
		{"socket path traversal", "server:\n  socket: /run/../etc/x.sock\n  socket_group: latch-agents\n", "server.socket"},
		{"log lines above hard cap", validBase + "limits:\n  max_log_lines: 100000\n", "max_log_lines"},
		{"log bytes above hard cap", validBase + "limits:\n  max_log_bytes: 99999999\n", "max_log_bytes"},
		{"negative log lines", validBase + "limits:\n  max_log_lines: -1\n", "max_log_lines"},
		{"docker exclude with flag-like name", validBase + "docker:\n  exclude: ['--privileged!']\n", "docker.exclude[0]"},
		{"docker exclude with space", validBase + "docker:\n  exclude: ['x y']\n", "docker.exclude[0]"},
		{"docker exclude with slash", validBase + "docker:\n  exclude: ['a/b']\n", "docker.exclude[0]"},
		{"docker exclude with class", validBase + "docker:\n  exclude: ['a[bc]']\n", "docker.exclude[0]"},
		{"docker exclude with escape", validBase + "docker:\n  exclude: ['a\\\\*']\n", "docker.exclude[0]"},
		{"docker exclude empty", validBase + "docker:\n  exclude: ['']\n", "docker.exclude[0]"},
		{"docker exclude duplicate", validBase + "docker:\n  exclude: [a, a]\n", "duplicate"},
		{"systemd exclude with path", validBase + "systemd:\n  exclude: ['../../etc/x.service']\n", "systemd.exclude[0]"},
		{"systemd exclude with space", validBase + "systemd:\n  exclude: ['a b.service']\n", "systemd.exclude[0]"},
		{"relative disk path", validBase + "host:\n  disks:\n    - {name: a, path: var}\n", "host.disks[0].path"},
		{"disk path traversal", validBase + "host:\n  disks:\n    - {name: a, path: /var/../etc}\n", "host.disks[0].path"},
		{"old http checks section", validBase + "health:\n  http:\n    - {name: a, url: 'https://h/'}\n", "field http not found"},
		{"dns host with scheme", validBase + "health:\n  dns:\n    - {name: a, host: 'http://x'}\n", "hostname"},
		{"dns wildcard", validBase + "health:\n  dns:\n    - {name: a, host: '*.example.com'}\n", "hostname"},
		{"network exclude wildcard", validBase + "network:\n  exclude: ['*.example.com']\n", "network.exclude[0]"},
		{"network exclude url", validBase + "network:\n  exclude: ['http://example.com']\n", "network.exclude[0]"},
		{"network exclude with port", validBase + "network:\n  exclude: ['example.com:80']\n", "network.exclude[0]"},
		{"network exclude cidr", validBase + "network:\n  exclude: ['10.0.0.0/8']\n", "network.exclude[0]"},
		{"network exclude empty", validBase + "network:\n  exclude: ['']\n", "network.exclude[0]"},
		{"network exclude zone", validBase + "network:\n  exclude: ['fe80::1%eth0']\n", "network.exclude[0]"},
		{"network exclude too long", validBase + "network:\n  exclude: ['" + strings.Repeat("a.", 127) + "aa']\n", "network.exclude[0]"},
		{"network exclude duplicate", validBase + "network:\n  exclude: [a.example, a.example]\n", "duplicate"},
		{"network exclude duplicate case and dot", validBase + "network:\n  exclude: [a.example, 'A.Example.']\n", "duplicate"},
		{"network exclude duplicate mapped ip", validBase + "network:\n  exclude: [10.0.0.1, '::ffff:10.0.0.1']\n", "duplicate"},
		{"network unknown key", validBase + "network:\n  allow: [x]\n", "field allow not found"},
		{"not yaml mapping", "- just\n- a list\n", "parse config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestNetworkExcludeAccepts(t *testing.T) {
	c, err := Parse([]byte(validBase + "network:\n  exclude: [vault.internal, 'Admin.Example.', 10.0.0.5, 'fd00::5']\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Network.Exclude) != 4 {
		t.Errorf("got %v", c.Network.Exclude)
	}
}

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"Example.COM.": "example.com", "example.com": "example.com",
		"::ffff:10.0.0.1": "10.0.0.1", "FD00::0005": "fd00::5", "10.0.0.1": "10.0.0.1",
	} {
		if got := NormalizeHost(in); got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatchesAny(t *testing.T) {
	pats := []string{"secrets-broker", "infisical*", "vault-?", "ssh.service", "getty@*.service"}
	for name, want := range map[string]bool{
		"secrets-broker": true, "secrets-broker2": false, "my-secrets-broker": false,
		"infisical": true, "infisical-redis": true, "not-infisical": false,
		"vault-1": true, "vault-12": false, "vault-": false,
		"ssh.service": true, "ssh": false, "SSH.service": false,
		"getty@tty1.service": true, "getty.service": false,
		"": false,
	} {
		if got := MatchesAny(pats, name); got != want {
			t.Errorf("MatchesAny(%q) = %v, want %v", name, got, want)
		}
	}
	if MatchesAny(nil, "anything") {
		t.Error("an empty list excludes nothing")
	}
}

func TestExcludeListsAccepted(t *testing.T) {
	c, err := Parse([]byte(validBase + "docker:\n  exclude: [secrets-broker, 'infisical*']\nsystemd:\n  exclude: [vault.service, 'getty@*.service']\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Docker.Exclude) != 2 || len(c.Systemd.Exclude) != 2 {
		t.Errorf("got %+v", c)
	}
}

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(validBase))
	if err != nil {
		t.Fatal(err)
	}
	if c.Limits.MaxLogLines != DefaultMaxLogLines || c.Docker.Socket != DefaultDockerSocket {
		t.Errorf("defaults not applied: %+v", c)
	}
}

func TestOversizedConfigRejected(t *testing.T) {
	big := validBase + "# " + strings.Repeat("x", MaxConfigBytes)
	if _, err := Parse([]byte(big)); err == nil {
		t.Fatal("expected size error")
	}
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected size error from Load")
	}
}

func TestMissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/latch.yaml"); err == nil {
		t.Fatal("expected error")
	}
}

func TestProblems(t *testing.T) {
	_, err := Parse([]byte(validBase + "  allowed_uids: [1]\nlimits:\n  max_log_lines: 5000\n"))
	got := Problems(err)
	if len(got) != 1 || got[0].Line != 5 || got[0].Text != `unknown field "allowed_uids"` {
		t.Errorf("unknown field: %+v", got)
	}

	_, err = Parse([]byte("server:\n  socket: relative\nlimits:\n  max_log_lines: 5000\n"))
	got = Problems(err)
	if len(got) != 3 {
		t.Fatalf("want 3 validation problems (socket, socket_group, max_log_lines), got %+v", got)
	}
	for _, p := range got {
		if p.Line != 0 || strings.Contains(p.Text, "\n") {
			t.Errorf("validation problems are single-line and have no line: %+v", p)
		}
	}

	if Problems(nil) != nil {
		t.Error("nil error has no problems")
	}
	if got := Problems(errors.New("open config: no such file")); len(got) != 1 || got[0].Text != "open config: no such file" {
		t.Errorf("plain error: %+v", got)
	}
}
