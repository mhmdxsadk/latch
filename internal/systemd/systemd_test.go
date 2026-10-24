package systemd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
)

type fakeReader struct {
	props map[string]map[string]dbus.Variant
	err   error
	calls []string
	paths []dbus.ObjectPath
}

func (f *fakeReader) getAll(_ context.Context, p dbus.ObjectPath, iface string) (map[string]dbus.Variant, error) {
	f.calls = append(f.calls, iface)
	f.paths = append(f.paths, p)
	if f.err != nil {
		return nil, f.err
	}
	return f.props[iface], nil
}

func v(x any) dbus.Variant { return dbus.MakeVariant(x) }

func TestEscapePath(t *testing.T) {
	tests := []struct{ in, want string }{
		{"ssh.service", "ssh_2eservice"},
		{"dbus-org.freedesktop.login1.service", "dbus_2dorg_2efreedesktop_2elogin1_2eservice"},
		{"getty@tty1.service", "getty_40tty1_2eservice"},
		{"1abc.service", "_31abc_2eservice"},
		{"", "_"},
		{"a/b", "a_2fb"},
		{"a\x00b", "a_00b"},
		{"é", "_c3_a9"},
		{`a\x2db.service`, "a_5cx2db_2eservice"},
	}
	for _, tt := range tests {
		got := escapePath(tt.in)
		if got != tt.want {
			t.Errorf("escapePath(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if p := dbus.ObjectPath(unitPrefix + got); !p.IsValid() {
			t.Errorf("path %q invalid", p)
		}
	}
}

func fullProps() map[string]map[string]dbus.Variant {
	return map[string]map[string]dbus.Variant{
		ifaceUnit: {
			"Id":                   v("ssh.service"),
			"LoadState":            v("loaded"),
			"ActiveState":          v("active"),
			"SubState":             v("running"),
			"UnitFileState":        v("enabled"),
			"Description":          v("OpenBSD Secure Shell server"),
			"ActiveEnterTimestamp": v(uint64(1700000000_000000)),
			"StateChangeTimestamp": v(uint64(1700000100_000000)),
			"ExecStart":            v("SECRET=hunter2 /usr/sbin/sshd"),
			"Environment":          v([]string{"SECRET=hunter2"}),
		},
		ifaceService: {
			"Result":         v("success"),
			"NRestarts":      v(uint32(3)),
			"ExecMainStatus": v(int32(1)),
			"ExecStart":      v("SECRET=hunter2"),
			"Environment":    v([]string{"SECRET=hunter2"}),
		},
	}
}

func TestReadStatusService(t *testing.T) {
	f := &fakeReader{props: fullProps()}
	got, err := readStatus(context.Background(), f, "ssh.service")
	if err != nil {
		t.Fatal(err)
	}
	want := UnitStatus{
		LoadState: "loaded", ActiveState: "active", SubState: "running",
		UnitFileState: "enabled", Description: "OpenBSD Secure Shell server",
		ActiveSince: "2023-11-14T22:13:20Z", StateChangedAt: "2023-11-14T22:15:00Z",
		Result: "success", Restarts: 3, ExecMainStatus: 1,
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if len(f.calls) != 2 || f.calls[0] != ifaceUnit || f.calls[1] != ifaceService {
		t.Errorf("calls = %v", f.calls)
	}
	if f.paths[0] != "/org/freedesktop/systemd1/unit/ssh_2eservice" {
		t.Errorf("path = %q", f.paths[0])
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "SECRET") || strings.Contains(string(b), "hunter2") {
		t.Errorf("leak: %s", b)
	}
}

func TestReadStatusNonService(t *testing.T) {
	f := &fakeReader{props: fullProps()}
	f.props[ifaceUnit]["Id"] = v("apt-daily.timer")
	got, err := readStatus(context.Background(), f, "apt-daily.timer")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0] != ifaceUnit {
		t.Errorf("calls = %v", f.calls)
	}
	if got.Result != "" || got.Restarts != 0 || got.ExecMainStatus != 0 {
		t.Errorf("service fields set: %+v", got)
	}
}

func TestBuildStatusEdgeCases(t *testing.T) {
	tests := []struct {
		name      string
		unit, svc map[string]dbus.Variant
		want      UnitStatus
		err       error
	}{
		{"nil maps", nil, nil, UnitStatus{}, nil},
		{"zero timestamps", map[string]dbus.Variant{
			"ActiveEnterTimestamp": v(uint64(0)), "StateChangeTimestamp": v(uint64(0)),
		}, nil, UnitStatus{}, nil},
		{"wrong types", map[string]dbus.Variant{
			"LoadState": v(5), "ActiveState": v([]string{"x"}), "ActiveEnterTimestamp": v("1"),
			"Description": v(uint64(1)),
		}, map[string]dbus.Variant{
			"Result": v(1), "NRestarts": v("3"), "ExecMainStatus": v(uint64(2)),
		}, UnitStatus{}, nil},
		{"huge timestamp", map[string]dbus.Variant{
			"ActiveEnterTimestamp": v(^uint64(0)),
		}, nil, UnitStatus{}, nil},
		{"not found", map[string]dbus.Variant{"LoadState": v("not-found")}, nil, UnitStatus{}, ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildStatus(tt.unit, tt.svc)
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestReadStatusNotFound(t *testing.T) {
	f := &fakeReader{props: map[string]map[string]dbus.Variant{
		ifaceUnit: {"Id": v("nope.service"), "LoadState": v("not-found")}, ifaceService: {},
	}}
	if _, err := readStatus(context.Background(), f, "nope.service"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

// A unit alias answers with the real unit's properties. That must not be
// returned, or an excluded unit could be read under its alias.
func TestAliasIsNotFound(t *testing.T) {
	f := &fakeReader{props: fullProps()}
	if _, err := readStatus(context.Background(), f, "sshd.service"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestBuildList(t *testing.T) {
	rows := []listRow{
		{Name: "ssh.service", Description: "SSH", Active: "active", Sub: "running"},
		{Name: "cron.service", Active: "active", Sub: "running"},
		{Name: "cron.service", Active: "active", Sub: "running"},
		{Name: "dbus.socket", Active: "active"},
		{Name: "-.mount", Active: "active"},
		{Name: "bad name.service"},
		{Name: "../x.service"},
	}
	got := buildList(rows)
	if len(got) != 2 || got[0].Name != "cron.service" || got[1].Name != "ssh.service" || got[1].ActiveState != "active" {
		t.Errorf("got %+v", got)
	}
}

func TestInvalidNames(t *testing.T) {
	names := []string{"../x.service", "ssh", "a/b.service", "", "ssh.service;id", "a b.service",
		"a\x00b.service", "x.swap", ".service", "ssh.service\n", strings.Repeat("a", 201) + ".service"}
	for _, n := range names {
		f := &fakeReader{props: fullProps()}
		if _, err := readStatus(context.Background(), f, n); err == nil {
			t.Errorf("%q: expected error", n)
		}
		if len(f.calls) != 0 {
			t.Errorf("%q: reader called", n)
		}
		if _, err := NewBus().UnitStatus(context.Background(), n); err == nil || err.Error() != "invalid unit name" {
			t.Errorf("%q: Bus err = %v", n, err)
		}
	}
}

func TestReaderErrorPropagates(t *testing.T) {
	boom := errors.New("boom")
	f := &fakeReader{err: boom}
	if _, err := readStatus(context.Background(), f, "ssh.service"); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
}

func TestRealBus(t *testing.T) {
	if os.Getenv("LATCH_TEST_SYSTEMD") != "1" {
		t.Skip("set LATCH_TEST_SYSTEMD=1 to test against the real system bus")
	}
	b := NewBus()
	if err := b.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := b.UnitStatus(context.Background(), "dbus.service")
	if err != nil {
		t.Fatal(err)
	}
	if st.LoadState == "" {
		t.Errorf("empty status: %+v", st)
	}
}
