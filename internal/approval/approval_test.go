package approval

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhmdxsadk/latch/internal/audit"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeAction records executions. block, when set, holds Execute until closed.
type fakeAction struct {
	kind     string
	res      Resource
	params   []Param
	mode     Mode
	checkErr error
	// checkAt fails Check on the given call number (1-based), 0 = never.
	checkAt  int32
	checks   atomic.Int32
	execErr  error
	block    chan struct{}
	executed *atomic.Int32
	started  chan struct{}
}

func (f *fakeAction) Kind() string       { return f.kind }
func (f *fakeAction) Resource() Resource { return f.res }
func (f *fakeAction) Params() []Param    { return f.params }
func (f *fakeAction) Describe() []string { return []string{f.kind + " " + f.res.Name} }
func (f *fakeAction) Mode() Mode         { return f.mode }
func (f *fakeAction) Check(context.Context) error {
	n := f.checks.Add(1)
	if f.checkAt != 0 && n == f.checkAt {
		return errors.New("vanished")
	}
	return f.checkErr
}

func (f *fakeAction) Execute(ctx context.Context) (Result, error) {
	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	if f.executed != nil {
		f.executed.Add(1)
	}
	if f.execErr != nil {
		return Result{}, f.execErr
	}
	return Result{Summary: f.kind + " done"}, nil
}

func restart(name string, n *atomic.Int32) *fakeAction {
	return &fakeAction{kind: "docker_restart", res: Resource{"container", name}, mode: RateLimited, executed: n}
}

func stop(name string, n *atomic.Int32) *fakeAction {
	return &fakeAction{kind: "docker_stop", res: Resource{"container", name}, mode: Always, executed: n}
}

