package main

import (
	"bytes"
	"context"
	"errors"
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
server: {socket: ` + t.TempDir() + `/l.sock, allowed_uids: [1]}
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
	}
}

func TestDoctorAllHealthy(t *testing.T) {
	var out bytes.Buffer
	if !doctor(context.Background(), doctorConfig(t), healthy(), &out) {
		t.Fatalf("expected success:\n%s", out.String())
	}
	for _, want := range []string{"docker daemon", "docker service web", "systemd unit ssh.service", "http check site", "dns check example"} {
		if !strings.Contains(out.String(), "ok    "+want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}

func TestDoctorReportsFailures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*integrations)
		want   string
	}{
		{"docker down", func(i *integrations) { i.docker = stubDocker{ping: errors.New("dial failed")} }, "FAIL  docker daemon"},
		{"container missing", func(i *integrations) { i.docker = stubDocker{status: docker.ErrNotFound} }, "FAIL  docker service web"},
		{"no systemd", func(i *integrations) { i.systemd = stubSystemd{ping: errors.New("no bus")} }, "FAIL  systemd"},
		{"unit missing", func(i *integrations) { i.systemd = stubSystemd{status: systemd.ErrNotFound} }, "FAIL  systemd unit ssh.service"},
		{"blocked http", func(i *integrations) {
			i.health = stubHealth{http: health.HTTPResult{Failure: health.FailureBlockedAddress}, dns: health.DNSResult{OK: true}}
		}, "allow_private"},
		{"bad status", func(i *integrations) {
			i.health = stubHealth{http: health.HTTPResult{StatusCode: 503, Failure: health.FailureUnexpectedStatus}, dns: health.DNSResult{OK: true}}
		}, "unexpected status 503"},
		{"dns failure", func(i *integrations) {
			i.health = stubHealth{http: health.HTTPResult{OK: true}, dns: health.DNSResult{Failure: health.FailureNotFound}}
		}, "FAIL  dns check example: not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := healthy()
			tt.mutate(&in)
			var out bytes.Buffer
			if doctor(context.Background(), doctorConfig(t), in, &out) {
				t.Fatalf("expected failure:\n%s", out.String())
			}
			if !strings.Contains(out.String(), tt.want) {
				t.Errorf("want %q in:\n%s", tt.want, out.String())
			}
		})
	}
}
