package mcp

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhmdxsadk/latch/internal/approval"
	"github.com/mhmdxsadk/latch/internal/audit"
	"github.com/mhmdxsadk/latch/internal/config"
	"github.com/mhmdxsadk/latch/internal/docker"
	"github.com/mhmdxsadk/latch/internal/policy"
	"github.com/mhmdxsadk/latch/internal/systemd"
)

// actDocker is a thread-safe read client: executions run in goroutines.
// Methods other than Status come from fakeDocker and are not used here.
type actDocker struct {
	fakeDocker
	mu      sync.Mutex
	missing map[string]bool
	health  string // health status reported for every container; "" = healthy
	logs    []docker.LogEntry
	calls   []string
}

func (f *actDocker) Logs(context.Context, string, int, int) (docker.Logs, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return docker.Logs{Entries: slices.Clone(f.logs)}, nil
}

func (f *actDocker) setHealth(h string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.health = h
}

func (f *actDocker) Status(_ context.Context, c string) (docker.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	if f.missing[c] {
		return docker.Status{}, docker.ErrNotFound
	}
	h := f.health
	if h == "" {
		h = "healthy"
	}
	return docker.Status{State: "running", Running: true, Health: docker.Health{Status: h}}, nil
}
func (f *actDocker) setMissing(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.missing == nil {
		f.missing = map[string]bool{}
	}
	f.missing[name] = true
}

type fakeDockerActions struct {
	mu         sync.Mutex
	calls      []string
	target     docker.ComposeTarget
	targetErr  error
	composed   []docker.ComposeTarget
	pulls      []bool
	composeRes docker.ComposeResult
	composeErr error
	block      chan struct{}
}

func (f *fakeDockerActions) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeDockerActions) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeDockerActions) Restart(_ context.Context, n string) error {
	if f.block != nil {
		<-f.block
	}
	f.record("restart " + n)
	return nil
}
func (f *fakeDockerActions) Stop(_ context.Context, n string) (bool, error) {
	f.record("stop " + n)
	return true, nil
}
func (f *fakeDockerActions) Start(_ context.Context, n string) (bool, error) {
	f.record("start " + n)
	return false, nil
}
func (f *fakeDockerActions) ComposeTarget(_ context.Context, n string) (docker.ComposeTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "target "+n)
	t := f.target
	t.ConfigFiles = slices.Clone(t.ConfigFiles)
	return t, f.targetErr
}
func (f *fakeDockerActions) ComposeUp(_ context.Context, t docker.ComposeTarget, pull bool) (docker.ComposeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.composed = append(f.composed, t)
	f.pulls = append(f.pulls, pull)
	return f.composeRes, f.composeErr
}

// Methods other than UnitStatus come from fakeSystemd.
type actSystemd struct {
	fakeSystemd
	mu      sync.Mutex
	missing map[string]bool
}

func (f *actSystemd) UnitStatus(_ context.Context, u string) (systemd.UnitStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.missing[u] {
		return systemd.UnitStatus{}, systemd.ErrNotFound
	}
	return systemd.UnitStatus{LoadState: "loaded"}, nil
}

type fakeSystemdActions struct {
	mu    sync.Mutex
	calls []string
	res   systemd.JobResult
	err   error
}

func (f *fakeSystemdActions) do(verb, u string) (systemd.JobResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, verb+" "+u)
	if f.res.Result == "" {
		return systemd.JobResult{Result: "done", ActiveState: "active", SubState: "running"}, f.err
	}
	return f.res, f.err
}
func (f *fakeSystemdActions) Restart(_ context.Context, u string) (systemd.JobResult, error) {
	return f.do("restart", u)
}
func (f *fakeSystemdActions) Stop(_ context.Context, u string) (systemd.JobResult, error) {
	return f.do("stop", u)
}
func (f *fakeSystemdActions) Start(_ context.Context, u string) (systemd.JobResult, error) {
	return f.do("start", u)
}
func (f *fakeSystemdActions) Reload(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "daemon-reload")
	return nil
}
func (f *fakeSystemdActions) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