type harness struct {
	s     *Store
	clk   *clock
	audit *lockedBuf
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newStore(t *testing.T, o Options) harness {
	t.Helper()
	clk := &clock{t: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	buf := &lockedBuf{}
	o.Now = clk.now
	o.Audit = audit.New(slog.New(slog.NewJSONHandler(buf, nil)))
	if o.AutoWait == 0 {
		o.AutoWait = 5 * time.Second
	}
	s := New(o)
	t.Cleanup(s.Wait)
	return harness{s: s, clk: clk, audit: buf}
}

var agent = Requester{UID: 1000, PID: 4242}

func TestAutomaticRestartRunsAndReportsDone(t *testing.T) {
	h := newStore(t, Options{})
	var n atomic.Int32
	r, err := h.s.Submit(context.Background(), agent, restart("web", &n))
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusDone || r.NeedsApproval || n.Load() != 1 || r.Result == nil || r.Result.Summary != "docker_restart done" {
		t.Fatalf("got %+v executed=%d", r, n.Load())
	}
	if r.ID != 1 || r.RequestedBy != agent {
		t.Errorf("got %+v", r)
	}
	a := h.audit.String()
	for _, want := range []string{`"msg":"action_requested","id":1,"action":"docker_restart","resource":"container/web","needs_approval":false`, `"msg":"action_executed","id":1,"outcome":"done"`} {
		if !strings.Contains(a, want) {
			t.Errorf("audit missing %s:\n%s", want, a)
		}
	}
}

func TestRateLimitThenPendingAndWindowSlides(t *testing.T) {
	h := newStore(t, Options{})
	var n atomic.Int32
	for i := range 5 {
		r, err := h.s.Submit(context.Background(), agent, restart("web", &n))
		if err != nil || r.Status != StatusDone {
			t.Fatalf("restart %d: %v %+v", i+1, err, r)
		}
		h.clk.add(10 * time.Minute)
	}
	// 6th within the hour (first one was 50 minutes ago): pending.
	r, err := h.s.Submit(context.Background(), agent, restart("web", &n))
	if err != nil || r.Status != StatusPending || !r.NeedsApproval {
		t.Fatalf("6th restart: %v %+v", err, r)
	}
	// Another resource has its own budget.
	if r, _ := h.s.Submit(context.Background(), agent, restart("db", &n)); r.Status != StatusDone {
		t.Fatalf("other resource: %+v", r)
	}
	// Approving the 6th counts as an executed restart too.
	if _, err := h.s.Approve(context.Background(), r.ID, 0); err != nil {
		t.Fatal(err)
	}
	h.s.Wait()
	// After the first restart leaves the window, one slot opens, but the
	// approved restart filled it: still pending until two leave the window.
	h.clk.add(10 * time.Minute) // first restart is now 60 minutes old
	r, _ = h.s.Submit(context.Background(), agent, restart("web", &n))
	if r.Status != StatusPending {
		t.Fatalf("approved restart must count: %+v", r)
	}
	h.clk.add(10 * time.Minute) // second restart left the window too
	r, _ = h.s.Submit(context.Background(), agent, restart("web", &n))
	if r.Status != StatusDone {
		t.Fatalf("window must slide: %+v", r)
	}
	if n.Load() != 8 {
		t.Errorf("executed %d, want 8", n.Load())
	}
}

func TestFailedRestartsCount(t *testing.T) {
	h := newStore(t, Options{RateLimit: 2})
	for range 2 {
		a := restart("web", nil)
		a.execErr = errors.New("boom")
		if r, _ := h.s.Submit(context.Background(), agent, a); r.Status != StatusFailed {
			t.Fatalf("got %+v", r)
		}
	}
	if r, _ := h.s.Submit(context.Background(), agent, restart("web", nil)); r.Status != StatusPending {
		t.Fatalf("failed executions must count: %+v", r)
	}
}

func TestAlwaysActionsWaitForApproval(t *testing.T) {
	h := newStore(t, Options{})
	var n atomic.Int32
	r, err := h.s.Submit(context.Background(), agent, stop("web", &n))
	if err != nil || r.Status != StatusPending || !r.NeedsApproval {
		t.Fatalf("%v %+v", err, r)
	}
	if !r.ExpiresAt.Equal(h.clk.now().Add(time.Hour)) {
		t.Errorf("expires %v", r.ExpiresAt)
	}
	if n.Load() != 0 {
		t.Fatal("an approval-only action ran without approval")
	}
	if p := h.s.List(true, 0); len(p) != 1 || p[0].ID != r.ID {
		t.Fatalf("pending = %+v", p)
	}
}

func TestApproveExecutesExactlyTheStoredActionOnce(t *testing.T) {
	h := newStore(t, Options{})
	var n atomic.Int32
	a := stop("web", &n)
	a.params = []Param{{"timeout", "30s"}}
	r, _ := h.s.Submit(context.Background(), agent, a)
	// The snapshot is a copy: changing it changes nothing.
	r.Params[0].Value = "0s"
	r.Resource.Name = "other"

	got, err := h.s.Approve(context.Background(), r.ID, 0)
	if err != nil || got.Status != StatusRunning || got.DecidedBy == nil || *got.DecidedBy != 0 {
		t.Fatalf("%v %+v", err, got)
	}
	h.s.Wait()
	final, _ := h.s.Get(r.ID)
	if final.Status != StatusDone || n.Load() != 1 || final.Resource.Name != "web" || final.Params[0].Value != "30s" {
		t.Fatalf("got %+v executed=%d", final, n.Load())
	}
	// Single use.
	if _, err := h.s.Approve(context.Background(), r.ID, 0); !errors.Is(err, ErrNotPending) {
		t.Errorf("second approve: %v", err)
	}
	if _, err := h.s.Deny(context.Background(), r.ID, 0); !errors.Is(err, ErrNotPending) {
		t.Errorf("deny after approve: %v", err)
	}
	h.s.Wait()
	if n.Load() != 1 {
		t.Errorf("executed %d times", n.Load())
	}
	if a := h.audit.String(); !strings.Contains(a, `"msg":"action_approved","id":1,"by_uid":0`) {
		t.Errorf("audit: %s", a)
	}
}

func TestConcurrentApprovalsRunOnce(t *testing.T) {
	h := newStore(t, Options{})
	var n atomic.Int32
	r, _ := h.s.Submit(context.Background(), agent, stop("web", &n))
	var ok atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := h.s.Approve(context.Background(), r.ID, 0); err == nil {
				ok.Add(1)
			}
		})
	}
	wg.Wait()
	h.s.Wait()
	if ok.Load() != 1 || n.Load() != 1 {
		t.Fatalf("approvals=%d executions=%d", ok.Load(), n.Load())
	}
}

