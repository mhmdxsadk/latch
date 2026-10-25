// Package approval queues and runs the changes an agent asks for.
//
// Read-only tools never come here. A change is an Action: an immutable,
// already validated description of exactly one named operation on one
// resource. Submit either runs it at once (restarts, within a rate limit) or
// stores it as a pending request that only the owner can approve or deny,
// through the root-only admin socket. An approval runs exactly the stored
// Action, once. Requests expire after an hour and live in memory only: a
// Latch restart forgets them.
//
// Only one change runs per resource at a time. A second change to a busy
// resource is refused with ErrBusy rather than queued, so the agent can see
// what happened and try again; a pending request is not affected and can be
// approved once the resource is free.
package approval

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/mhmdxsadk/latch/internal/audit"
)

// Mode says when an Action may run without the owner.
type Mode int

const (
	// Always means every request waits for the owner's approval.
	Always Mode = iota
	// RateLimited means the action runs automatically while its resource had
	// fewer than RateLimit rate-limited executions in the last RateWindow;
	// beyond that it waits for approval like an Always action.
	RateLimited
)

// Resource names what an Action changes.
type Resource struct {
	Type string `json:"type"` // "container", "unit", later "file"
	Name string `json:"name"`
}

func (r Resource) key() string { return r.Type + "/" + r.Name }

// Param is one named parameter of an Action, shown to the owner. Never put a
// secret or file content in a Param: Params are returned by the admin API and
// the agent-facing status tools.
type Param struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Result is what an execution produced. Every field must be safe to show the
// agent: fixed text, validated names, or host text that went through
// textsafe and redact.
type Result struct {
	Summary string   `json:"summary"`
	Details []string `json:"details,omitempty"`
}

// Action is one change, fully validated when it is built. Implementations
// must be immutable: the store keeps the value and approval executes exactly
// it, so there must be nothing an agent can alter between request and
// execution.
type Action interface {
	// Kind is the tool name, for example "docker_restart".
	Kind() string
	// Resource is what the action changes. It is the lock and rate-limit key.
	Resource() Resource
	// Params are extra parameters to show (for example pull=true).
	Params() []Param
	// Describe returns plain lines for "latch pending" saying exactly what
	// will run (later: a diff).
	Describe() []string
	// Mode decides whether approval is needed.
	Mode() Mode
	// Check verifies that the resource still exists and is allowed. It runs
	// when the request is made and again right before execution.
	Check(ctx context.Context) error
	// Execute performs the change. The context carries the execution
	// deadline; Execute must not outlive it.
	Execute(ctx context.Context) (Result, error)
}

// RunsOnTimeout is implemented by actions that run, instead of lapsing,
// when the owner does not decide in time. Latch files these itself (a
// restore after a failed change), never on the agent's behalf.
type RunsOnTimeout interface {
	// Timeout is how long the request waits for the owner before it runs.
	Timeout() time.Duration
}

// Status is the state of a request.
type Status string

const (
	StatusPending  Status = "pending"
	StatusApproved Status = "approved" // approved, about to run
	StatusRunning  Status = "running"
	StatusDone     Status = "done"
	StatusFailed   Status = "failed"
	StatusDenied   Status = "denied"
	StatusExpired  Status = "expired"
)

// Requester is who asked for a change (kernel-verified peer).
type Requester struct {
	UID uint32 `json:"uid"`
	PID int32  `json:"pid"`
}

// Request is a snapshot of one request. It is a copy; changing it changes
// nothing in the store.
type Request struct {
	ID            int64    `json:"id"`
	Action        string   `json:"action"`
	Resource      Resource `json:"resource"`
	Params        []Param  `json:"params"`
	Details       []string `json:"details"`
	NeedsApproval bool     `json:"needs_approval"`
	// RunsOnTimeout means the request runs at ExpiresAt unless denied.
	RunsOnTimeout bool      `json:"runs_on_timeout,omitempty"`
	Status        Status    `json:"status"`
	RequestedBy   Requester `json:"requested_by"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at,omitzero"`
	DecidedAt     time.Time `json:"decided_at,omitzero"`
	DecidedBy     *uint32   `json:"decided_by,omitempty"`
	StartedAt     time.Time `json:"started_at,omitzero"`
	FinishedAt    time.Time `json:"finished_at,omitzero"`
	Result        *Result   `json:"result,omitempty"`
}

