package mcp

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mhmdxsadk/latch/internal/approval"
	"github.com/mhmdxsadk/latch/internal/files"
	"github.com/mhmdxsadk/latch/internal/policy"
	"github.com/mhmdxsadk/latch/internal/redact"
	"github.com/mhmdxsadk/latch/internal/textsafe"
)

// DefaultRestartSettle is how long Latch waits after the restart that
// follows a file change before it checks that the target is up.
const DefaultRestartSettle = 30 * time.Second

// Describe bounds: what "latch pending" shows of one change.
const (
	maxDiffLines    = 500
	maxDetailBytes  = 500
	maxCreateLines  = 200
	maxDescribeBody = 4 << 10
)

var (
	errRestartTarget = userError{"restart must name exactly one of container or unit"}
	errRestartSelf   = userError{"Latch cannot restart latch.service after a change; restart it yourself later"}
	errBadMode       = userError{"mode must be an octal string such as 0644 or 0600: no setuid, setgid or sticky bits, and not writable by everyone"}
	errNoBackupID    = userError{"backup must be a backup id from file_backups, such as 20261025T143000Z"}

	// errRolledBack, errRollbackFailed and errReloadFailed end an execution
	// whose summary already says what happened.
	errRolledBack     = errors.New("change rolled back")
	errRollbackFailed = errors.New("rollback failed")
	errReloadFailed   = errors.New("daemon-reload failed")
	errHTTPFailed     = errors.New("http request failed")
)

type restartIn struct {
	Container string `json:"container,omitempty" jsonschema:"exact container name, from docker_list"`
	Unit      string `json:"unit,omitempty" jsonschema:"full systemd unit name, for example nginx.service"`
}

type fileEditIn struct {
	Path    string     `json:"path" jsonschema:"absolute, clean path of an existing text file, e.g. /etc/nginx/nginx.conf"`
	Old     string     `json:"old" jsonschema:"text to replace, exactly as file_read shows it (including [hidden]); it must occur exactly once; 1 byte to 64 KiB"`
	New     string     `json:"new" jsonschema:"replacement text, up to 64 KiB; may be empty to delete"`
	Restart *restartIn `json:"restart,omitempty" jsonschema:"optional container or unit to restart after the edit; the edit is rolled back if it is not up 30 seconds later"`
}

type fileCreateIn struct {
	Path    string `json:"path" jsonschema:"absolute, clean path of the new file; its folder must exist"`
	Content string `json:"content" jsonschema:"text content, up to 1 MiB"`
	Mode    string `json:"mode,omitempty" jsonschema:"permission bits as an octal string; default 0644"`
}

type fileBackupsIn struct {
	Path string `json:"path" jsonschema:"absolute, clean path of a file"`
}

type fileRestoreIn struct {
	Path    string     `json:"path" jsonschema:"absolute, clean path of the file to restore"`
	Backup  string     `json:"backup" jsonschema:"backup id from file_backups, such as 20261025T143000Z"`
	Restart *restartIn `json:"restart,omitempty" jsonschema:"optional container or unit to restart after the restore; it is rolled back if the target is not up 30 seconds later"`
}

type backupOut struct {
	ID    string `json:"id"`
	Time  string `json:"time"`
	Bytes int64  `json:"size_bytes"`
}

type fileBackupsOut struct {
	Path    string      `json:"path"`
	Backups []backupOut `json:"backups"`
}

// restartTarget is a container or unit restarted after a file change.
type restartTarget struct {
	kind string // "container" or "unit"
	name string
}

func (t restartTarget) label() string { return t.kind + " " + t.name }

// restartTarget resolves the optional restart input through policy.
func (d *Deps) restartTarget(ctx context.Context, in *restartIn) (*restartTarget, error) {
	if in == nil {
		return nil, nil
	}
	var t restartTarget
	switch {
	case in.Container != "" && in.Unit == "":
		name, err := d.Policy.Container(in.Container)
		if err != nil {
			return nil, err
		}
		t = restartTarget{"container", name}
	case in.Unit != "" && in.Container == "":
		unit, err := d.Policy.SystemdUnit(in.Unit)
		if err != nil {
			return nil, err
		}
		if unit == "latch.service" {
			return nil, errRestartSelf
		}
		t = restartTarget{"unit", unit}
	default:
		return nil, errRestartTarget
	}
	if err := d.checkRestartTarget(ctx, t); err != nil {
		return nil, err
	}
	return &t, nil
}

