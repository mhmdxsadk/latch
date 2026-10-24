package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

type stubDocker struct{ ping, status error }

func (s stubDocker) Ping(context.Context) error { return s.ping }
func (s stubDocker) Status(context.Context, string) (docker.Status, error) {
	return docker.Status{}, s.status
}
func (s stubDocker) Logs(context.Context, string, int, int) (docker.Logs, error) {
	return docker.Logs{}, nil
}

type stubSystemd struct{ ping, status error }

func (s stubSystemd) Ping(context.Context) error { return s.ping }
func (s stubSystemd) UnitStatus(context.Context, string) (systemd.UnitStatus, error) {
	return systemd.UnitStatus{}, s.status
}

type stubHealth struct {
	http health.HTTPResult
	dns  health.DNSResult
}

func (s stubHealth) HTTP(context.Context, config.HTTPTarget) (health.HTTPResult, error) {
	return s.http, nil
}
func (s stubHealth) DNS(context.Context, config.DNSTarget) (health.DNSResult, error) {
	return s.dns, nil
}

func doctorConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(`
server: {socket: ` + t.TempDir() + `/l.sock, socket_group: latch-agents}
docker:
  services: [{name: web, container: my-web}]
systemd:
  units: [ssh.service]
health:
  http: [{name: site, url: "https://example.com/"}]
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
		health:  stubHealth{http: health.HTTPResult{OK: true, StatusCode: 200}, dns: health.DNSResult{OK: true}},
		groupID: func(string) (uint32, error) { return 1500, nil },
	}
}

func TestDoctorAllHealthy(t *testing.T) {
	var out bytes.Buffer
	if !doctor(context.Background(), doctorConfig(t), healthy(), &out, false) {
		t.Fatalf("expected success:\n%s", out.String())
	}
	for _, want := range []string{"OK    socket group latch-agents", "OK    docker daemon", "OK    docker service web (container my-web)", "OK    systemd unit ssh.service", "OK    http check site", "OK    dns check example", "All checks passed."} {
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
		{"container missing", func(i *integrations) { i.docker = stubDocker{status: docker.ErrNotFound} }, "FAIL  docker service web (container my-web)"},
		{"no systemd", func(i *integrations) { i.systemd = stubSystemd{ping: errors.New("no bus")} }, "FAIL  systemd bus"},
		{"unit missing", func(i *integrations) { i.systemd = stubSystemd{status: systemd.ErrNotFound} }, "FAIL  systemd unit ssh.service"},
		{"blocked http", func(i *integrations) {
			i.health = stubHealth{http: health.HTTPResult{Failure: health.FailureBlockedAddress}, dns: health.DNSResult{OK: true}}
		}, "allow_private"},
		{"bad status", func(i *integrations) {
			i.health = stubHealth{http: health.HTTPResult{StatusCode: 503, Failure: health.FailureUnexpectedStatus}, dns: health.DNSResult{OK: true}}
		}, "unexpected status 503"},
		{"dns failure", func(i *integrations) {
			i.health = stubHealth{http: health.HTTPResult{OK: true}, dns: health.DNSResult{Failure: health.FailureNotFound}}
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
	in.docker = stubDocker{status: docker.ErrNotFound}
	in.systemd = stubSystemd{status: systemd.ErrNotFound}
	var out bytes.Buffer
	if doctor(context.Background(), doctorConfig(t), in, &out, false) {
		t.Fatal("expected failure")
	}
	for _, want := range []string{"2 problems found.", "docker ps -a", "systemctl list-unit-files"} {
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
