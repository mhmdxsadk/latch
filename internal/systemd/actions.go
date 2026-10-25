package systemd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"
)

// JobTimeout bounds how long an action waits for systemd to finish its job.
const JobTimeout = 90 * time.Second

// JobResult is the outcome of a start, stop or restart job.
type JobResult struct {
	// Result is systemd's job result: done, canceled, timeout, failed,
	// dependency, skipped; or "unknown" when the job ended without Latch
	// seeing its result.
	Result      string `json:"result"`
	ActiveState string `json:"active_state"`
	SubState    string `json:"sub_state"`
}

// ErrJobFailed means systemd finished the job with a result other than done.
var ErrJobFailed = errors.New("systemd job did not succeed")

// ErrJobWait means the job did not finish within JobTimeout.
var ErrJobWait = errors.New("systemd job did not finish in time")

// Mutator is the change interface the MCP layer uses; tests substitute a fake.
// Unit names must already have been resolved by policy.
type Mutator interface {
	Restart(ctx context.Context, unit string) (JobResult, error)
	Stop(ctx context.Context, unit string) (JobResult, error)
	Start(ctx context.Context, unit string) (JobResult, error)
	// Reload makes systemd reread its unit files (systemctl daemon-reload).
	Reload(ctx context.Context) error
}

// verb is one of the three Manager methods Actions may call. The method name
// is never built from input.
type verb int

const (
	verbRestart verb = iota
	verbStop
	verbStart
)

func (v verb) method() string {
	switch v {
	case verbRestart:
		return ifaceManager + ".RestartUnit"
	case verbStop:
		return ifaceManager + ".StopUnit"
	case verbStart:
		return ifaceManager + ".StartUnit"
	}
	panic("unknown verb")
}

// jobBus is the part of the bus Actions uses; tests substitute a fake.
type jobBus interface {
	propertyReader
	// enqueue calls method(unit, "replace") and returns the job path.
	enqueue(ctx context.Context, method, unit string) (dbus.ObjectPath, error)
	// wait returns the result of job once systemd removes it.
	wait(ctx context.Context, job dbus.ObjectPath) (string, error)
	// reload calls Manager.Reload, which returns once systemd has reread
	// every unit file.
	reload(ctx context.Context) error
	close()
}

// Actions starts, stops and restarts units over the system bus. It is
// separate from Bus, which stays read-only. Before acting it checks, on the
// same connection, that the unit exists and that its Id is exactly the
// requested name, so an alias cannot reach an excluded unit.
type Actions struct {
	connect func(ctx context.Context) (jobBus, error)
}

var _ Mutator = (*Actions)(nil)

// NewActions returns Actions over the system bus.
func NewActions() *Actions { return &Actions{connect: connectJobBus} }

// Restart restarts the unit and waits for the job.
func (a *Actions) Restart(ctx context.Context, unit string) (JobResult, error) {
	return a.do(ctx, verbRestart, unit)
}

// Stop stops the unit and waits for the job.
func (a *Actions) Stop(ctx context.Context, unit string) (JobResult, error) {
	return a.do(ctx, verbStop, unit)
}

// Start starts the unit and waits for the job.
func (a *Actions) Start(ctx context.Context, unit string) (JobResult, error) {
	return a.do(ctx, verbStart, unit)
}

// ReloadTimeout bounds a daemon-reload.
const ReloadTimeout = 60 * time.Second

// ErrReload means the daemon-reload failed.
var ErrReload = errors.New("systemd daemon-reload failed")

// Reload runs a daemon-reload (Manager.Reload) and waits for it to finish.
func (a *Actions) Reload(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, ReloadTimeout)
	defer cancel()
	bus, err := a.connect(ctx)
	if err != nil {
		return err
	}
	defer bus.close()
	if err := bus.reload(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrReload, err)
	}
	return nil
}