// Errors returned by the store. Their text is safe to show.
var (
	ErrNotFound   = errors.New("not found or expired")
	ErrNotPending = errors.New("not pending")
	ErrBusy       = errors.New("another change to this resource is in progress")
	ErrTooMany    = errors.New("too many requests are waiting for approval")
	ErrIDNotSaved = errors.New("could not record the request id")
)

// Defaults for Options.
const (
	DefaultExpiry       = time.Hour
	DefaultRateLimit    = 5
	DefaultRateWindow   = time.Hour
	DefaultAutoWait     = 30 * time.Second
	DefaultMaxPending   = 20
	DefaultKeep         = 200
	DefaultMaxExecution = 15 * time.Minute
)

// Options configure a Store. Zero values take the defaults.
type Options struct {
	Audit *audit.Logger
	// Now is the clock; tests inject one.
	Now func() time.Time
	// Base is the parent context of executions; cancel it on shutdown.
	Base context.Context
	// Explain turns an execution or re-check error into a summary for the
	// agent and an audit reason. It must not return error text.
	Explain func(error) (summary, reason string)

	Expiry       time.Duration // pending requests expire after this
	RateLimit    int           // rate-limited executions per resource per window
	RateWindow   time.Duration // sliding window for RateLimit
	AutoWait     time.Duration // how long Submit waits for an automatic action
	MaxPending   int           // pending requests allowed at once
	Keep         int           // finished requests kept for status queries
	MaxExecution time.Duration // hard bound on one execution

	// LastID is the highest id handed out before, so ids continue after a
	// restart instead of starting at 1 again: an old id must never name a
	// new request. SaveID records each new id before it is used; if it
	// fails, the request is refused.
	LastID int64
	SaveID func(id int64) error
}

type entry struct {
	req    Request
	action Action
	done   chan struct{} // closed when the execution ends
}

// Store holds requests and runs actions. It is safe for concurrent use.
type Store struct {
	o Options

	mu      sync.Mutex
	nextID  int64
	entries map[int64]*entry
	order   []int64                // ids, ascending
	busy    map[string]bool        // resources with a running change
	history map[string][]time.Time // rate-limited executions per resource
	wg      sync.WaitGroup
}

// New returns an empty Store.
func New(o Options) *Store {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Base == nil {
		o.Base = context.Background()
	}
	if o.Explain == nil {
		o.Explain = func(error) (string, string) { return "the change failed; see the Latch audit log", "error" }
	}
	if o.Audit == nil {
		o.Audit = audit.Discard()
	}
	def := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	defInt := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&o.Expiry, DefaultExpiry)
	def(&o.RateWindow, DefaultRateWindow)
	def(&o.AutoWait, DefaultAutoWait)
	def(&o.MaxExecution, DefaultMaxExecution)
	defInt(&o.RateLimit, DefaultRateLimit)
	defInt(&o.MaxPending, DefaultMaxPending)
	defInt(&o.Keep, DefaultKeep)
	return &Store{
		o:       o,
		nextID:  max(o.LastID, 0) + 1,
		entries: map[int64]*entry{},
		busy:    map[string]bool{},
		history: map[string][]time.Time{},
	}
}

// Wait blocks until every started execution has ended. Tests use it.
func (s *Store) Wait() { s.wg.Wait() }