type actHarness struct {
	*harness
	store *approval.Store
	clk   *testClock
	dk    *actDocker
	da    *fakeDockerActions
	sd    *actSystemd
	sa    *fakeSystemdActions
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

const actionsConfig = `
server: {socket: /run/l.sock, socket_group: latch-agents}
docker:
  exclude: [secret-*]
  approve: [my-db]
systemd:
  exclude: [vault.service]
  approve: [cron.service]
`

func newActHarness(t *testing.T, autoWait time.Duration, extra ...func(*Deps)) *actHarness {
	t.Helper()
	cfg, err := config.Parse([]byte(actionsConfig))
	if err != nil {
		t.Fatal(err)
	}
	a := &actHarness{
		clk: &testClock{t: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)},
		dk:  &actDocker{}, da: &fakeDockerActions{},
		sd: &actSystemd{}, sa: &fakeSystemdActions{},
	}
	a.harness = newHarnessWith(t, func(d *Deps) {
		d.Policy = policy.New(cfg)
		d.Docker, d.DockerActions = a.dk, a.da
		d.Systemd, d.SystemdActions = a.sd, a.sa
		a.store = approval.New(approval.Options{Audit: d.Audit, Explain: ExplainActionError, Now: a.clk.now, AutoWait: autoWait})
		d.Actions = a.store
		for _, f := range extra {
			f(d)
		}
	})
	t.Cleanup(a.store.Wait)
	return a
}

func (a *actHarness) act(t *testing.T, tool string, args map[string]any) actionOut {
	t.Helper()
	return structured[actionOut](t, a.harness, tool, args)
}

// refusal calls a tool that must fail and returns the agent-visible text.
func (a *actHarness) refusal(t *testing.T, tool string, args any) string {
	t.Helper()
	res, err := a.call(tool, args)
	if err != nil {
		return err.Error()
	}
	if !res.IsError {
		t.Fatalf("%s %v: expected a refusal, got %s", tool, args, resultText(res))
	}
	return resultText(res)
}

func (a *actHarness) approve(t *testing.T, id int64) {
	t.Helper()
	if _, err := a.store.Approve(context.Background(), id, 0); err != nil {
		t.Fatal(err)
	}
	a.store.Wait()
}

// The agent-facing server has no way to approve or deny: no such tool, and
// no tool argument moves a pending request on.
func TestAgentCannotApprove(t *testing.T) {
	a := newActHarness(t, 5*time.Second)
	out := a.act(t, "docker_stop", map[string]any{"name": "my-web"})
	if out.Status != "pending" {
		t.Fatalf("got %+v", out)
	}
	res, err := a.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		for _, bad := range []string{"approve", "deny", "exec", "shell", "command"} {
			if strings.Contains(tool.Name, bad) {
				t.Errorf("tool %q must not exist", tool.Name)
			}
		}
	}
	for _, name := range []string{"approve", "action_approve", "latch_approve", "deny", "action_deny", "approve_action"} {
		if res, err := a.call(name, map[string]any{"id": out.ID}); err == nil && !res.IsError {
			t.Errorf("tool %q answered", name)
		}
	}
	for _, args := range []map[string]any{{"id": out.ID, "approve": true}, {"id": out.ID, "status": "approved"}} {
		a.refusal(t, "action_status", args)
	}
	a.store.Wait()
	if got := a.act(t, "action_status", map[string]any{"id": out.ID}); got.Status != "pending" || len(a.da.Calls()) != 0 {
		t.Errorf("request moved without the owner: %+v %v", got, a.da.Calls())
	}
}