func TestDeny(t *testing.T) {
	h := newStore(t, Options{})
	var n atomic.Int32
	r, _ := h.s.Submit(context.Background(), agent, stop("web", &n))
	d, err := h.s.Deny(context.Background(), r.ID, 0)
	if err != nil || d.Status != StatusDenied {
		t.Fatalf("%v %+v", err, d)
	}
	if _, err := h.s.Approve(context.Background(), r.ID, 0); !errors.Is(err, ErrNotPending) {
		t.Errorf("approve after deny: %v", err)
	}
	h.s.Wait()
	if n.Load() != 0 {
		t.Error("denied request ran")
	}
	if len(h.s.List(true, 0)) != 0 {
		t.Error("denied request still pending")
	}
	if a := h.audit.String(); !strings.Contains(a, `"msg":"action_denied","id":1`) {
		t.Errorf("audit: %s", a)
	}
}

func TestUnknownID(t *testing.T) {
	h := newStore(t, Options{})
	for _, id := range []int64{0, -1, 1, 99} {
		if _, err := h.s.Approve(context.Background(), id, 0); !errors.Is(err, ErrNotFound) {
			t.Errorf("approve %d: %v", id, err)
		}
		if _, err := h.s.Deny(context.Background(), id, 0); !errors.Is(err, ErrNotFound) {
			t.Errorf("deny %d: %v", id, err)
		}
		if _, err := h.s.Get(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("get %d: %v", id, err)
		}
	}
}

func TestExpiryAfterOneHour(t *testing.T) {
	h := newStore(t, Options{})
	var n atomic.Int32
	r, _ := h.s.Submit(context.Background(), agent, stop("web", &n))
	h.clk.add(59*time.Minute + 59*time.Second)
	if got, _ := h.s.Get(r.ID); got.Status != StatusPending {
		t.Fatalf("expired early: %+v", got)
	}
	h.clk.add(time.Second)
	if got, _ := h.s.Get(r.ID); got.Status != StatusExpired {
		t.Fatalf("not expired: %+v", got)
	}
	if _, err := h.s.Approve(context.Background(), r.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("approve expired: %v", err)
	}
	if _, err := h.s.Deny(context.Background(), r.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("deny expired: %v", err)
	}
	h.s.Wait()
	if n.Load() != 0 {
		t.Error("expired request ran")
	}
	if a := h.audit.String(); strings.Count(a, `"msg":"action_expired"`) != 1 {
		t.Errorf("expiry must be audited once: %s", a)
	}
}

func TestSweepExpiresOnTime(t *testing.T) {
	h := newStore(t, Options{})
	h.s.Submit(context.Background(), agent, stop("web", nil))
	h.clk.add(time.Hour)
	h.s.Sweep(context.Background())
	if !strings.Contains(h.audit.String(), "action_expired") {
		t.Error("sweep did not expire")
	}
}

func TestCheckFailureStoresNothing(t *testing.T) {
	h := newStore(t, Options{})
	sentinel := errors.New("not allowed")
	a := stop("secret", nil)
	a.checkErr = sentinel
	if _, err := h.s.Submit(context.Background(), agent, a); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v", err)
	}
	if len(h.s.List(false, 0)) != 0 {
		t.Error("a refused request was stored")
	}
	if strings.Contains(h.audit.String(), "action_requested") {
		t.Error("refused request audited as requested")
	}
}

func TestResourceVanishedBeforeExecution(t *testing.T) {
	h := newStore(t, Options{Explain: func(error) (string, string) { return "the resource no longer exists", "not_found" }})
	var n atomic.Int32
	a := stop("web", &n)
	a.checkAt = 2 // passes at request time, fails at execution time
	r, _ := h.s.Submit(context.Background(), agent, a)
	h.s.Approve(context.Background(), r.ID, 0)
	h.s.Wait()
	got, _ := h.s.Get(r.ID)
	if got.Status != StatusFailed || got.Result.Summary != "the resource no longer exists" || n.Load() != 0 {
		t.Fatalf("got %+v executed=%d", got, n.Load())
	}
	if a := h.audit.String(); !strings.Contains(a, `"outcome":"failed","reason":"not_found"`) || strings.Contains(a, "vanished") {
		t.Errorf("audit: %s", a)
	}
}