// checkRestartTarget verifies the target is visible and exists. A target on
// the approve list is fine: the whole change is approved.
func (d *Deps) checkRestartTarget(ctx context.Context, t restartTarget) error {
	if t.kind == "container" {
		if !d.Policy.ContainerVisible(t.name) {
			return policy.ErrNotAllowed
		}
		_, err := d.Docker.Status(ctx, t.name)
		return err
	}
	if !d.Policy.UnitVisible(t.name) {
		return policy.ErrNotAllowed
	}
	_, err := d.Systemd.UnitStatus(ctx, t.name)
	return err
}

func (d *Deps) settle() time.Duration {
	if d.RestartSettle > 0 {
		return d.RestartSettle
	}
	return DefaultRestartSettle
}

// restartAndCheck restarts t, waits, and reports whether it is up. why says
// what was wrong, in fixed words and validated names.
func (d *Deps) restartAndCheck(ctx context.Context, t restartTarget) (up bool, why string) {
	var err error
	if t.kind == "container" {
		err = d.DockerActions.Restart(ctx, t.name)
	} else {
		_, err = d.SystemdActions.Restart(ctx, t.name)
	}
	if err != nil {
		return false, "the restart of " + t.label() + " failed"
	}
	wait := d.settle()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, t.label() + " was not checked: the change ran out of time"
	case <-timer.C:
	}
	after := " " + wait.String() + " after the restart"
	if t.kind == "container" {
		st, err := d.Docker.Status(ctx, t.name)
		switch {
		case err != nil:
			return false, "container " + t.name + " could not be checked" + after
		case st.Restarting:
			return false, "container " + t.name + " was restarting" + after
		case !st.Running || st.State != "running":
			return false, "container " + t.name + " was not running (" + textsafe.Line(st.State) + ")" + after
		case st.Health.Status == "unhealthy":
			return false, "container " + t.name + " was unhealthy" + after
		}
		return true, ""
	}
	st, err := d.Systemd.UnitStatus(ctx, t.name)
	switch {
	case err != nil:
		return false, "unit " + t.name + " could not be checked" + after
	case st.ActiveState != "active":
		return false, "unit " + t.name + " was " + textsafe.Line(st.ActiveState) + after
	}
	return true, ""
}

// reload runs a daemon-reload when path is a unit file.
func (d *Deps) reloadFor(ctx context.Context, path string) (string, error) {
	if !files.IsUnitPath(path) {
		return "", nil
	}
	if err := d.SystemdActions.Reload(ctx); err != nil {
		return "; daemon-reload failed", errReloadFailed
	}
	return "; daemon-reload done", nil
}

// clip cuts a line for "latch pending" and says so.
func clip(s string) string {
	if len(s) <= maxDetailBytes {
		return s
	}
	return textsafe.Head(s, maxDetailBytes) + " ... (line cut for display)"
}

// diffLines is a unified diff of two views for "latch pending".
func diffLines(path string, before, after []string) []string {
	d := files.Diff(before, after, 3)
	out := []string{"--- " + path + " (now)", "+++ " + path + " (after the change)"}
	for i, l := range d {
		if i == maxDiffLines {
			out = append(out, fmt.Sprintf("... %d more diff lines not shown; the whole change is applied", len(d)-i))
			break
		}
		out = append(out, clip(l))
	}
	return out
}

func restartPlan(t *restartTarget, settle time.Duration) []string {
	if t == nil {
		return nil
	}
	return []string{"then restart " + t.label() + "; if it is not up " + settle.String() +
		" later, put the old content back and restart it again"}
}

// fileChange replaces the content of an existing file: file_edit and
// file_restore. The request holds the file as it was when the request was
// made; the change only runs if the file is still exactly that.
type fileChange struct {
	d        *Deps
	kind     string
	path     string     // as the agent gave it
	file     files.File // real content at request time: never shown
	content  []byte     // new real content: never shown
	restart  *restartTarget
	backupID string // file_restore: the backup restored
	details  []string
}