func TestDockerRestartRunsAutomatically(t *testing.T) {
	a := newActHarness(t, 5*time.Second)
	out := a.act(t, "docker_restart", map[string]any{"name": "my-web"})
	if out.Status != "done" || out.Resource != "my-web" || out.ResourceType != "container" || out.Action != "docker_restart" {
		t.Fatalf("got %+v", out)
	}
	if out.Result == nil || out.Result.Summary != "container my-web restarted; state running, health healthy" || out.ExpiresAt != "" {
		t.Errorf("got %+v", out.Result)
	}
	if got := a.da.Calls(); !slices.Equal(got, []string{"restart my-web"}) {
		t.Errorf("calls %v", got)
	}
	st := a.act(t, "action_status", map[string]any{"id": out.ID})
	if st.Status != "done" || st.Message != "request 1 is done" || st.FinishedAt == "" {
		t.Errorf("status %+v", st)
	}
	if !strings.Contains(a.audit.String(), `"msg":"action_requested","id":1,"action":"docker_restart","resource":"container/my-web","needs_approval":false`) {
		t.Errorf("audit: %s", a.audit)
	}
}

func TestRestartRateLimitBecomesApproval(t *testing.T) {
	a := newActHarness(t, 5*time.Second)
	for i := range 5 {
		if out := a.act(t, "systemd_restart", map[string]any{"unit": "nginx.service"}); out.Status != "done" {
			t.Fatalf("restart %d: %+v", i+1, out)
		}
		a.clk.add(time.Minute)
	}
	out := a.act(t, "systemd_restart", map[string]any{"unit": "nginx.service"})
	if out.Status != "pending" || out.Message != "request 6 is waiting for approval" || out.ExpiresAt != "2026-10-24T13:05:00Z" {
		t.Fatalf("6th restart: %+v", out)
	}
	if n := len(a.sa.Calls()); n != 5 {
		t.Errorf("executed %d", n)
	}
	a.approve(t, out.ID)
	if got := a.act(t, "action_status", map[string]any{"id": out.ID}); got.Status != "done" || got.Result.Summary != "systemd job result done; unit is active (running)" {
		t.Errorf("got %+v", got)
	}
	// The window slides: an hour after the first restart, one runs again.
	a.clk.add(56 * time.Minute)
	if out := a.act(t, "systemd_restart", map[string]any{"unit": "nginx.service"}); out.Status != "done" {
		t.Errorf("after the window: %+v", out)
	}
}

func TestApproveListsForceApproval(t *testing.T) {
	a := newActHarness(t, 5*time.Second)
	for _, tc := range []struct {
		tool, key, name string
		pending         bool
	}{
		{"docker_restart", "name", "my-db", true},   // docker.approve
		{"docker_restart", "name", "my-web", false}, // not listed
		{"systemd_restart", "unit", "cron.service", true},
		{"systemd_restart", "unit", "nginx.service", false},
	} {
		out := a.act(t, tc.tool, map[string]any{tc.key: tc.name})
		if (out.Status == "pending") != tc.pending {
			t.Errorf("%s %s: %+v", tc.tool, tc.name, out)
		}
	}
	for _, u := range policy.BuiltinApproveUnits {
		u = strings.ReplaceAll(u, "*", "journald.service")
		if out := a.act(t, "systemd_restart", map[string]any{"unit": u}); out.Status != "pending" {
			t.Errorf("built-in %s: %+v", u, out)
		}
	}
	for _, c := range a.sa.Calls() {
		if c != "restart nginx.service" {
			t.Errorf("protected unit ran without approval: %s", c)
		}
	}
}