func (a *Actions) do(ctx context.Context, v verb, unit string) (JobResult, error) {
	if !unitName.MatchString(unit) {
		return JobResult{}, errors.New("invalid unit name")
	}
	ctx, cancel := context.WithTimeout(ctx, JobTimeout+10*time.Second)
	defer cancel()
	bus, err := a.connect(ctx)
	if err != nil {
		return JobResult{}, err
	}
	defer bus.close()

	path := dbus.ObjectPath(unitPrefix + escapePath(unit))
	props, err := bus.getAll(ctx, path, ifaceUnit)
	if err != nil {
		return JobResult{}, err
	}
	if get[string](props, "Id") != unit || get[string](props, "LoadState") == "not-found" {
		return JobResult{}, ErrNotFound
	}
	job, err := bus.enqueue(ctx, v.method(), unit)
	if err != nil {
		return JobResult{}, fmt.Errorf("enqueue job: %w", err)
	}
	wctx, wcancel := context.WithTimeout(ctx, JobTimeout)
	defer wcancel()
	res := JobResult{}
	res.Result, err = bus.wait(wctx, job)
	if err != nil {
		res.Result = "still running"
		return res, ErrJobWait
	}
	if after, err := bus.getAll(ctx, path, ifaceUnit); err == nil {
		res.ActiveState = get[string](after, "ActiveState")
		res.SubState = get[string](after, "SubState")
	}
	if res.Result != "done" && res.Result != "unknown" {
		return res, ErrJobFailed
	}
	return res, nil
}

// dbusJobBus is the real jobBus: a private system bus connection that
// listens for the Manager's JobRemoved signal.
type dbusJobBus struct {
	connReader
	signals chan *dbus.Signal
}

func connectJobBus(ctx context.Context) (jobBus, error) {
	conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx))
	if err != nil {
		return nil, errors.New("connect to system bus failed")
	}
	b := &dbusJobBus{connReader: connReader{conn}, signals: make(chan *dbus.Signal, 64)}
	// Listen before enqueueing so the signal cannot be missed.
	if err := conn.AddMatchSignalContext(ctx,
		dbus.WithMatchObjectPath(managerPath),
		dbus.WithMatchInterface(ifaceManager),
		dbus.WithMatchMember("JobRemoved"),
	); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("watch jobs: %w", err)
	}
	conn.Signal(b.signals)
	return b, nil
}

func (b *dbusJobBus) close() { _ = b.conn.Close() }

func (b *dbusJobBus) enqueue(ctx context.Context, method, unit string) (dbus.ObjectPath, error) {
	var job dbus.ObjectPath
	err := b.conn.Object(busName, managerPath).CallWithContext(ctx, method, 0, unit, "replace").Store(&job)
	return job, err
}

func (b *dbusJobBus) reload(ctx context.Context) error {
	return b.conn.Object(busName, managerPath).CallWithContext(ctx, ifaceManager+".Reload", 0).Err
}

func (b *dbusJobBus) wait(ctx context.Context, job dbus.ObjectPath) (string, error) {
	return waitJob(ctx, job, b.signals, func(ctx context.Context) bool {
		var v dbus.Variant
		err := b.conn.Object(busName, job).CallWithContext(ctx, ifaceProps+".Get", 0, "org.freedesktop.systemd1.Job", "State").Store(&v)
		return err == nil
	}, 2*time.Second)
}

// waitJob waits for the JobRemoved signal of job. As a fallback it polls
// whether the job still exists; a job that vanished without a signal ends
// with "unknown".
func waitJob(ctx context.Context, job dbus.ObjectPath, signals <-chan *dbus.Signal, exists func(context.Context) bool, poll time.Duration) (string, error) {
	match := func(s *dbus.Signal) (string, bool) {
		if s == nil || s.Name != ifaceManager+".JobRemoved" || len(s.Body) != 4 {
			return "", false
		}
		p, ok1 := s.Body[1].(dbus.ObjectPath)
		r, ok2 := s.Body[3].(string)
		return r, ok1 && ok2 && p == job
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case s := <-signals:
			if r, ok := match(s); ok {
				return r, nil
			}
		case <-t.C:
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if exists(ctx) {
				continue
			}
			for {
				select {
				case s := <-signals:
					if r, ok := match(s); ok {
						return r, nil
					}
				default:
					return "unknown", nil
				}
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}