func (a *fileChange) Kind() string { return a.kind }
func (a *fileChange) Resource() approval.Resource {
	return approval.Resource{Type: "file", Name: a.file.Path}
}
func (a *fileChange) Mode() approval.Mode { return approval.Always }
func (a *fileChange) Describe() []string  { return a.details }

func (a *fileChange) Params() []approval.Param {
	ps := []approval.Param{{Name: "path", Value: a.file.Path}}
	if a.backupID != "" {
		ps = append(ps, approval.Param{Name: "backup", Value: a.backupID})
	}
	if a.restart != nil {
		ps = append(ps, approval.Param{Name: "restart", Value: a.restart.label()})
	}
	return ps
}

func (a *fileChange) Check(ctx context.Context) error {
	cur, err := a.d.Editor.Load(ctx, a.path)
	if err != nil {
		return err
	}
	if cur.Path != a.file.Path || cur.Hash != a.file.Hash {
		return files.ErrChanged
	}
	if a.restart != nil {
		return a.d.checkRestartTarget(ctx, *a.restart)
	}
	return nil
}

func (a *fileChange) Execute(ctx context.Context) (approval.Result, error) {
	res, err := a.d.Editor.Replace(ctx, a.path, a.file, a.content, true)
	if err != nil {
		return approval.Result{}, err
	}
	verb := "edited "
	if a.kind == "file_restore" {
		verb = "restored " + a.backupID + " to "
	}
	sum := verb + a.file.Path + ", backup " + res.BackupID
	note, err := a.d.reloadFor(ctx, a.file.Path)
	sum += note
	if err != nil || a.restart == nil {
		return approval.Result{Summary: sum}, err
	}
	up, why := a.d.restartAndCheck(ctx, *a.restart)
	if up {
		return approval.Result{Summary: sum + "; restarted " + a.restart.label() + ", up"}, nil
	}

	// Roll back: put the old content back, but only over exactly what was
	// written.
	written := files.File{Path: a.file.Path, Hash: sha256.Sum256(a.content)}
	if _, err := a.d.Editor.Replace(ctx, a.path, written, res.Old, false); err != nil {
		return approval.Result{Summary: sum + "; " + why + "; rollback failed (the file changed or could not be written), so the new content stays; backup " + res.BackupID + " has the old content"}, errRollbackFailed
	}
	sum += "; rolled back: " + why + "; the old content is back"
	if note, err := a.d.reloadFor(ctx, a.file.Path); err != nil {
		return approval.Result{Summary: sum + note}, errRolledBack
	}
	if up, _ := a.d.restartAndCheck(ctx, *a.restart); up {
		sum += "; restarted " + a.restart.label() + " again, up"
	} else {
		sum += "; restarted " + a.restart.label() + " again, still not up"
	}
	return approval.Result{Summary: sum}, errRolledBack
}

// fileCreate creates a new file.
type fileCreate struct {
	d       *Deps
	path    string
	target  files.NewFile
	content []byte // never shown in full
	mode    fs.FileMode
	details []string
}

func (a *fileCreate) Kind() string { return "file_create" }
func (a *fileCreate) Resource() approval.Resource {
	return approval.Resource{Type: "file", Name: a.target.Path}
}
func (a *fileCreate) Mode() approval.Mode { return approval.Always }
func (a *fileCreate) Describe() []string  { return a.details }
func (a *fileCreate) Params() []approval.Param {
	return []approval.Param{{Name: "path", Value: a.target.Path}, {Name: "mode", Value: modeText(a.mode)}}
}

func (a *fileCreate) Check(ctx context.Context) error {
	nf, err := a.d.Editor.ResolveNew(ctx, a.path)
	if err != nil {
		return err
	}
	if nf != a.target {
		return files.ErrChanged
	}
	return nil
}

func (a *fileCreate) Execute(ctx context.Context) (approval.Result, error) {
	if err := a.d.Editor.Create(ctx, a.path, a.target, a.content, a.mode); err != nil {
		return approval.Result{}, err
	}
	sum := "created " + a.target.Path + " (mode " + modeText(a.mode) + ")"
	note, err := a.d.reloadFor(ctx, a.target.Path)
	return approval.Result{Summary: sum + note}, err
}