func TestStopAndStartAlwaysNeedApproval(t *testing.T) {
	a := newActHarness(t, 5*time.Second)
	var ids []int64
	for _, tc := range []struct{ tool, key, name string }{
		{"docker_stop", "name", "my-web"},
		{"docker_start", "name", "my-web"},
		{"systemd_stop", "unit", "nginx.service"},
		{"systemd_start", "unit", "nginx.service"},
	} {
		out := a.act(t, tc.tool, map[string]any{tc.key: tc.name})
		if out.Status != "pending" || out.ExpiresAt == "" {
			t.Fatalf("%s: %+v", tc.tool, out)
		}
		ids = append(ids, out.ID)
	}
	if len(a.da.Calls())+len(a.sa.Calls()) != 0 {
		t.Fatal("ran without approval")
	}
	for _, id := range ids {
		a.approve(t, id)
	}
	if got := a.da.Calls(); !slices.Equal(got, []string{"stop my-web", "start my-web"}) {
		t.Errorf("docker calls %v", got)
	}
	if got := a.sa.Calls(); !slices.Equal(got, []string{"stop nginx.service", "start nginx.service"}) {
		t.Errorf("systemd calls %v", got)
	}
	if got := a.act(t, "action_status", map[string]any{"id": ids[1]}); got.Result.Summary != "container my-web was already running; state running, health healthy" {
		t.Errorf("got %+v", got.Result)
	}
	list := structured[actionsListOut](t, a.harness, "actions_list", map[string]any{})
	if len(list.Requests) != 4 || list.Requests[0].ID != ids[0] {
		t.Errorf("list %+v", list)
	}
}

// Excluded and missing resources give the same answer for every change tool.
func TestExcludedRefusedLikeMissing(t *testing.T) {
	a := newActHarness(t, 5*time.Second)
	a.dk.setMissing("gone")
	a.da.targetErr = docker.ErrNotFound
	a.sd.missing = map[string]bool{"gone.service": true}
	for _, tc := range []struct{ tool, key, excluded, missing string }{
		{"docker_restart", "name", "secret-vault", "gone"},
		{"docker_stop", "name", "secret-vault", "gone"},
		{"docker_start", "name", "secret-vault", "gone"},
		{"docker_compose_up", "name", "secret-vault", "gone"},
		{"systemd_restart", "unit", "vault.service", "gone.service"},
		{"systemd_stop", "unit", "vault.service", "gone.service"},
		{"systemd_start", "unit", "vault.service", "gone.service"},
	} {
		ex := a.refusal(t, tc.tool, map[string]any{tc.key: tc.excluded})
		mi := a.refusal(t, tc.tool, map[string]any{tc.key: tc.missing})
		if ex != mi || !strings.Contains(ex, "does not exist or is not allowed") {
			t.Errorf("%s: excluded %q, missing %q", tc.tool, ex, mi)
		}
	}
	if len(a.store.List(false, 0)) != 0 {
		t.Error("refused requests were stored")
	}
	for _, c := range a.da.Calls() {
		if strings.Contains(c, "secret") {
			t.Errorf("excluded container reached the integration: %s", c)
		}
	}
}

func TestInvalidInputRejectedBeforeIntegration(t *testing.T) {
	a := newActHarness(t, 5*time.Second)
	bad := []any{"", "../web", "web;id", "-v", "web name", "my-web\n", strings.Repeat("a", 300), 5, nil}
	for _, tool := range []string{"docker_restart", "docker_stop", "docker_start", "docker_compose_up"} {
		for _, n := range bad {
			a.refusal(t, tool, map[string]any{"name": n})
		}
		a.refusal(t, tool, map[string]any{"name": "my-web", "command": "sh"})
		a.refusal(t, tool, map[string]any{})
	}
	a.refusal(t, "docker_compose_up", map[string]any{"name": "my-web", "pull": "yes"})
	a.refusal(t, "docker_compose_up", map[string]any{"name": "my-web", "file": "/tmp/x.yaml"})
	for _, tool := range []string{"systemd_restart", "systemd_stop", "systemd_start"} {
		for _, u := range []any{"ssh", "../ssh.service", "*.service", "ssh.service --now", "a b.service", 1} {
			a.refusal(t, tool, map[string]any{"unit": u})
		}
		a.refusal(t, tool, map[string]any{"unit": "nginx.service", "mode": "isolate"})
	}
	for _, id := range []any{0, -1, "1", 1.5, nil} {
		a.refusal(t, "action_status", map[string]any{"id": id})
	}
	if msg := a.refusal(t, "action_status", map[string]any{"id": 99}); !strings.Contains(msg, "does not exist") {
		t.Errorf("unknown id: %q", msg)
	}
	a.dk.mu.Lock()
	n := len(a.dk.calls)
	a.dk.mu.Unlock()
	if n != 0 || len(a.da.Calls()) != 0 || len(a.sa.Calls()) != 0 {
		t.Errorf("rejected input reached integrations: %d %v %v", n, a.da.Calls(), a.sa.Calls())
	}
}