// Submit checks a and either starts it (automatic) or stores it as a pending
// request. A Check error is returned unchanged and nothing is stored. For an
// automatic action Submit waits up to AutoWait for the result, so a quick
// restart reports "done" directly; otherwise the status is "running".
func (s *Store) Submit(ctx context.Context, who Requester, a Action) (Request, error) {
	if err := a.Check(ctx); err != nil {
		return Request{}, err
	}
	res := a.Resource()
	key := res.key()

	s.mu.Lock()
	now := s.o.Now()
	s.sweepLocked(ctx, now)
	needs := a.Mode() == Always || !s.rateAllowsLocked(key, now)
	if needs && s.countPendingLocked() >= s.o.MaxPending {
		s.mu.Unlock()
		return Request{}, ErrTooMany
	}
	if !needs && s.busy[key] {
		s.mu.Unlock()
		return Request{}, ErrBusy
	}
	if s.o.SaveID != nil {
		if err := s.o.SaveID(s.nextID); err != nil {
			s.mu.Unlock()
			return Request{}, ErrIDNotSaved
		}
	}
	e := &entry{
		action: a,
		done:   make(chan struct{}),
		req: Request{
			ID:            s.nextID,
			Action:        a.Kind(),
			Resource:      res,
			Params:        slices.Clone(a.Params()),
			Details:       slices.Clone(a.Describe()),
			NeedsApproval: needs,
			Status:        StatusPending,
			RequestedBy:   who,
			CreatedAt:     now,
		},
	}
	if e.req.Params == nil {
		e.req.Params = []Param{}
	}
	if e.req.Details == nil {
		e.req.Details = []string{}
	}
	s.nextID++
	s.entries[e.req.ID] = e
	s.order = append(s.order, e.req.ID)
	s.o.Audit.ActionRequested(ctx, e.req.ID, e.req.Action, res.key(), needs, audit.Peer{UID: who.UID, PID: who.PID})
	if needs {
		e.req.ExpiresAt = now.Add(s.o.Expiry)
		if r, ok := a.(RunsOnTimeout); ok {
			e.req.RunsOnTimeout = true
			e.req.ExpiresAt = now.Add(r.Timeout())
		}
		snap := s.snapshotLocked(e)
		s.pruneLocked()
		s.mu.Unlock()
		return snap, nil
	}
	s.startLocked(e, key, now)
	s.pruneLocked()
	s.mu.Unlock()

	t := time.NewTimer(s.o.AutoWait)
	defer t.Stop()
	select {
	case <-e.done:
	case <-t.C:
	case <-ctx.Done():
	}
	return s.Get(e.req.ID)
}

// Approve runs a pending request on behalf of uid. It fails with ErrNotFound
// for unknown or expired requests, ErrNotPending once decided, and ErrBusy
// (the request stays pending) while another change to the resource runs.
func (s *Store) Approve(ctx context.Context, id int64, uid uint32) (Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.o.Now()
	s.sweepLocked(ctx, now)
	e, err := s.pendingLocked(id)
	if err != nil {
		return Request{}, err
	}
	key := e.req.Resource.key()
	if s.busy[key] {
		return Request{}, ErrBusy
	}
	e.req.Status = StatusApproved
	e.req.DecidedAt = now
	e.req.DecidedBy = &uid
	s.o.Audit.ActionDecided(ctx, audit.ActionApproved, id, uid)
	s.startLocked(e, key, now)
	return s.snapshotLocked(e), nil
}

// Deny refuses a pending request on behalf of uid.
func (s *Store) Deny(ctx context.Context, id int64, uid uint32) (Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.o.Now()
	s.sweepLocked(ctx, now)
	e, err := s.pendingLocked(id)
	if err != nil {
		return Request{}, err
	}
	e.req.Status = StatusDenied
	e.req.DecidedAt = now
	e.req.DecidedBy = &uid
	e.req.FinishedAt = now
	close(e.done)
	s.o.Audit.ActionDecided(ctx, audit.ActionDenied, id, uid)
	return s.snapshotLocked(e), nil
}

// Get returns one request.
func (s *Store) Get(id int64) (Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(context.Background(), s.o.Now())
	e, ok := s.entries[id]
	if !ok {
		return Request{}, ErrNotFound
	}
	return s.snapshotLocked(e), nil
}

// List returns up to limit requests, oldest first. With pendingOnly it
// returns only requests waiting for approval.
func (s *Store) List(pendingOnly bool, limit int) []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(context.Background(), s.o.Now())
	out := []Request{}
	for i := len(s.order) - 1; i >= 0 && (limit <= 0 || len(out) < limit); i-- {
		e := s.entries[s.order[i]]
		if pendingOnly && e.req.Status != StatusPending {
			continue
		}
		out = append(out, s.snapshotLocked(e))
	}
	slices.Reverse(out)
	return out
}

// Sweep expires overdue requests now. Every other method sweeps too; this is
// for a periodic timer so expiry is audited on time.
func (s *Store) Sweep(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(ctx, s.o.Now())
}

func (s *Store) pendingLocked(id int64) (*entry, error) {
	e, ok := s.entries[id]
	switch {
	case !ok, e.req.Status == StatusExpired:
		return nil, ErrNotFound
	case e.req.Status != StatusPending:
		return nil, ErrNotPending
	}
	return e, nil
}

