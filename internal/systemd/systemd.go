// Package systemd reports the state of allowlisted systemd units.
//
// It only performs read-only D-Bus property reads (Properties.GetAll/Get on a
// computed unit object path). It never calls a Manager method, so it cannot
// start, stop, enable or otherwise change anything. Unit names come from
// policy and are re-validated here. Output is a fixed projection: only the
// listed properties are copied out, so ExecStart, Environment and the like
// are never returned or retained.
package systemd

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	busName      = "org.freedesktop.systemd1"
	managerPath  = dbus.ObjectPath("/org/freedesktop/systemd1")
	unitPrefix   = "/org/freedesktop/systemd1/unit/"
	ifaceUnit    = "org.freedesktop.systemd1.Unit"
	ifaceService = "org.freedesktop.systemd1.Service"
	ifaceManager = "org.freedesktop.systemd1.Manager"
	ifaceProps   = "org.freedesktop.DBus.Properties"
	callTimeout  = 5 * time.Second
)

// Same pattern as config's unit name validation (defence in depth).
var unitName = regexp.MustCompile(`^[A-Za-z0-9:_.\\@-]{1,200}\.(service|socket|timer|mount|target|path)$`)

// UnitStatus is the projected state of one unit.
type UnitStatus struct {
	LoadState      string `json:"load_state"`
	ActiveState    string `json:"active_state"`
	SubState       string `json:"sub_state"`
	UnitFileState  string `json:"unit_file_state"`
	Description    string `json:"description"`
	ActiveSince    string `json:"active_since,omitempty"`
	StateChangedAt string `json:"state_changed_at,omitempty"`
	// Service-only fields; zero/omitted for other unit types.
	Result         string `json:"result,omitempty"`
	Restarts       uint32 `json:"restarts"`
	ExecMainStatus int32  `json:"exec_main_status"`
}

// ErrNotFound is returned when systemd reports LoadState "not-found".
var ErrNotFound = errors.New("unit not found")

// Client is the systemd interface the MCP layer uses; tests substitute a fake.
type Client interface {
	// Ping checks that the system bus is reachable and systemd answers.
	Ping(ctx context.Context) error
	// UnitStatus takes a unit name that has already been resolved by policy.
	UnitStatus(ctx context.Context, unit string) (UnitStatus, error)
}

// propertyReader abstracts the D-Bus transport so mapping is testable.
type propertyReader interface {
	getAll(ctx context.Context, path dbus.ObjectPath, iface string) (map[string]dbus.Variant, error)
}

// Bus reads unit properties from the system D-Bus.
type Bus struct{}

var _ Client = (*Bus)(nil)

// NewBus returns a Bus. Connections are opened per call.
func NewBus() *Bus { return &Bus{} }

// connReader is the real propertyReader over a private bus connection.
type connReader struct{ conn *dbus.Conn }

func (r connReader) getAll(ctx context.Context, path dbus.ObjectPath, iface string) (map[string]dbus.Variant, error) {
	var props map[string]dbus.Variant
	call := r.conn.Object(busName, path).CallWithContext(ctx, ifaceProps+".GetAll", 0, iface)
	if err := call.Store(&props); err != nil {
		return nil, fmt.Errorf("read %s properties: %w", iface, err)
	}
	return props, nil
}

// open connects to the system bus; the caller must call the returned cancel
// and conn.Close.
func open(ctx context.Context) (context.Context, context.CancelFunc, *dbus.Conn, error) {
	cancel := context.CancelFunc(func() {})
	if _, ok := ctx.Deadline(); !ok {
		ctx, cancel = context.WithTimeout(ctx, callTimeout)
	}
	conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, nil, nil, errors.New("connect to system bus failed")
	}
	return ctx, cancel, conn, nil
}

// Ping connects to the system bus and reads systemd's Version property.
func (*Bus) Ping(ctx context.Context) error {
	ctx, cancel, conn, err := open(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	defer func() { _ = conn.Close() }()
	var v dbus.Variant
	call := conn.Object(busName, managerPath).CallWithContext(ctx, ifaceProps+".Get", 0, ifaceManager, "Version")
	if err := call.Store(&v); err != nil {
		return fmt.Errorf("read systemd version: %w", err)
	}
	if s, ok := v.Value().(string); !ok || s == "" {
		return errors.New("systemd version unavailable")
	}
	return nil
}

// UnitStatus reads the projected status of a unit.
func (*Bus) UnitStatus(ctx context.Context, unit string) (UnitStatus, error) {
	if !unitName.MatchString(unit) {
		return UnitStatus{}, errors.New("invalid unit name")
	}
	ctx, cancel, conn, err := open(ctx)
	if err != nil {
		return UnitStatus{}, err
	}
	defer cancel()
	defer func() { _ = conn.Close() }()
	return readStatus(ctx, connReader{conn}, unit)
}

// readStatus validates the name, reads the property maps and projects them.
func readStatus(ctx context.Context, r propertyReader, unit string) (UnitStatus, error) {
	if !unitName.MatchString(unit) {
		return UnitStatus{}, errors.New("invalid unit name")
	}
	path := dbus.ObjectPath(unitPrefix + escapePath(unit))
	unitProps, err := r.getAll(ctx, path, ifaceUnit)
	if err != nil {
		return UnitStatus{}, err
	}
	var svcProps map[string]dbus.Variant
	if strings.HasSuffix(unit, ".service") {
		if svcProps, err = r.getAll(ctx, path, ifaceService); err != nil {
			return UnitStatus{}, err
		}
	}
	return buildStatus(unitProps, svcProps)
}

// buildStatus projects property maps onto UnitStatus, ignoring everything else.
func buildStatus(unit, svc map[string]dbus.Variant) (UnitStatus, error) {
	st := UnitStatus{
		LoadState:      get[string](unit, "LoadState"),
		ActiveState:    get[string](unit, "ActiveState"),
		SubState:       get[string](unit, "SubState"),
		UnitFileState:  get[string](unit, "UnitFileState"),
		Description:    get[string](unit, "Description"),
		ActiveSince:    formatUsec(get[uint64](unit, "ActiveEnterTimestamp")),
		StateChangedAt: formatUsec(get[uint64](unit, "StateChangeTimestamp")),
		Result:         get[string](svc, "Result"),
		Restarts:       get[uint32](svc, "NRestarts"),
		ExecMainStatus: get[int32](svc, "ExecMainStatus"),
	}
	if st.LoadState == "not-found" {
		return UnitStatus{}, ErrNotFound
	}
	return st, nil
}

// get returns props[key] as T, or the zero value if missing or mistyped.
func get[T any](props map[string]dbus.Variant, key string) T {
	v, _ := props[key].Value().(T)
	return v
}

func formatUsec(us uint64) string {
	if us == 0 || us > 1<<62 {
		return ""
	}
	return time.UnixMicro(int64(us)).UTC().Format(time.RFC3339) //nolint:gosec // bounded above
}

// escapePath applies systemd's bus path escaping: every byte outside
// [A-Za-z0-9] (and a leading digit) becomes "_" plus two lowercase hex digits.
func escapePath(s string) string {
	if s == "" {
		return "_"
	}
	const hex = "0123456789abcdef"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' && i > 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('_')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0xf])
	}
	return b.String()
}