func composeTarget() docker.ComposeTarget {
	return docker.ComposeTarget{Project: "media", Service: "web", WorkingDir: "/srv/media",
		ConfigFiles: []string{"/srv/media/compose.yaml"}, Siblings: []string{"my-web"}}
}

func TestComposeUpWaitsAndRunsStoredTarget(t *testing.T) {
	a := newActHarness(t, 5*time.Second)
	a.da.target = composeTarget()
	a.da.composeRes = docker.ComposeResult{Output: []string{"Container my-web Recreated", "Container my-web Started"}}
	out := a.act(t, "docker_compose_up", map[string]any{"name": "my-web", "pull": true})
	if out.Status != "pending" {
		t.Fatalf("got %+v", out)
	}
	req, _ := a.store.Get(out.ID)
	want := "docker compose --project-name media --project-directory /srv/media -f /srv/media/compose.yaml up -d --no-deps --pull always web"
	if !slices.Contains(req.Details, want) || !slices.Equal(req.Params, []approval.Param{{Name: "pull", Value: "true"}}) {
		t.Errorf("owner sees %q %+v", req.Details, req.Params)
	}
	a.approve(t, out.ID)
	if len(a.da.composed) != 1 || !a.da.composed[0].Same(composeTarget()) || !a.da.pulls[0] {
		t.Fatalf("ran %+v %v", a.da.composed, a.da.pulls)
	}
	got := a.act(t, "action_status", map[string]any{"id": out.ID})
	if got.Status != "done" || got.Result.Summary != "docker compose exited with status 0" || len(got.Result.Details) != 2 {
		t.Errorf("got %+v", got)
	}
}