// startLocked marks the resource busy, counts a rate-limited execution and
// runs the action in the background.
func (s *Store) startLocked(e *entry, key string, now time.Time) {
	s.busy[key] = true
	if e.action.Mode() == RateLimited {
		s.history[key] = append(s.history[key], now)
	}
	e.req.Status = StatusRunning
	e.req.StartedAt = now
	s.wg.Add(1)
	go s.run(e, key)
}

func (s *Store) run(e *entry, key string) {
	defer s.wg.Done()
	ctx, cancel := context.WithTimeout(s.o.Base, s.o.MaxExecution)
	defer cancel()
	start := s.o.Now()

	var (
		res    Result
		err    error
		reason string
	)
	func() {
		// A panicking action must not take the resource lock with it.
		defer func() {
			if p := recover(); p != nil {
				err = errors.New("action panicked")
			}
		}()
		if err = e.action.Check(ctx); err != nil {
			return
		}
		res, err = e.action.Execute(ctx)
	}()

	s.mu.Lock()
	defer s.mu.Unlock()
	e.req.FinishedAt = s.o.Now()
	if err != nil {
		summary, why := s.o.Explain(err)
		reason = why
		if res.Summary == "" {
			res.Summary = summary
		}
		e.req.Status = StatusFailed
	} else {
		e.req.Status = StatusDone
	}
	res.Details = slices.Clone(res.Details)
	e.req.Result = &res
	delete(s.busy, key)
	close(e.done)
	s.o.Audit.ActionExecuted(context.Background(), e.req.ID, string(e.req.Status), reason, e.req.FinishedAt.Sub(start))
	s.pruneLocked()
}

func (s *Store) rateAllowsLocked(key string, now time.Time) bool {
	h := s.history[key]
	cut := 0
	for cut < len(h) && now.Sub(h[cut]) >= s.o.RateWindow {
		cut++
	}
	h = h[cut:]
	if len(h) == 0 {
		delete(s.history, key)
	} else {
		s.history[key] = h
	}
	return len(h) < s.o.RateLimit
}

func (s *Store) countPendingLocked() int {
	n := 0
	for _, e := range s.entries {
		if e.req.Status == StatusPending {
			n++
		}
	}
	return n
}

func (s *Store) sweepLocked(ctx context.Context, now time.Time) {
	for _, id := range s.order {
		e := s.entries[id]
		if e.req.Status != StatusPending || now.Before(e.req.ExpiresAt) {
			continue
		}
		if e.req.RunsOnTimeout {
			// Runs as if approved; a busy resource is tried again on the
			// next sweep.
			key := e.req.Resource.key()
			if !s.busy[key] {
				e.req.Status = StatusApproved
				e.req.DecidedAt = now
				s.o.Audit.ActionDecided(ctx, audit.ActionTimedOut, id, 0)
				s.startLocked(e, key, now)
			}
			continue
		}
		e.req.Status = StatusExpired
		e.req.FinishedAt = now
		close(e.done)
		s.o.Audit.ActionDecided(ctx, audit.ActionExpired, id, 0)
	}
}

// pruneLocked forgets the oldest finished requests beyond Keep. Pending and
// running requests are never dropped.
func (s *Store) pruneLocked() {
	finished := 0
	for _, id := range s.order {
		if isFinal(s.entries[id].req.Status) {
			finished++
		}
	}
	if finished <= s.o.Keep {
		return
	}
	drop := finished - s.o.Keep
	kept := s.order[:0]
	for _, id := range s.order {
		if drop > 0 && isFinal(s.entries[id].req.Status) {
			delete(s.entries, id)
			drop--
			continue
		}
		kept = append(kept, id)
	}
	s.order = kept
}

func isFinal(st Status) bool {
	switch st {
	case StatusDone, StatusFailed, StatusDenied, StatusExpired:
		return true
	}
	return false
}

func (s *Store) snapshotLocked(e *entry) Request {
	r := e.req
	r.Params = slices.Clone(r.Params)
	r.Details = slices.Clone(r.Details)
	if r.DecidedBy != nil {
		v := *r.DecidedBy
		r.DecidedBy = &v
	}
	if r.Result != nil {
		res := *r.Result
		res.Details = slices.Clone(res.Details)
		r.Result = &res
	}
	return r
}
