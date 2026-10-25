// Package systemd reports the state of systemd units and, through the
// separate Actions type, starts, stops and restarts them.
//
// Bus only performs read-only D-Bus calls: Properties.GetAll/Get on a computed
// unit object path, and Manager.ListUnits. It never calls a Manager method that
// starts, stops, enables or otherwise changes anything; only Actions calls
// Manager.RestartUnit, StopUnit and StartUnit. Unit names come from
// policy and are re-validated here. Output is a fixed projection: only the
// listed properties are copied out, so ExecStart, Environment and the like
// are never returned or retained.
package systemd

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/mhmdxsadk/latch/internal/redact"
	"github.com/mhmdxsadk/latch/internal/textsafe"
)

const (
	busName      = "org.freedesktop.systemd1"
	managerPath  = dbus.ObjectPath("/org/freedesktop/systemd1")
	unitPrefix   = "/org/freedesktop/systemd1/unit/"
	ifaceUnit    = "org.freedesktop.systemd1.Unit"
	ifaceService = "org.freedesktop.systemd1.Service"
	ifaceManager = "org.freedesktop.systemd1.Manager"
	ifaceProps   = "org.freedesktop.DBus.Properties"
	listMethod   = ifaceManager + ".ListUnits"
	callTimeout  = 5 * time.Second
)

// Same pattern as config's unit name validation (defence in depth).
var unitName = regexp.MustCompile(`^[A-Za-z0-9:_.\\@-]{1,200}\.(service|socket|timer|mount|target|path)$`)

// failedUnitName also admits the unit types that cannot be queried by name
// but can fail (scopes, slices, swaps, devices, automounts).
var failedUnitName = regexp.MustCompile(`^[A-Za-z0-9:_.\\@-]{1,200}\.(service|socket|timer|mount|target|path|scope|slice|swap|device|automount)$`)

const (
	maxFailed          = 500
	maxDescriptionSize = 200
)

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

// ListedUnit is one loaded service unit and its state.
type ListedUnit struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ActiveState string `json:"active_state"`
	SubState    string `json:"sub_state"`
}

// ErrNotFound is returned when systemd reports LoadState "not-found".
var ErrNotFound = errors.New("unit not found")

// Client is the systemd interface the MCP layer uses; tests substitute a fake.
type Client interface {
	// Ping checks that the system bus is reachable and systemd answers.
	Ping(ctx context.Context) error
	// List returns the loaded service units, sorted by name. The caller must
	// filter the result through policy.
	List(ctx context.Context) ([]ListedUnit, error)
	// UnitStatus takes a unit name that has already been resolved by policy.
	UnitStatus(ctx context.Context, unit string) (UnitStatus, error)
	// Failed returns the units of any type that are failed or restarting,
	// sorted by name. The caller must filter the result through policy.
	Failed(ctx context.Context) ([]ListedUnit, error)
	// Logs returns the newest journal lines of a unit already resolved by
	// policy, at most lines entries and maxBytes bytes of text in total.
	Logs(ctx context.Context, unit string, lines, maxBytes int) (Logs, error)
}

// propertyReader abstracts the D-Bus transport so mapping is testable.
type propertyReader interface {
	getAll(ctx context.Context, path dbus.ObjectPath, iface string) (map[string]dbus.Variant, error)
}

// Bus reads unit properties from the system D-Bus.
type Bus struct {
	run runner // starts journalctl; nil means the real one
}

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

// List returns the loaded service units known to systemd.
func (*Bus) List(ctx context.Context) ([]ListedUnit, error) {
	rows, err := listRows(ctx)
	if err != nil {
		return nil, err
	}
	return buildList(rows), nil
}

// Failed returns the units that are failed or waiting to restart.
func (*Bus) Failed(ctx context.Context) ([]ListedUnit, error) {
	rows, err := listRows(ctx)
	if err != nil {
		return nil, err
	}
	return buildFailed(rows), nil
}

// listRows calls Manager.ListUnits, a read-only call.
func listRows(ctx context.Context) ([]listRow, error) {
	ctx, cancel, conn, err := open(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer func() { _ = conn.Close() }()
	var rows []listRow
	if err := conn.Object(busName, managerPath).CallWithContext(ctx, listMethod, 0).Store(&rows); err != nil {
		return nil, fmt.Errorf("list units: %w", err)
	}
	return rows, nil
}

// listRow is the (ssssssouso) struct returned by Manager.ListUnits.
type listRow struct {
	Name, Description, Load, Active, Sub, Following string
	Path                                            dbus.ObjectPath
	JobID                                           uint32
	JobType                                         string
	JobPath                                         dbus.ObjectPath
}

// buildList keeps valid service names only and projects them.
func buildList(rows []listRow) []ListedUnit {
	out := make([]ListedUnit, 0, len(rows))
	for _, r := range rows {
		if !unitName.MatchString(r.Name) || !strings.HasSuffix(r.Name, ".service") {
			continue
		}
		out = append(out, project(r))
	}
	return sortUnits(out)
}

// buildFailed keeps valid units, of any type, that are failed or waiting to
// restart, and projects them.
func buildFailed(rows []listRow) []ListedUnit {
	out := []ListedUnit{}
	for _, r := range rows {
		if !failedUnitName.MatchString(r.Name) {
			continue
		}
		if r.Active == "failed" || r.Sub == "auto-restart" {
			out = append(out, project(r))
		}
	}
	out = sortUnits(out)
	if len(out) > maxFailed {
		out = out[:maxFailed]
	}
	return out
}

func project(r listRow) ListedUnit {
	return ListedUnit{Name: r.Name, Description: cleanText(r.Description), ActiveState: r.Active, SubState: r.Sub}
}

func sortUnits(u []ListedUnit) []ListedUnit {
	slices.SortFunc(u, func(a, b ListedUnit) int { return strings.Compare(a.Name, b.Name) })
	return slices.CompactFunc(u, func(a, b ListedUnit) bool { return a.Name == b.Name })
}

// cleanText makes host-written text safe to show an agent.
func cleanText(s string) string {
	return textsafe.Head(redact.Text(textsafe.Line(s)), maxDescriptionSize)
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
	// An alias resolves to the unit it points at. Policy matched the alias
	// name, not the real one, so an excluded unit could be reached through
	// it. Only the unit's own name is accepted.
	if id := get[string](unitProps, "Id"); id != unit {
		return UnitStatus{}, ErrNotFound
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
		Description:    cleanText(get[string](unit, "Description")),
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
