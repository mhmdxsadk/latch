package systemd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// fakeJobBus answers unit properties and records enqueued jobs.
type fakeJobBus struct {
	fakeReader
	methods   []string
	units     []string
	result    string
	waitErr   error
	closed    bool
	reloads   int
	reloadErr error
}

func (f *fakeJobBus) reload(context.Context) error {
	f.reloads++
	return f.reloadErr
}

func (f *fakeJobBus) enqueue(_ context.Context, method, unit string) (dbus.ObjectPath, error) {
	f.methods = append(f.methods, method)
	f.units = append(f.units, unit)
	return "/org/freedesktop/systemd1/job/42", nil
}

func (f *fakeJobBus) wait(context.Context, dbus.ObjectPath) (string, error) {
	return f.result, f.waitErr
}

func (f *fakeJobBus) close() { f.closed = true }

func actionsWith(f *fakeJobBus) *Actions {
	return &Actions{connect: func(context.Context) (jobBus, error) { return f, nil }}
}

func unitProps(id string) map[string]map[string]dbus.Variant {
	return map[string]map[string]dbus.Variant{ifaceUnit: {
		"Id": v(id), "LoadState": v("loaded"), "ActiveState": v("active"), "SubState": v("running"),
	}}
}

func TestActionsCallFixedMethods(t *testing.T) {
	for _, tc := range []struct {
		call   func(*Actions) (JobResult, error)
		method string
	}{
		{func(a *Actions) (JobResult, error) { return a.Restart(context.Background(), "web.service") }, "org.freedesktop.systemd1.Manager.RestartUnit"},
		{func(a *Actions) (JobResult, error) { return a.Stop(context.Background(), "web.service") }, "org.freedesktop.systemd1.Manager.StopUnit"},
		{func(a *Actions) (JobResult, error) { return a.Start(context.Background(), "web.service") }, "org.freedesktop.systemd1.Manager.StartUnit"},
	} {
		f := &fakeJobBus{fakeReader: fakeReader{props: unitProps("web.service")}, result: "done"}
		res, err := tc.call(actionsWith(f))
		if err != nil || res.Result != "done" || res.ActiveState != "active" || res.SubState != "running" {
			t.Fatalf("%s: %v %+v", tc.method, err, res)
		}
		if len(f.methods) != 1 || f.methods[0] != tc.method || f.units[0] != "web.service" {
			t.Errorf("called %v %v", f.methods, f.units)
		}
		if f.paths[0] != "/org/freedesktop/systemd1/unit/web_2eservice" || !f.closed {
			t.Errorf("paths %v closed %v", f.paths, f.closed)
		}
	}
}

// An alias resolves to another unit; acting on it could reach an excluded
// unit, so it is refused before any job is enqueued.
func TestActionsRefuseAliasAndMissing(t *testing.T) {
	for _, props := range []map[string]map[string]dbus.Variant{
		unitProps("vault.service"),
		{ifaceUnit: {"Id": v("web.service"), "LoadState": v("not-found")}},
		{ifaceUnit: {}},
	} {
		f := &fakeJobBus{fakeReader: fakeReader{props: props}, result: "done"}
		if _, err := actionsWith(f).Restart(context.Background(), "web.service"); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v", err)
		}
		if len(f.methods) != 0 {
			t.Errorf("job enqueued: %v", f.methods)
		}
	}
}

func TestActionsInvalidNamesRejectedBeforeBus(t *testing.T) {
	connected := false
	a := &Actions{connect: func(context.Context) (jobBus, error) { connected = true; return nil, errors.New("no") }}
	for _, u := range []string{"", "ssh", "../ssh.service", "a b.service", "ssh.service\n", "*.service", "-x.service/.."} {
		if _, err := a.Stop(context.Background(), u); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	if connected {
		t.Error("bus contacted for an invalid name")
	}
}

func TestActionsJobResults(t *testing.T) {
	for _, tc := range []struct {
		result  string
		waitErr error
		want    error
	}{
		{"done", nil, nil},
		{"unknown", nil, nil},
		{"failed", nil, ErrJobFailed},
		{"dependency", nil, ErrJobFailed},
		{"canceled", nil, ErrJobFailed},
		{"", context.DeadlineExceeded, ErrJobWait},
	} {
		f := &fakeJobBus{fakeReader: fakeReader{props: unitProps("web.service")}, result: tc.result, waitErr: tc.waitErr}
		res, err := actionsWith(f).Start(context.Background(), "web.service")
		if !errors.Is(err, tc.want) {
			t.Errorf("%q: err = %v", tc.result, err)
		}
		if errors.Is(tc.want, ErrJobFailed) && res.Result != tc.result {
			t.Errorf("result %q lost", tc.result)
		}
	}
}

func removed(path dbus.ObjectPath, result string) *dbus.Signal {
	return &dbus.Signal{
		Name: ifaceManager + ".JobRemoved",
		Body: []any{uint32(42), path, "web.service", result},
	}
}

func TestWaitJob(t *testing.T) {
	job := dbus.ObjectPath("/org/freedesktop/systemd1/job/42")
	always := func(context.Context) bool { return true }

	ch := make(chan *dbus.Signal, 4)
	ch <- removed("/org/freedesktop/systemd1/job/41", "failed") // another job
	ch <- &dbus.Signal{Name: ifaceManager + ".JobNew", Body: []any{uint32(42), job, "web.service"}}
	ch <- removed(job, "done")
	if r, err := waitJob(context.Background(), job, ch, always, time.Hour); err != nil || r != "done" {
		t.Errorf("got %q %v", r, err)
	}

	// The job vanished without a signal: unknown, not success.
	gone := func(context.Context) bool { return false }
	if r, err := waitJob(context.Background(), job, make(chan *dbus.Signal), gone, time.Millisecond); err != nil || r != "unknown" {
		t.Errorf("got %q %v", r, err)
	}

	// Signal already queued when polling finds the job gone.
	ch = make(chan *dbus.Signal, 1)
	ch <- removed(job, "timeout")
	if r, _ := waitJob(context.Background(), job, ch, gone, time.Nanosecond); r != "timeout" && r != "unknown" {
		t.Errorf("got %q", r)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := waitJob(ctx, job, make(chan *dbus.Signal), always, time.Millisecond); err == nil {
		t.Error("deadline ignored")
	}
}

func TestVerbMethodsAreTheOnlyMutations(t *testing.T) {
	var got []string
	for _, v := range []verb{verbRestart, verbStop, verbStart} {
		got = append(got, v.method())
	}
	if strings.Join(got, ",") != "org.freedesktop.systemd1.Manager.RestartUnit,org.freedesktop.systemd1.Manager.StopUnit,org.freedesktop.systemd1.Manager.StartUnit" {
		t.Errorf("methods = %v", got)
	}
}

func TestReloadCallsManagerReload(t *testing.T) {
	f := &fakeJobBus{}
	if err := actionsWith(f).Reload(context.Background()); err != nil || f.reloads != 1 || !f.closed || len(f.methods) != 0 {
		t.Fatalf("err %v reloads %d closed %v methods %v", err, f.reloads, f.closed, f.methods)
	}
	f = &fakeJobBus{reloadErr: errors.New("access denied")}
	if err := actionsWith(f).Reload(context.Background()); !errors.Is(err, ErrReload) {
		t.Errorf("err = %v", err)
	}
}
