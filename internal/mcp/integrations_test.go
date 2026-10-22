package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mhmdxsadk/latch/internal/config"
	"github.com/mhmdxsadk/latch/internal/docker"
	"github.com/mhmdxsadk/latch/internal/health"
	"github.com/mhmdxsadk/latch/internal/systemd"
)

// fakeDocker records the identifiers it receives so tests can prove the
// integration only ever sees policy-resolved values.
type fakeDocker struct {
	err        error
	errFor     map[string]error
	containers []string
	lines      int
	maxBytes   int
}

func (f *fakeDocker) Ping(context.Context) error { return f.err }
func (f *fakeDocker) Status(_ context.Context, c string) (docker.Status, error) {
	f.containers = append(f.containers, c)
	if err := f.errFor[c]; err != nil {
		return docker.Status{}, err
	}
	return docker.Status{State: "running", Running: true, Health: docker.Health{Status: "healthy"}}, f.err
}
func (f *fakeDocker) Logs(_ context.Context, c string, lines, maxBytes int) (docker.Logs, error) {
	f.containers = append(f.containers, c)
	f.lines, f.maxBytes = lines, maxBytes
	return docker.Logs{Entries: []docker.LogEntry{{Stream: "stdout", Text: "hello"}}}, f.err
}

type fakeSystemd struct {
	err   error
	units []string
}

func (f *fakeSystemd) Ping(context.Context) error { return f.err }
func (f *fakeSystemd) UnitStatus(_ context.Context, u string) (systemd.UnitStatus, error) {
	f.units = append(f.units, u)
	return systemd.UnitStatus{LoadState: "loaded", ActiveState: "active", SubState: "running"}, f.err
}

type fakeHealth struct {
	err     error
	targets []string
}

func (f *fakeHealth) HTTP(_ context.Context, t config.HTTPTarget) (health.HTTPResult, error) {
	f.targets = append(f.targets, t.URL)
	return health.HTTPResult{OK: true, StatusCode: 200}, f.err
}
func (f *fakeHealth) DNS(_ context.Context, t config.DNSTarget) (health.DNSResult, error) {
	f.targets = append(f.targets, t.Host)
	return health.DNSResult{OK: true, Addresses: []string{"192.0.2.1"}}, f.err
}