func modeText(m fs.FileMode) string { return fmt.Sprintf("%04o", uint32(m.Perm())) }

var modePattern = regexp.MustCompile(`^0?[0-7]{3}$`)

// parseMode accepts permission bits only, not writable by everyone.
func parseMode(s string) (fs.FileMode, error) {
	if s == "" {
		return 0o644, nil
	}
	if !modePattern.MatchString(s) {
		return 0, errBadMode
	}
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil || v&0o002 != 0 {
		return 0, errBadMode
	}
	return fs.FileMode(v), nil
}

func requestedAs(agent, real string) []string {
	if agent == real {
		return nil
	}
	return []string{"requested as " + agent + " (a symlink to it)"}
}

func registerFileActions(s *sdk.Server, d *Deps) {
	sdk.AddTool(s, &sdk.Tool{
		Name: "file_edit",
		Description: "Replace text in an existing text file (at most 1 MiB) on the host. 'old' is text exactly as file_read shows it and " +
			"must occur exactly once; only the lines it covers change, and the rest of the file is kept byte for byte. " +
			"Lines that show [hidden] keep their real value when left unchanged; a changed line must not contain [hidden] " +
			"(write the full value instead). Files whose hidden values span several lines (private keys) cannot be edited. " +
			"Latch keeps the file's owner, permissions and line endings, makes a backup first (see file_backups), " +
			"runs a systemd daemon-reload after editing a unit file, and with 'restart' restarts that container or unit " +
			"afterwards and puts the old content back if it is not up 30 seconds later." + alwaysNote + approvalNote,
		Annotations: changeAnnotations(true, false),
	}, wrap(d, "file_edit", func(ctx context.Context, in fileEditIn) (actionOut, string, error) {
		if err := files.ValidatePath(in.Path); err != nil {
			return actionOut{}, "", err
		}
		t, err := d.restartTarget(ctx, in.Restart)
		if err != nil {
			return actionOut{}, in.Path, err
		}
		file, err := d.Editor.Load(ctx, in.Path)
		if err != nil {
			return actionOut{}, in.Path, err
		}
		plan, err := files.PlanEdit(file.Content, in.Old, in.New)
		if err != nil {
			return actionOut{}, in.Path, err
		}
		details := append([]string{"edit " + file.Path}, requestedAs(in.Path, file.Path)...)
		details = append(details, diffLines(file.Path, plan.Before, plan.After)...)
		if plan.Kept > 0 {
			details = append(details, fmt.Sprintf("%d changed-area line(s) with hidden values keep their real value", plan.Kept))
		}
		if files.IsUnitPath(file.Path) {
			details = append(details, "then run systemd daemon-reload")
		}
		details = append(details, restartPlan(t, d.settle())...)
		out, err := d.submit(ctx, &fileChange{d: d, kind: "file_edit", path: in.Path, file: file, content: plan.Content, restart: t, details: details})
		return out, in.Path, err
	}))

	sdk.AddTool(s, &sdk.Tool{
		Name: "file_create",
		Description: "Create a new text file (at most 1 MiB) on the host. It fails if the file exists; use file_edit for that. " +
			"The folder must exist. The file gets the folder's owner and group and the given mode (default 0644; no setuid, " +
			"setgid or sticky bits, and never writable by everyone). The content must not contain [hidden]. " +
			"A new unit file gets a systemd daemon-reload." + alwaysNote + approvalNote,
		Annotations: changeAnnotations(false, false),
	}, wrap(d, "file_create", func(ctx context.Context, in fileCreateIn) (actionOut, string, error) {
		if err := files.ValidatePath(in.Path); err != nil {
			return actionOut{}, "", err
		}
		mode, err := parseMode(in.Mode)
		if err != nil {
			return actionOut{}, in.Path, err
		}
		switch {
		case len(in.Content) > files.MaxEditBytes:
			return actionOut{}, in.Path, files.ErrTooLarge
		case !files.PlainText(in.Content, true):
			return actionOut{}, in.Path, files.ErrBadText
		case strings.Contains(in.Content, redact.Hidden):
			return actionOut{}, in.Path, files.ErrNewHidden
		}
		nf, err := d.Editor.ResolveNew(ctx, in.Path)
		if err != nil {
			return actionOut{}, in.Path, err
		}
		view := files.ViewLines([]byte(in.Content))
		details := append([]string{fmt.Sprintf("create %s (mode %s, owner uid %d gid %d, %d bytes)", nf.Path, modeText(mode), nf.UID, nf.GID, len(in.Content))},
			requestedAs(in.Path, nf.Path)...)
		details = append(details, "content:")
		for i, l := range view {
			if i == maxCreateLines {
				details = append(details, fmt.Sprintf("... %d more lines not shown", len(view)-i))
				break
			}
			details = append(details, "  "+clip(l))
		}
		if files.IsUnitPath(nf.Path) {
			details = append(details, "then run systemd daemon-reload")
		}
		out, err := d.submit(ctx, &fileCreate{d: d, path: in.Path, target: nf, content: []byte(in.Content), mode: mode, details: details})
		return out, in.Path, err
	}))

	sdk.AddTool(s, &sdk.Tool{
		Name: "file_backups",
		Description: "List the backups Latch keeps of a file (the 3 newest, made before each change): id, time and size. " +
			"Their content is never shown; file_restore puts one back.",
		Annotations: readOnly,
	}, wrap(d, "file_backups", func(ctx context.Context, in fileBackupsIn) (fileBackupsOut, string, error) {
		if err := files.ValidatePath(in.Path); err != nil {
			return fileBackupsOut{}, "", err
		}
		real, err := d.Editor.Locate(ctx, in.Path)
		if err != nil {
			return fileBackupsOut{}, in.Path, err
		}
		list, err := d.Editor.Backups().List(real)
		if err != nil {
			return fileBackupsOut{}, in.Path, err
		}
		out := fileBackupsOut{Path: real, Backups: []backupOut{}}
		for _, b := range list {
			out.Backups = append(out.Backups, backupOut{ID: b.ID, Time: stamp(b.Time), Bytes: b.Size})
		}
		return out, in.Path, nil
	}))

	sdk.AddTool(s, &sdk.Tool{
		Name: "file_restore",
		Description: "Put a backup from file_backups back in place of the file's current content. Latch backs up the current " +
			"content first, keeps the file's owner and permissions, runs a systemd daemon-reload for a unit file, and with " +
			"'restart' restarts that container or unit and undoes the restore if it is not up 30 seconds later." +
			alwaysNote + approvalNote,
		Annotations: changeAnnotations(true, false),
	}, wrap(d, "file_restore", func(ctx context.Context, in fileRestoreIn) (actionOut, string, error) {
		if err := files.ValidatePath(in.Path); err != nil {
			return actionOut{}, "", err
		}
		if !files.ValidBackupID(in.Backup) {
			return actionOut{}, in.Path, errNoBackupID
		}
		t, err := d.restartTarget(ctx, in.Restart)
		if err != nil {
			return actionOut{}, in.Path, err
		}
		file, err := d.Editor.Load(ctx, in.Path)
		if err != nil {
			return actionOut{}, in.Path, err
		}
		content, err := d.Editor.Backups().Read(file.Path, in.Backup)
		if err != nil {
			return actionOut{}, in.Path, err
		}
		before, after := files.ViewLines(file.Content), files.ViewLines(content)
		details := append([]string{"restore backup " + in.Backup + " of " + file.Path}, requestedAs(in.Path, file.Path)...)
		if d := diffLines(file.Path, before, after); len(d) > 2 {
			details = append(details, d...)
		} else {
			details = append(details, "the backup shows the same text as the file now")
		}
		if files.IsUnitPath(file.Path) {
			details = append(details, "then run systemd daemon-reload")
		}
		details = append(details, restartPlan(t, d.settle())...)
		out, err := d.submit(ctx, &fileChange{d: d, kind: "file_restore", path: in.Path, file: file, content: content, restart: t, backupID: in.Backup, details: details})
		return out, in.Path, err
	}))
}
