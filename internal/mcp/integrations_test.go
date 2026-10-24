package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

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
	names      []string // returned by List; defaults to web, db and an excluded one
	containers []string
	lines      int
	maxBytes   int
	events     []docker.Event // returned by Events, after the visibility filter
	since      time.Time
	until      time.Time
	ids        map[string]string
	usage      docker.Usage
}

func (f *fakeDocker) Ping(context.Context) error { return f.err }
func (f *fakeDocker) List(context.Context) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.names != nil {
		return f.names, nil
	}
	return []string{"my-db", "my-web", "secret-vault"}, nil
}
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

func (f *fakeDocker) Usage(_ context.Context, c string) (docker.Usage, error) {
	f.containers = append(f.containers, c)
	return f.usage, f.err
}

func (f *fakeDocker) Events(_ context.Context, since, until time.Time, visible func(string) bool, limit int) (docker.Events, error) {
	f.since, f.until = since, until
	if f.err != nil {
		return docker.Events{}, f.err
	}
	out := docker.Events{Events: []docker.Event{}}
	for _, e := range f.events {
		if visible(e.Container) {
			out.Events = append(out.Events, e)
		}
	}
	if len(out.Events) > limit {
		out.Events, out.Truncated = out.Events[len(out.Events)-limit:], true
	}
	return out, nil
}

func (f *fakeDocker) IDs(context.Context) (map[string]string, error) {
	return f.ids, f.err
}

func (f *fakeDocker) Settings(_ context.Context, c string) (docker.Settings, error) {
	f.containers = append(f.containers, c)
	return docker.Settings{
		Image: "nginx:1.27", RestartPolicy: "always",
		Ports: []docker.Port{}, Mounts: []docker.Mount{}, Networks: []docker.Network{}, Labels: map[string]string{},
	}, f.err
}

type fakeSystemd struct {
	err   error
	units []string
}

func (f *fakeSystemd) Ping(context.Context) error { return f.err }
func (f *fakeSystemd) List(context.Context) ([]systemd.ListedUnit, error) {
	return []systemd.ListedUnit{{Name: "ssh.service", ActiveState: "active"}, {Name: "vault.service", ActiveState: "active"}}, f.err
}
func (f *fakeSystemd) UnitStatus(_ context.Context, u string) (systemd.UnitStatus, error) {
	f.units = append(f.units, u)
	return systemd.UnitStatus{LoadState: "loaded", ActiveState: "active", SubState: "running"}, f.err
}

type fakeHealth struct {
	err     error
	targets []string
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

	st := structured[dockerStatusOut](t, h, "docker_status", map[string]any{"name": "my-web"})
	if st.Name != "my-web" || st.Status.State != "running" {
		t.Errorf("got %+v", st)
	}
	logs := structured[dockerLogsOut](t, h, "docker_logs", map[string]any{"name": "my-db", "lines": 50})
	if len(logs.Entries) != 1 || fd.lines != 50 || fd.maxBytes != config.DefaultMaxLogBytes {
		t.Errorf("got %+v lines=%d maxBytes=%d", logs, fd.lines, fd.maxBytes)
	}
	structured[dockerLogsOut](t, h, "docker_logs", map[string]any{"name": "my-db"})
	if fd.lines != 100 {
		t.Errorf("default lines = %d", fd.lines)
	}
	set := structured[dockerSettingsOut](t, h, "docker_settings", map[string]any{"name": "my-web"})
	if set.Name != "my-web" || set.Settings.RestartPolicy != "always" {
		t.Errorf("got %+v", set)
	}
	if strings.Join(fd.containers, ",") != "my-web,my-db,my-db,my-web" {
		t.Errorf("integration saw %v; want only the requested container names", fd.containers)
	}
}

func TestDockerListHidesExcluded(t *testing.T) {
	fd := &fakeDocker{}
	h := newHarnessWith(t, func(d *Deps) { d.Docker = fd })
	out := structured[dockerListOut](t, h, "docker_list", map[string]any{})
	if len(out.Services) != 2 || out.Services[0].Name != "my-db" || out.Services[1].Name != "my-web" {
		t.Errorf("got %+v", out)
	}
	for _, c := range fd.containers {
		if c == "secret-vault" {
			t.Error("an excluded container was inspected")
		}
	}
}