func structured[T any](t *testing.T, h *harness, tool string, args any) T {
	t.Helper()
	res, err := h.call(tool, args)
	if err != nil || res.IsError {
		t.Fatalf("%s: %v %s", tool, err, resultText(res))
	}
	var out T
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDockerToolsResolveThroughPolicy(t *testing.T) {
	fd := &fakeDocker{}
	h := newHarnessWith(t, func(d *Deps) { d.Docker = fd })

	st := structured[dockerStatusOut](t, h, "docker_status", map[string]any{"name": "web"})
	if st.Name != "web" || st.Status.State != "running" {
		t.Errorf("got %+v", st)
	}
	logs := structured[dockerLogsOut](t, h, "docker_logs", map[string]any{"name": "db", "lines": 50})
	if len(logs.Entries) != 1 || fd.lines != 50 || fd.maxBytes != config.DefaultMaxLogBytes {
		t.Errorf("got %+v lines=%d maxBytes=%d", logs, fd.lines, fd.maxBytes)
	}
	structured[dockerLogsOut](t, h, "docker_logs", map[string]any{"name": "db"})
	if fd.lines != 100 {
		t.Errorf("default lines = %d", fd.lines)
	}
	if strings.Join(fd.containers, ",") != "my-web,my-db,my-db" {
		t.Errorf("integration saw %v; want only configured container names", fd.containers)
	}
}

func TestDockerListToleratesPerServiceFailures(t *testing.T) {
	fd := &fakeDocker{errFor: map[string]error{
		"my-web": docker.ErrNotFound,
		"my-db":  errors.New("SECRET daemon detail"),
	}}
	h := newHarnessWith(t, func(d *Deps) { d.Docker = fd })
	res, err := h.call("docker_list", map[string]any{})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	b, _ := json.Marshal(res.StructuredContent)
	if strings.Contains(string(b), "SECRET") || strings.Contains(resultText(res), "SECRET") {
		t.Errorf("leaked: %s", b)
	}
	var out dockerListOut
	json.Unmarshal(b, &out)
	if len(out.Services) != 2 || out.Services[0].State != "not_found" || out.Services[1].State != "unknown" {
		t.Errorf("got %+v", out)
	}
}

func TestDockerRejections(t *testing.T) {
	fd := &fakeDocker{}
	h := newHarnessWith(t, func(d *Deps) { d.Docker = fd })
	cases := []struct {
		tool string
		args any
	}{
		{"docker_status", map[string]any{"name": "my-web"}}, // container name, not service name
		{"docker_status", map[string]any{"name": "../web"}},
		{"docker_status", map[string]any{"name": "web/../../etc"}},
		{"docker_status", map[string]any{"name": "WEB"}},
		{"docker_status", map[string]any{"name": "web", "container": "other"}},
		{"docker_status", map[string]any{}},
		{"docker_logs", map[string]any{"name": "web", "lines": 1001}},
		{"docker_logs", map[string]any{"name": "web", "lines": config.DefaultMaxLogLines + 1}},
		{"docker_logs", map[string]any{"name": "web", "lines": -1}},
		{"docker_logs", map[string]any{"name": "web", "lines": "all"}},
		{"docker_logs", map[string]any{"name": "web", "lines": 1.5}},
		{"docker_logs", map[string]any{"name": "web", "follow": true}},
		{"docker_logs", map[string]any{"name": "web", "max_bytes": 1 << 30}},
		{"docker_logs", map[string]any{"name": "nope"}},
	}
	for _, c := range cases {
		res, err := h.call(c.tool, c.args)
		if err == nil && !res.IsError {
			t.Errorf("%s %v: expected rejection", c.tool, c.args)
		}
	}
	if len(fd.containers) != 0 {
		t.Errorf("rejected calls reached the integration: %v", fd.containers)
	}
}

func TestSystemdTool(t *testing.T) {
	fs := &fakeSystemd{}
	h := newHarnessWith(t, func(d *Deps) { d.Systemd = fs })
	out := structured[unitStatusOut](t, h, "systemd_unit_status", map[string]any{"unit": "ssh.service"})
	if out.Unit != "ssh.service" || out.Status.ActiveState != "active" {
		t.Errorf("got %+v", out)
	}
	for _, u := range []string{"ssh", "sshd.service", "SSH.service", "../ssh.service", "ssh.service\n", "*.service", "ssh.service --now", ""} {
		res, err := h.call("systemd_unit_status", map[string]any{"unit": u})
		if err == nil && !res.IsError {
			t.Errorf("unit %q accepted", u)
		}
	}
	if len(fs.units) != 1 {
		t.Errorf("integration calls: %v", fs.units)
	}
}

func TestHealthTools(t *testing.T) {
	fh := &fakeHealth{}
	h := newHarnessWith(t, func(d *Deps) { d.Health = fh })
	hc := structured[httpCheckOut](t, h, "http_check", map[string]any{"name": "site"})
	dc := structured[dnsCheckOut](t, h, "dns_check", map[string]any{"name": "example"})
	if !hc.Result.OK || !dc.Result.OK || len(dc.Result.Addresses) != 1 {
		t.Errorf("got %+v %+v", hc, dc)
	}
	for _, c := range []struct {
		tool string
		args any
	}{
		{"http_check", map[string]any{"name": "https://evil.example/"}},
		{"http_check", map[string]any{"name": "site", "url": "http://169.254.169.254/"}},
		{"http_check", map[string]any{"url": "http://127.0.0.1/"}},
		{"http_check", map[string]any{"name": "example"}}, // a DNS target is not an HTTP target
		{"dns_check", map[string]any{"name": "evil.example"}},
		{"dns_check", map[string]any{"name": "example", "host": "internal.corp"}},
	} {
		res, err := h.call(c.tool, c.args)
		if err == nil && !res.IsError {
			t.Errorf("%s %v: expected rejection", c.tool, c.args)
		}
	}
	if strings.Join(fh.targets, ",") != "https://example.com/,example.com" {
		t.Errorf("integration saw %v", fh.targets)
	}
}

func TestMissingResourcesAndLeaks(t *testing.T) {
	secret := errors.New("SECRET-TOKEN hunter2 /var/lib/docker")
	h := newHarnessWith(t, func(d *Deps) {
		d.Docker = &fakeDocker{err: secret}
		d.Systemd = &fakeSystemd{err: secret}
		d.Health = &fakeHealth{err: secret}
	})
	for _, c := range []struct {
		tool string
		args any
	}{
		{"docker_status", map[string]any{"name": "web"}},
		{"docker_logs", map[string]any{"name": "web"}},
		{"systemd_unit_status", map[string]any{"unit": "ssh.service"}},
		{"http_check", map[string]any{"name": "site"}},
		{"dns_check", map[string]any{"name": "example"}},
	} {
		res, err := h.call(c.tool, c.args)
		text := ""
		switch {
		case err != nil:
			text = err.Error()
		case res.IsError:
			text = resultText(res)
		default:
			t.Fatalf("%s: expected an error", c.tool)
		}
		if strings.Contains(text, "SECRET") || strings.Contains(text, "hunter2") {
			t.Errorf("%s leaked: %q", c.tool, text)
		}
	}
	if a := h.audit.String(); strings.Contains(a, "SECRET") {
		t.Errorf("audit leaked: %s", a)
	}

	h = newHarnessWith(t, func(d *Deps) {
		d.Docker = &fakeDocker{err: docker.ErrNotFound}
		d.Systemd = &fakeSystemd{err: systemd.ErrNotFound}
	})
	for _, c := range []struct {
		tool string
		args any
	}{
		{"docker_status", map[string]any{"name": "web"}},
		{"systemd_unit_status", map[string]any{"unit": "ssh.service"}},
	} {
		res, err := h.call(c.tool, c.args)
		if err != nil || !res.IsError || !strings.Contains(resultText(res), "does not exist") {
			t.Errorf("%s: %v %s", c.tool, err, resultText(res))
		}
	}
	if !strings.Contains(h.audit.String(), `"reason":"not_found"`) {
		t.Errorf("audit: %s", h.audit)
	}
}
