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
		{"unknown nested key", validBase + "docker:\n  services:\n    - name: a\n      container: b\n      exec: /bin/sh\n", "field exec not found"},
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
		{"service name uppercase", validBase + "docker:\n  services:\n    - {name: Web, container: c}\n", "docker.services[0].name"},
		{"service name with slash", validBase + "docker:\n  services:\n    - {name: a/b, container: c}\n", "docker.services[0].name"},
		{"container with flag-like name", validBase + "docker:\n  services:\n    - {name: a, container: --privileged}\n", "docker.services[0].container"},
		{"container with space", validBase + "docker:\n  services:\n    - {name: a, container: 'x y'}\n", "docker.services[0].container"},
		{"duplicate service", validBase + "docker:\n  services:\n    - {name: a, container: c}\n    - {name: a, container: d}\n", "duplicate"},
		{"unit without suffix", validBase + "systemd:\n  units: [ssh]\n", "systemd.units[0]"},
		{"unit with path", validBase + "systemd:\n  units: ['../../etc/x.service']\n", "systemd.units[0]"},
		{"unit with space", validBase + "systemd:\n  units: ['a b.service']\n", "systemd.units[0]"},
		{"relative disk path", validBase + "host:\n  disks:\n    - {name: a, path: var}\n", "host.disks[0].path"},
		{"disk path traversal", validBase + "host:\n  disks:\n    - {name: a, path: /var/../etc}\n", "host.disks[0].path"},
		{"http file scheme", validBase + "health:\n  http:\n    - {name: a, url: 'file:///etc/passwd'}\n", "scheme"},
		{"http credentials in url", validBase + "health:\n  http:\n    - {name: a, url: 'http://u:p@host/'}\n", "credentials"},
		{"http query string", validBase + "health:\n  http:\n    - {name: a, url: 'http://h/?token=s'}\n", "query"},
		{"http bad status", validBase + "health:\n  http:\n    - {name: a, url: 'http://h/', expect_status: 999}\n", "expect_status"},
		{"http huge timeout", validBase + "health:\n  http:\n    - {name: a, url: 'http://h/', timeout: 1h}\n", "timeout"},
		{"dns host with scheme", validBase + "health:\n  dns:\n    - {name: a, host: 'http://x'}\n", "hostname"},
		{"dns wildcard", validBase + "health:\n  dns:\n    - {name: a, host: '*.example.com'}\n", "hostname"},
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

func TestErrorsDoNotEchoURLs(t *testing.T) {
	_, err := Parse([]byte(validBase + "health:\n  http:\n    - {name: a, url: 'http://user:hunter2@host/'}\n"))
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error must not echo credentials: %v", err)
	}
}

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(validBase + "health:\n  http:\n    - {name: a, url: 'https://h/'}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Limits.MaxLogLines != DefaultMaxLogLines || c.Docker.Socket != DefaultDockerSocket {
		t.Errorf("defaults not applied: %+v", c)
	}
	if h := c.Health.HTTP[0]; h.ExpectStatus != 200 || h.Timeout != DefaultCheckTimeout || h.AllowPrivate {
		t.Errorf("http defaults wrong (allow_private must default to false): %+v", h)
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