func TestOneChangePerResource(t *testing.T) {
	h := newStore(t, Options{AutoWait: time.Millisecond})
	block := make(chan struct{})
	started := make(chan struct{}, 1)
	a := restart("web", nil)
	a.block, a.started = block, started
	r, err := h.s.Submit(context.Background(), agent, a)
	if err != nil || r.Status != StatusRunning {
		t.Fatalf("%v %+v", err, r)
	}
	<-started
	// An automatic change to the same resource is refused...
	if _, err := h.s.Submit(context.Background(), agent, restart("web", nil)); !errors.Is(err, ErrBusy) {
		t.Errorf("concurrent restart: %v", err)
	}
	// ...a different resource is not affected...
	if r2, err := h.s.Submit(context.Background(), agent, restart("db", nil)); err != nil || r2.Status == StatusPending {
		t.Errorf("other resource: %v %+v", err, r2)
	}
	// ...the same name of another type is another resource...
	u := restart("web", nil)
	u.res.Type = "unit"
	if _, err := h.s.Submit(context.Background(), agent, u); err != nil {
		t.Errorf("unit with the same name: %v", err)
	}
	// ...and an approval-only request is stored, but cannot run yet.
	p, err := h.s.Submit(context.Background(), agent, stop("web", nil))
	if err != nil || p.Status != StatusPending {
		t.Fatalf("%v %+v", err, p)
	}
	if _, err := h.s.Approve(context.Background(), p.ID, 0); !errors.Is(err, ErrBusy) {
		t.Errorf("approve while busy: %v", err)
	}
	if got, _ := h.s.Get(p.ID); got.Status != StatusPending {
		t.Errorf("busy approval must leave the request pending: %+v", got)
	}
	close(block)
	h.s.Wait()
	if _, err := h.s.Approve(context.Background(), p.ID, 0); err != nil {
		t.Errorf("approve once free: %v", err)
	}
}

func TestPendingCap(t *testing.T) {
	h := newStore(t, Options{MaxPending: 3})
	for i := range 3 {
		if _, err := h.s.Submit(context.Background(), agent, stop("web", nil)); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	if _, err := h.s.Submit(context.Background(), agent, stop("db", nil)); !errors.Is(err, ErrTooMany) {
		t.Fatalf("err = %v", err)
	}
	// Restarts within the rate limit still run.
	if r, err := h.s.Submit(context.Background(), agent, restart("db", nil)); err != nil || r.Status != StatusDone {
		t.Fatalf("%v %+v", err, r)
	}
}

func TestSlowAutomaticActionReportsRunning(t *testing.T) {
	h := newStore(t, Options{AutoWait: 10 * time.Millisecond})
	block := make(chan struct{})
	a := restart("web", nil)
	a.block = block
	r, err := h.s.Submit(context.Background(), agent, a)
	if err != nil || r.Status != StatusRunning || r.StartedAt.IsZero() {
		t.Fatalf("%v %+v", err, r)
	}
	close(block)
	h.s.Wait()
	if got, _ := h.s.Get(r.ID); got.Status != StatusDone || got.FinishedAt.IsZero() {
		t.Errorf("got %+v", got)
	}
}

func TestIDsIncreaseAndListOrder(t *testing.T) {
	h := newStore(t, Options{})
	for range 4 {
		h.s.Submit(context.Background(), agent, stop("web", nil))
	}
	h.s.Submit(context.Background(), agent, restart("web", nil))
	all := h.s.List(false, 0)
	if len(all) != 5 {
		t.Fatalf("got %d", len(all))
	}
	for i, r := range all {
		if r.ID != int64(i+1) {
			t.Errorf("order: %d at %d", r.ID, i)
		}
	}
	if last := h.s.List(false, 2); len(last) != 2 || last[0].ID != 4 || last[1].ID != 5 {
		t.Errorf("limit keeps the newest, oldest first: %+v", last)
	}
	if p := h.s.List(true, 0); len(p) != 4 {
		t.Errorf("pending = %d", len(p))
	}
}

func TestPruneKeepsPendingAndRecent(t *testing.T) {
	h := newStore(t, Options{Keep: 3})
	p, _ := h.s.Submit(context.Background(), agent, stop("web", nil))
	for range 6 {
		h.s.Submit(context.Background(), agent, restart("db", nil))
		h.clk.add(time.Hour)
	}
	// Expiry made the pending request final; resubmit one to stay pending.
	q, _ := h.s.Submit(context.Background(), agent, stop("web", nil))
	h.s.Submit(context.Background(), agent, restart("db", nil))
	all := h.s.List(false, 0)
	if len(all) > 4 {
		t.Errorf("kept %d, want at most Keep finished plus pending", len(all))
	}
	if _, err := h.s.Get(q.ID); err != nil {
		t.Error("pending request pruned")
	}
	if _, err := h.s.Get(p.ID); !errors.Is(err, ErrNotFound) {
		t.Error("oldest finished request kept")
	}
}

func TestExecutionPanicReleasesLock(t *testing.T) {
	h := newStore(t, Options{})
	r, err := h.s.Submit(context.Background(), agent, panicking{})
	if err != nil || r.Status != StatusFailed {
		t.Fatalf("%v %+v", err, r)
	}
	if r, err := h.s.Submit(context.Background(), agent, restart("web", nil)); err != nil || r.Status != StatusDone {
		t.Fatalf("lock not released: %v %+v", err, r)
	}
}

type panicking struct{}

func (panicking) Kind() string                            { return "docker_restart" }
func (panicking) Resource() Resource                      { return Resource{"container", "web"} }
func (panicking) Params() []Param                         { return nil }
func (panicking) Describe() []string                      { return nil }
func (panicking) Mode() Mode                              { return RateLimited }
func (panicking) Check(context.Context) error             { return nil }
func (panicking) Execute(context.Context) (Result, error) { panic("boom") }

func TestIDsContinueAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "last-request-id")
	open := func() *Store {
		last, err := LoadLastID(path)
		if err != nil {
			t.Fatal(err)
		}
		return New(Options{LastID: last, SaveID: IDSaver(path)})
	}
	s := open()
	for range 3 {
		if _, err := s.Submit(context.Background(), Requester{}, idAction()); err != nil {
			t.Fatal(err)
		}
	}
	r, err := open().Submit(context.Background(), Requester{}, idAction())
	if err != nil || r.ID != 4 {
		t.Fatalf("after restart got id %d, %v; want 4", r.ID, err)
	}
}