func TestDockerListToleratesPerServiceFailures(t *testing.T) {
	fd := &fakeDocker{errFor: map[string]error{
		"my-db":  docker.ErrNotFound,
		"my-web": errors.New("SECRET daemon detail"),
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
	if a := h.audit.String(); !strings.Contains(a, `"outcome":"error","reason":"partial"`) || strings.Contains(a, "SECRET") {
		t.Errorf("audit: %s", a)
	}
}

func TestDockerListAllFailedIsAnError(t *testing.T) {
	fd := &fakeDocker{errFor: map[string]error{
		"my-db":  errors.New("SECRET daemon detail"),
		"my-web": errors.New("SECRET daemon detail"),
	}}
	h := newHarnessWith(t, func(d *Deps) { d.Docker = fd })
	res, err := h.call("docker_list", map[string]any{})
	if err != nil || !res.IsError || strings.Contains(resultText(res), "SECRET") {
		t.Fatalf("want a sanitized tool error, got %v %s", err, resultText(res))
	}
	if a := h.audit.String(); !strings.Contains(a, `"reason":"integration_error"`) || strings.Contains(a, `"outcome":"ok"`) {
		t.Errorf("audit: %s", a)
	}
}

func TestDockerListMissingOnlyIsOK(t *testing.T) {
	fd := &fakeDocker{errFor: map[string]error{"my-db": docker.ErrNotFound, "my-web": docker.ErrNotFound}}
	h := newHarnessWith(t, func(d *Deps) { d.Docker = fd })
	out := structured[dockerListOut](t, h, "docker_list", map[string]any{})
	if len(out.Services) != 2 || !strings.Contains(h.audit.String(), `"outcome":"ok"`) {
		t.Errorf("got %+v, audit %s", out, h.audit.String())
	}
}

func TestDockerRejections(t *testing.T) {
	fd := &fakeDocker{}
	h := newHarnessWith(t, func(d *Deps) { d.Docker = fd })
	cases := []struct {
		tool string
		args any
	}{
		{"docker_status", map[string]any{"name": "secret-vault"}}, // excluded
		{"docker_logs", map[string]any{"name": "secret-vault"}},
		{"docker_settings", map[string]any{"name": "secret-vault"}},
		{"docker_settings", map[string]any{"name": "../web"}},
		{"docker_status", map[string]any{"name": "secret-"}},
		{"docker_status", map[string]any{"name": "../web"}},
		{"docker_status", map[string]any{"name": "web/../../etc"}},
		{"docker_status", map[string]any{"name": "web;id"}},
		{"docker_status", map[string]any{"name": "-v"}},
		{"docker_status", map[string]any{"name": "web", "container": "other"}},
		{"docker_status", map[string]any{}},
		{"docker_logs", map[string]any{"name": "my-web", "lines": 1001}},
		{"docker_logs", map[string]any{"name": "my-web", "lines": config.DefaultMaxLogLines + 1}},
		{"docker_logs", map[string]any{"name": "my-web", "lines": -1}},
		{"docker_logs", map[string]any{"name": "my-web", "lines": "all"}},
		{"docker_logs", map[string]any{"name": "my-web", "lines": 1.5}},
		{"docker_logs", map[string]any{"name": "my-web", "follow": true}},
		{"docker_logs", map[string]any{"name": "my-web", "max_bytes": 1 << 30}},
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
	for _, u := range []string{"vault.service", "ssh", "../ssh.service", "ssh.service\n", "*.service", "ssh.service --now", ""} {
		res, err := h.call("systemd_unit_status", map[string]any{"unit": u})
		if err == nil && !res.IsError {
			t.Errorf("unit %q accepted", u)
		}
	}
	if len(fs.units) != 1 {
		t.Errorf("integration calls: %v", fs.units)
	}
}

func TestSystemdListHidesExcluded(t *testing.T) {
	h := newHarnessWith(t, func(d *Deps) {})
	out := structured[unitListOut](t, h, "systemd_list", map[string]any{})
	if len(out.Units) != 1 || out.Units[0].Name != "ssh.service" {
		t.Errorf("got %+v", out)
	}
}

// An excluded resource must look exactly like one that does not exist.
func TestExcludedLooksLikeMissing(t *testing.T) {
	h := newHarnessWith(t, func(d *Deps) { d.Docker = &fakeDocker{err: docker.ErrNotFound} })
	excluded, err := h.call("docker_status", map[string]any{"name": "secret-vault"})
	if err != nil || !excluded.IsError {
		t.Fatalf("%v %+v", err, excluded)
	}
	missing, err := h.call("docker_status", map[string]any{"name": "my-web"})
	if err != nil || !missing.IsError {
		t.Fatalf("%v %+v", err, missing)
	}
	if resultText(excluded) != resultText(missing) {
		t.Errorf("excluded %q differs from missing %q", resultText(excluded), resultText(missing))
	}
}

func TestHealthTools(t *testing.T) {
	fh := &fakeHealth{}
	h := newHarnessWith(t, func(d *Deps) { d.Health = fh })
	dc := structured[dnsCheckOut](t, h, "dns_check", map[string]any{"name": "example"})
	if !dc.Result.OK || len(dc.Result.Addresses) != 1 {
		t.Errorf("got %+v", dc)
	}
	for _, c := range []struct {
		tool string
		args any
	}{
		{"http_check", map[string]any{"name": "example"}}, // there is no such tool any more
		{"dns_check", map[string]any{"name": "evil.example"}},
		{"dns_check", map[string]any{"name": "example", "host": "internal.corp"}},
	} {
		res, err := h.call(c.tool, c.args)
		if err == nil && !res.IsError {
			t.Errorf("%s %v: expected rejection", c.tool, c.args)
		}
	}
	if strings.Join(fh.targets, ",") != "example.com" {
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
		{"docker_list", map[string]any{}},
		{"docker_status", map[string]any{"name": "my-web"}},
		{"docker_logs", map[string]any{"name": "my-web"}},
		{"systemd_list", map[string]any{}},
		{"systemd_unit_status", map[string]any{"unit": "ssh.service"}},
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
		{"docker_status", map[string]any{"name": "my-web"}},
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

// blockingDocker holds every Status call until released.
type blockingDocker struct {
	fakeDocker
	entered chan struct{}
	release chan struct{}
}

func (b *blockingDocker) Status(ctx context.Context, c string) (docker.Status, error) {
	b.entered <- struct{}{}
	<-b.release
	return docker.Status{State: "running"}, nil
}

func TestConcurrentCallsAreBounded(t *testing.T) {
	bd := &blockingDocker{entered: make(chan struct{}), release: make(chan struct{})}
	h := newHarnessWith(t, func(d *Deps) { d.Docker = bd })
	done := make(chan struct{})
	for range MaxConcurrentCalls {
		go func() {
			h.cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "docker_status", Arguments: map[string]any{"name": "my-web"}})
			done <- struct{}{}
		}()
		<-bd.entered
	}
	res, err := h.call("host_load", map[string]any{})
	if err == nil && !res.IsError {
		t.Error("call beyond the limit was served")
	} else if err == nil && !strings.Contains(resultText(res), "too many") {
		t.Errorf("unexpected message %q", resultText(res))
	}
	close(bd.release)
	for range MaxConcurrentCalls {
		<-done
	}
	if res, err := h.call("host_load", map[string]any{}); err != nil || res.IsError {
		t.Errorf("slot not released: %v", err)
	}
	if !strings.Contains(h.audit.String(), `"reason":"busy"`) {
		t.Error("busy refusal not audited")
	}
}

func TestDockerUsage(t *testing.T) {
	fd := &fakeDocker{usage: docker.Usage{CPUPercent: 12.5, MemoryUsedBytes: 100, PIDs: 3}}
	h := newHarnessWith(t, func(d *Deps) { d.Docker = fd })
	out := structured[dockerUsageOut](t, h, "docker_usage", map[string]any{"name": "my-web"})
	if out.Name != "my-web" || out.Usage.CPUPercent != 12.5 || out.Usage.PIDs != 3 {
		t.Errorf("got %+v", out)
	}
	for _, name := range []string{"secret-vault", "../x", "", "a b"} {
		res, err := h.call("docker_usage", map[string]any{"name": name})
		if err == nil && !res.IsError {
			t.Errorf("name %q accepted", name)
		}
	}
	if strings.Join(fd.containers, ",") != "my-web" {
		t.Errorf("integration saw %v", fd.containers)
	}
}

func TestDockerUsageExcludedReadsAsMissing(t *testing.T) {
	h := newHarnessWith(t, func(d *Deps) { d.Docker = &fakeDocker{err: docker.ErrNotFound} })
	excluded, _ := h.call("docker_usage", map[string]any{"name": "secret-vault"})
	missing, _ := h.call("docker_usage", map[string]any{"name": "my-web"})
	if !excluded.IsError || !missing.IsError || resultText(excluded) != resultText(missing) {
		t.Errorf("excluded %q, missing %q", resultText(excluded), resultText(missing))
	}
}

func TestDockerEvents(t *testing.T) {
	zero := 0
	fd := &fakeDocker{events: []docker.Event{
		{Time: "2024-01-01T00:00:00Z", Container: "my-web", Action: "die", ExitCode: &zero},
		{Time: "2024-01-01T00:00:01Z", Container: "secret-vault", Action: "oom"},
		{Time: "2024-01-01T00:00:02Z", Container: "my-db", Action: "health_status", Health: "unhealthy"},
	}}
	h := newHarnessWith(t, func(d *Deps) { d.Docker = fd })
	out := structured[dockerEventsOut](t, h, "docker_events", map[string]any{})
	if out.Hours != 1 || len(out.Events) != 2 || out.Events[0].Container != "my-web" || out.Events[1].Health != "unhealthy" {
		t.Errorf("got %+v", out)
	}
	if got := fd.until.Sub(fd.since); got != time.Hour {
		t.Errorf("window = %v", got)
	}
	res, _ := h.call("docker_events", map[string]any{})
	if strings.Contains(resultText(res), "secret-vault") {
		t.Errorf("excluded container leaked: %s", resultText(res))
	}
	out = structured[dockerEventsOut](t, h, "docker_events", map[string]any{"hours": 24})
	if got := fd.until.Sub(fd.since); got != 24*time.Hour || out.Hours != 24 {
		t.Errorf("window = %v", got)
	}
}

func TestDockerEventsEmptyIsNonNilAndCapped(t *testing.T) {
	fd := &fakeDocker{}
	h := newHarnessWith(t, func(d *Deps) { d.Docker = fd })
	res, err := h.call("docker_events", map[string]any{})
	if err != nil || res.IsError {
		t.Fatal(err, resultText(res))
	}
	b, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(b), `"events":[]`) {
		t.Errorf("got %s", b)
	}
	for i := range 250 {
		fd.events = append(fd.events, docker.Event{Container: "my-web", Action: "start", Time: fmt.Sprint(i)})
	}
	out := structured[dockerEventsOut](t, h, "docker_events", map[string]any{})
	if len(out.Events) != 200 || !out.Truncated || out.Events[199].Time != "249" {
		t.Errorf("len=%d truncated=%v", len(out.Events), out.Truncated)
	}
}

func TestDockerEventsRejections(t *testing.T) {
	fd := &fakeDocker{}
	h := newHarnessWith(t, func(d *Deps) { d.Docker = fd })
	for _, args := range []any{
		map[string]any{"hours": 25}, map[string]any{"hours": -1}, map[string]any{"hours": "all"},
		map[string]any{"hours": 1.5}, map[string]any{"since": 1},
	} {
		res, err := h.call("docker_events", args)
		if err == nil && !res.IsError {
			t.Errorf("%v accepted", args)
		}
	}
	if !fd.since.IsZero() {
		t.Error("rejected call reached the integration")
	}
}