func TestComposeUpFailureReportsExitStatus(t *testing.T) {
	a := newActHarness(t, 5*time.Second)
	a.da.target = composeTarget()
	a.da.composeRes = docker.ComposeResult{ExitCode: 1, Output: []string{"yaml: line 3: did not find expected key"}}
	a.da.composeErr = docker.ErrComposeFailed
	out := a.act(t, "docker_compose_up", map[string]any{"name": "my-web"})
	a.approve(t, out.ID)
	got := a.act(t, "action_status", map[string]any{"id": out.ID})
	if got.Status != "failed" || got.Result.Summary != "docker compose exited with status 1" || len(got.Result.Details) != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestComposeUpRefusals(t *testing.T) {
	a := newActHarness(t, 5*time.Second)
	a.da.targetErr = docker.ErrNotCompose
	if msg := a.refusal(t, "docker_compose_up", map[string]any{"name": "my-web"}); msg != docker.ErrNotCompose.Error() {
		t.Errorf("no labels: %q", msg)
	}
	a.da.targetErr = docker.ErrBadCompose
	if msg := a.refusal(t, "docker_compose_up", map[string]any{"name": "my-web"}); msg != docker.ErrBadCompose.Error() {
		t.Errorf("bad labels: %q", msg)
	}
	// A service that also runs an excluded container is refused like a
	// missing one.
	a.da.targetErr = nil
	a.da.target = composeTarget()
	a.da.target.Siblings = []string{"my-web", "secret-vault"}
	if msg := a.refusal(t, "docker_compose_up", map[string]any{"name": "my-web"}); !strings.Contains(msg, "does not exist or is not allowed") {
		t.Errorf("excluded sibling: %q", msg)
	}
	if len(a.store.List(false, 0)) != 0 {
		t.Error("refused compose request stored")
	}
}

func TestComposeLabelsChangedBeforeApproval(t *testing.T) {
	a := newActHarness(t, 5*time.Second)
	a.da.target = composeTarget()
	out := a.act(t, "docker_compose_up", map[string]any{"name": "my-web"})
	a.da.mu.Lock()
	a.da.target.ConfigFiles = []string{"/srv/other/compose.yaml"}
	a.da.mu.Unlock()
	a.approve(t, out.ID)
	got := a.act(t, "action_status", map[string]any{"id": out.ID})
	if got.Status != "failed" || !strings.Contains(got.Result.Summary, "compose labels changed") || len(a.da.composed) != 0 {
		t.Errorf("got %+v, ran %v", got, a.da.composed)
	}
}

func TestResourceGoneBeforeApprovalFails(t *testing.T) {
	a := newActHarness(t, 5*time.Second)
	out := a.act(t, "docker_stop", map[string]any{"name": "my-web"})
	a.dk.setMissing("my-web")
	a.approve(t, out.ID)
	got := a.act(t, "action_status", map[string]any{"id": out.ID})
	if got.Status != "failed" || got.Result.Summary != "the resource no longer exists or is not allowed" || len(a.da.Calls()) != 0 {
		t.Errorf("got %+v %v", got, a.da.Calls())
	}
	if !strings.Contains(a.audit.String(), `"outcome":"failed","reason":"not_found"`) {
		t.Errorf("audit: %s", a.audit)
	}
}

func TestSystemdJobFailureReported(t *testing.T) {
	a := newActHarness(t, 5*time.Second)
	a.sa.res = systemd.JobResult{Result: "failed", ActiveState: "failed", SubState: "failed"}
	a.sa.err = systemd.ErrJobFailed
	out := a.act(t, "systemd_restart", map[string]any{"unit": "nginx.service"})
	if out.Status != "failed" || out.Result.Summary != "systemd job result failed; unit is failed (failed)" {
		t.Errorf("got %+v", out)
	}
}

func TestBusyResourceRefused(t *testing.T) {
	a := newActHarness(t, 10*time.Millisecond)
	a.da.block = make(chan struct{})
	out := a.act(t, "docker_restart", map[string]any{"name": "my-web"})
	if out.Status != "running" || !strings.Contains(out.Message, "running") {
		t.Fatalf("got %+v", out)
	}
	if msg := a.refusal(t, "docker_restart", map[string]any{"name": "my-web"}); !strings.Contains(msg, "in progress") {
		t.Errorf("got %q", msg)
	}
	close(a.da.block)
	a.store.Wait()
	if got := a.act(t, "action_status", map[string]any{"id": out.ID}); got.Status != "done" {
		t.Errorf("got %+v", got)
	}
}

// Requests made over the socket record the kernel-verified peer.
func TestSubmitRecordsPeer(t *testing.T) {
	a := newActHarness(t, 5*time.Second)
	d := &Deps{Actions: a.store}
	ctx := audit.WithPeer(context.Background(), audit.Peer{UID: 1000, PID: 4242})
	out, err := d.submit(ctx, containerAction{d: &Deps{Policy: policy.New(mustConfig(t)), Docker: a.dk}, verb: "stop", name: "my-web", mode: approval.Always})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := a.store.Get(out.ID)
	if r.RequestedBy != (approval.Requester{UID: 1000, PID: 4242}) {
		t.Errorf("got %+v", r.RequestedBy)
	}
}

func mustConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(actionsConfig))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestExplainNeverPassesErrorText(t *testing.T) {
	secret := errors.New("SECRET /etc/shadow")
	if s, r := ExplainActionError(secret); strings.Contains(s, "SECRET") || r != "integration_error" {
		t.Errorf("%q %q", s, r)
	}
}