func TestUnsavedIDRefusesTheRequest(t *testing.T) {
	s := New(Options{SaveID: func(int64) error { return errors.New("disk full") }})
	if _, err := s.Submit(context.Background(), Requester{}, idAction()); !errors.Is(err, ErrIDNotSaved) {
		t.Fatalf("got %v", err)
	}
	if len(s.List(false, 0)) != 0 {
		t.Error("a request was stored without a saved id")
	}
}

func TestLoadLastIDRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id")
	if err := os.WriteFile(path, []byte("abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLastID(path); err == nil {
		t.Error("garbage accepted")
	}
}

// idAction is a change that waits for approval, so it never runs.
func idAction() *fakeAction {
	return &fakeAction{kind: "docker_stop", res: Resource{Type: "container", Name: "web"}, mode: Always}
}

type undoAction struct{ *fakeAction }

func (undoAction) Timeout() time.Duration { return 10 * time.Minute }

func TestRunsOnTimeoutWaitsForABusyResource(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}
	s := New(Options{Now: clk.now})
	var ran atomic.Int32
	block := make(chan struct{})
	busy := &fakeAction{kind: "docker_restart", res: Resource{Type: "file", Name: "/x"}, mode: Always, block: block, started: make(chan struct{}, 1)}
	b, _ := s.Submit(context.Background(), Requester{}, busy)
	if _, err := s.Approve(context.Background(), b.ID, 0); err != nil {
		t.Fatal(err)
	}
	<-busy.started
	u, err := s.Submit(context.Background(), Requester{}, undoAction{&fakeAction{kind: "file_restore", res: Resource{Type: "file", Name: "/x"}, mode: Always, executed: &ran}})
	if err != nil || !u.RunsOnTimeout || !u.ExpiresAt.Equal(clk.now().Add(10*time.Minute)) {
		t.Fatalf("%+v %v", u, err)
	}
	clk.add(10 * time.Minute)
	s.Sweep(context.Background())
	if r, _ := s.Get(u.ID); r.Status != StatusPending {
		t.Fatalf("ran over a busy resource: %s", r.Status)
	}
	close(block)
	s.Wait()
	s.Sweep(context.Background())
	s.Wait()
	if r, _ := s.Get(u.ID); r.Status != StatusDone || ran.Load() != 1 || r.DecidedBy != nil {
		t.Errorf("got %+v, ran %d", r, ran.Load())
	}
}
