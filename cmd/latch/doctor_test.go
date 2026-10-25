package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mhmdxsadk/latch/internal/config"
	"github.com/mhmdxsadk/latch/internal/docker"
	"github.com/mhmdxsadk/latch/internal/health"
	"github.com/mhmdxsadk/latch/internal/host"
	"github.com/mhmdxsadk/latch/internal/systemd"
)

type okHost struct{}

func (okHost) Load(context.Context) (host.Load, error)         { return host.Load{}, nil }
func (okHost) Memory(context.Context) (host.Memory, error)     { return host.Memory{}, nil }
func (okHost) Disk(context.Context, string) (host.Disk, error) { return host.Disk{}, nil }

type stubDocker struct{ ping, list error }

func (s stubDocker) Ping(context.Context) error { return s.ping }
func (s stubDocker) List(context.Context) ([]string, error) {
	return []string{"jellyfin", "secrets-broker", "secrets-vault"}, s.list
}
func (s stubDocker) Status(context.Context, string) (docker.Status, error) {
	return docker.Status{}, nil
}
func (s stubDocker) Logs(context.Context, string, int, int) (docker.Logs, error) {
	return docker.Logs{}, nil
}

func (s stubDocker) Exposure(context.Context, string) (docker.Exposure, error) {
	return docker.Exposure{}, nil
}

func (s stubDocker) Settings(context.Context, string) (docker.Settings, error) {
	return docker.Settings{}, nil
}
func (s stubDocker) Usage(context.Context, string) (docker.Usage, error) {
	return docker.Usage{}, nil
}
func (s stubDocker) Events(context.Context, time.Time, time.Time, func(string) bool, int) (docker.Events, error) {
	return docker.Events{}, nil
}
func (s stubDocker) IDs(context.Context) (map[string]string, error) { return nil, nil }

type stubSystemd struct{ ping, list error }

func (s stubSystemd) Ping(context.Context) error { return s.ping }
func (s stubSystemd) List(context.Context) ([]systemd.ListedUnit, error) {
	return []systemd.ListedUnit{{Name: "ssh.service"}, {Name: "vault.service"}}, s.list
}
func (s stubSystemd) Failed(context.Context) ([]systemd.ListedUnit, error) { return nil, nil }
func (s stubSystemd) Logs(context.Context, string, int, int) (systemd.Logs, error) {
	return systemd.Logs{}, nil
}
func (s stubSystemd) UnitStatus(context.Context, string) (systemd.UnitStatus, error) {
	return systemd.UnitStatus{}, nil
}

type stubHealth struct {
	dns health.DNSResult
}

func (s stubHealth) DNS(context.Context, config.DNSTarget) (health.DNSResult, error) {
	return s.dns, nil
}

func doctorConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Parse([]byte(`
server: {socket: ` + dir + `/l.sock, admin_socket: ` + dir + `/admin.sock, socket_group: latch-agents}
docker:
  exclude: [secrets-*]
systemd:
  exclude: [vault.service]
health:
  dns: [{name: example, host: example.com}]
`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func healthy() integrations {
	return integrations{
		host:    okHost{},
		docker:  stubDocker{},
		systemd: stubSystemd{},
		health:  stubHealth{dns: health.DNSResult{OK: true}},
		groupID: func(string) (uint32, error) { return 1500, nil },
		euid:    func() int { return 0 },
		compose: func(context.Context) (string, error) { return "/usr/bin/docker", nil },
	}
}

func TestDoctorRootAndCompose(t *testing.T) {
	in := healthy()
	in.euid = func() int { return 1000 }
	var out bytes.Buffer
	if doctor(context.Background(), doctorConfig(t), in, &out, false) {
		t.Fatal("non-root must fail")
	}
	if !strings.Contains(out.String(), "FAIL  running as root\n      running as uid 1000, not root\n      fix: run doctor with sudo") {
		t.Errorf("got:\n%s", out.String())
	}

	in = healthy()
	in.compose = func(context.Context) (string, error) { return "", errors.New("the docker CLI was not found") }
	out.Reset()
	if !doctor(context.Background(), doctorConfig(t), in, &out, false) {
		t.Fatalf("a missing compose plugin is only a warning:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "WARN  docker compose\n      the docker CLI was not found; docker_compose_up will fail") ||
		!strings.Contains(out.String(), "All checks passed.") {
		t.Errorf("got:\n%s", out.String())
	}
	out.Reset()
	doctor(context.Background(), doctorConfig(t), healthy(), &out, false)
	for _, want := range []string{"OK    running as root", "OK    docker compose (/usr/bin/docker)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}

func TestDoctorAdminSocketDirectory(t *testing.T) {
	cfg := doctorConfig(t)
	open := t.TempDir()
	os.Chmod(open, 0o777)
	cfg.Server.AdminSocket = filepath.Join(open, "admin.sock")
	var out bytes.Buffer
	if doctor(context.Background(), cfg, healthy(), &out, false) {
		t.Fatal("world-writable admin directory must fail")
	}
	if !strings.Contains(out.String(), "FAIL  admin socket directory "+open+"\n      anyone can write to it") {
		t.Errorf("got:\n%s", out.String())
	}
}

func TestDoctorAllHealthy(t *testing.T) {
	var out bytes.Buffer
	if !doctor(context.Background(), doctorConfig(t), healthy(), &out, false) {
		t.Fatalf("expected success:\n%s", out.String())
	}
	for _, want := range []string{"OK    socket group latch-agents", "OK    docker daemon", "OK    docker containers (1 visible, 2 excluded)", "OK    systemd services (1 visible, 1 excluded)", "OK    dns check example", "All checks passed."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "\x1b[") {
		t.Errorf("no color expected when color is off:\n%s", out.String())
	}
}

func TestDoctorReportsFailures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*integrations)
		want   string
	}{
		{"socket group missing", func(i *integrations) {
			i.groupID = func(string) (uint32, error) { return 0, errors.New("unknown group") }
		}, "FAIL  socket group latch-agents"},
		{"docker down", func(i *integrations) { i.docker = stubDocker{ping: errors.New("dial failed")} }, "FAIL  docker daemon"},
		{"container list fails", func(i *integrations) { i.docker = stubDocker{list: errors.New("denied")} }, "FAIL  docker containers"},
		{"no systemd", func(i *integrations) { i.systemd = stubSystemd{ping: errors.New("no bus")} }, "FAIL  systemd bus"},
		{"unit list fails", func(i *integrations) { i.systemd = stubSystemd{list: errors.New("denied")} }, "FAIL  systemd services"},
		{"dns failure", func(i *integrations) {
			i.health = stubHealth{dns: health.DNSResult{Failure: health.FailureNotFound}}
		}, "FAIL  dns check example\n      not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := healthy()
			tt.mutate(&in)
			var out bytes.Buffer
			if doctor(context.Background(), doctorConfig(t), in, &out, false) {
				t.Fatalf("expected failure:\n%s", out.String())
			}
			if !strings.Contains(out.String(), tt.want) {
				t.Errorf("want %q in:\n%s", tt.want, out.String())
			}
		})
	}
}

func TestDoctorSummaryCountsProblemsAndHintsAppear(t *testing.T) {
	in := healthy()
	in.docker = stubDocker{list: errors.New("denied")}
	in.systemd = stubSystemd{list: errors.New("denied")}
	var out bytes.Buffer
	if doctor(context.Background(), doctorConfig(t), in, &out, false) {
		t.Fatal("expected failure")
	}
	for _, want := range []string{"2 problems found.", "Docker socket", "system bus"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}

func TestDoctorColorOnlyWhenRequested(t *testing.T) {
	var out bytes.Buffer
	doctor(context.Background(), doctorConfig(t), healthy(), &out, true)
	if !strings.Contains(out.String(), "\x1b[32mOK  ") {
		t.Errorf("expected green check marks:\n%q", out.String())
	}
}

func TestUseColorOffForNonTerminal(t *testing.T) {
	if useColor(&bytes.Buffer{}) {
		t.Error("a buffer is not a terminal")
	}
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if useColor(f) {
		t.Error("a regular file is not a terminal")
	}
}

func TestDoctorInvalidConfigPrintsFileLineMessage(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	os.WriteFile(bad, []byte("server:\n  socket: /run/l.sock\n  socket_group: g\n  allowed_uids: [1]\n"), 0o600)
	var out, errb bytes.Buffer
	if code := cmdDoctor([]string{"-config", bad}, &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	want := "latch: " + bad + ":4: unknown field \"allowed_uids\"\n"
	if errb.String() != want {
		t.Errorf("stderr = %q, want %q", errb.String(), want)
	}
	if out.Len() != 0 {
		t.Errorf("nothing belongs on stdout, got: %s", out.String())
	}
}
