package mcp

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mhmdxsadk/latch/internal/approval"
	"github.com/mhmdxsadk/latch/internal/docker"
	"github.com/mhmdxsadk/latch/internal/files"
	"github.com/mhmdxsadk/latch/internal/policy"
	"github.com/mhmdxsadk/latch/internal/redact"
	"github.com/mhmdxsadk/latch/internal/textsafe"
)

// After a file change with a restart, Latch waits DefaultRestartSettle and
// checks the target. A container whose health check still says "starting"
// is checked every DefaultHealthPoll until DefaultHealthWait after the
// restart. If the target is not up, the change stays and Latch files an undo
// request that runs by itself after DefaultUndoWait unless the owner denies it.
const (
	DefaultRestartSettle = 30 * time.Second
	DefaultHealthWait    = 2 * time.Minute
	DefaultHealthPoll    = 5 * time.Second
	DefaultUndoWait      = 10 * time.Minute
)

// afterRestartNote tells the agent what happens after the restart.
const afterRestartNote = ". Latch then checks it (waiting for its health check, up to 2 minutes) and returns its log lines " +
	"since the restart. If it is not up, the change stays and Latch files a request to undo it, which runs by itself " +
	"after 10 minutes unless the owner denies it: tell the owner what the logs show so they can choose between the " +
	"undo and a fix."

// Log lines returned after a restart.
const (
	restartLogLines = 50
	restartLogBytes = 8 << 10
)

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

	// errTargetDown, errStillDown and errReloadFailed end an execution
	// whose summary already says what happened.
	errTargetDown   = errors.New("target not up after the change")
	errStillDown    = errors.New("target still not up after the undo")
	errReloadFailed = errors.New("daemon-reload failed")
	errHTTPFailed   = errors.New("http request failed")
)

type restartIn struct {
	Container string `json:"container,omitempty" jsonschema:"exact container name, from docker_list"`
	Unit      string `json:"unit,omitempty" jsonschema:"full systemd unit name, for example nginx.service"`
}

type fileEditIn struct {
	Path    string     `json:"path" jsonschema:"absolute, clean path of an existing text file, e.g. /etc/nginx/nginx.conf"`
	Old     string     `json:"old" jsonschema:"text to replace, exactly as file_read shows it (including [hidden]); it must occur exactly once; 1 byte to 64 KiB"`
	New     string     `json:"new" jsonschema:"replacement text, up to 64 KiB; may be empty to delete"`
	Restart *restartIn `json:"restart,omitempty" jsonschema:"optional container or unit to restart after the edit (for the container's own compose file Latch runs docker compose up instead, which applies it); Latch then reports whether it is up, with its new log lines"`
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
	Restart *restartIn `json:"restart,omitempty" jsonschema:"optional container or unit to restart after the restore (docker compose up for the container's own compose file); Latch then reports whether it is up, with its new log lines"`
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
	// compose is set when the changed file is the container's compose file:
	// a restart would not apply it, so Latch runs docker compose up instead.
	compose *docker.ComposeTarget
}

func (t restartTarget) label() string { return t.kind + " " + t.name }

// done says what was done to the target, for a result summary.
func (t restartTarget) done() string {
	if t.compose != nil {
		return "ran docker compose up for " + t.label()
	}
	return "restarted " + t.label()
}

// verb says how the target is brought back after the change.
func (t restartTarget) verb() string {
	if t.compose != nil {
		return "apply it with docker compose up for " + t.label()
	}
	return "restart " + t.label()
}

// withCompose switches a container restart to docker compose up when path is
// one of the container's compose files.
func (d *Deps) withCompose(ctx context.Context, t *restartTarget, path string) (*restartTarget, error) {
	if t == nil || t.kind != "container" {
		return t, nil
	}
	ct, err := d.DockerActions.ComposeTarget(ctx, t.name)
	if errors.Is(err, docker.ErrNotCompose) || errors.Is(err, docker.ErrBadCompose) {
		return t, nil
	}
	if err != nil {
		return nil, err
	}
	if !composeFile(ct, path) {
		return t, nil
	}
	if err := checkSiblings(d.Policy, ct); err != nil {
		return nil, err
	}
	c := *t
	c.compose = &ct
	return &c, nil
}

// composeFile reports whether path, a resolved path, is one of t's files.
func composeFile(t docker.ComposeTarget, path string) bool {
	for _, f := range t.ConfigFiles {
		if f == path {
			return true
		}
		if real, err := filepath.EvalSymlinks(f); err == nil && real == path {
			return true
		}
	}
	return false
}

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
		t = restartTarget{kind: "container", name: name}
	case in.Unit != "" && in.Container == "":
		unit, err := d.Policy.SystemdUnit(in.Unit)
		if err != nil {
			return nil, err
		}
		if unit == "latch.service" {
			return nil, errRestartSelf
		}
		t = restartTarget{kind: "unit", name: unit}
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
		if t.compose != nil {
			ct, err := d.DockerActions.ComposeTarget(ctx, t.name)
			if err != nil {
				return err
			}
			if !ct.Same(*t.compose) {
				return errComposeChanged
			}
			if err := checkSiblings(d.Policy, ct); err != nil {
				return err
			}
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

func orDefault(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

func (d *Deps) settle() time.Duration     { return orDefault(d.RestartSettle, DefaultRestartSettle) }
func (d *Deps) healthWait() time.Duration { return orDefault(d.HealthWait, DefaultHealthWait) }
func (d *Deps) healthPoll() time.Duration { return orDefault(d.HealthPoll, DefaultHealthPoll) }
func (d *Deps) undoWait() time.Duration   { return orDefault(d.UndoWait, DefaultUndoWait) }

// sleep waits for wait or the end of ctx, and reports whether it waited.
func sleep(ctx context.Context, wait time.Duration) bool {
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// restartCheck is what Latch saw after the restart that follows a change.
type restartCheck struct {
	up   bool
	why  string   // when not up: what was wrong, in fixed words and validated names
	note string   // when up: anything worth saying, such as a health check still starting
	logs []string // log lines written since the restart, already redacted
}

// restartAndCheck restarts t, waits, and reports whether it is up.
func (d *Deps) restartAndCheck(ctx context.Context, t restartTarget) restartCheck {
	start := time.Now()
	var err error
	switch {
	case t.compose != nil:
		res, err := d.DockerActions.ComposeUp(ctx, *t.compose, false)
		if err != nil {
			why := fmt.Sprintf("docker compose up for %s failed (exit status %d)", t.label(), res.ExitCode)
			if res.TimedOut {
				why = "docker compose up for " + t.label() + " did not finish in time"
			}
			return restartCheck{why: why, logs: append([]string{"docker compose output:"}, res.Output...)}
		}
	case t.kind == "container":
		err = d.DockerActions.Restart(ctx, t.name)
	default:
		_, err = d.SystemdActions.Restart(ctx, t.name)
	}
	if err != nil {
		return restartCheck{why: "the restart of " + t.label() + " failed"}
	}
	if !sleep(ctx, d.settle()) {
		return restartCheck{why: t.label() + " was not checked: the change ran out of time"}
	}
	c := d.checkUp(ctx, t, start)
	c.logs = d.logsSince(ctx, t, start)
	return c
}

func (d *Deps) checkUp(ctx context.Context, t restartTarget, start time.Time) restartCheck {
	down := func(what string) restartCheck {
		return restartCheck{why: t.label() + " " + what + " " + time.Since(start).Round(time.Second).String() + " after the restart"}
	}
	if t.kind == "unit" {
		st, err := d.Systemd.UnitStatus(ctx, t.name)
		switch {
		case err != nil:
			return down("could not be checked")
		case st.ActiveState != "active":
			return down("was " + textsafe.Line(st.ActiveState))
		}
		return restartCheck{up: true}
	}
	for {
		st, err := d.Docker.Status(ctx, t.name)
		switch {
		case err != nil:
			return down("could not be checked")
		case st.Restarting:
			return down("was restarting")
		case !st.Running || st.State != "running":
			return down("was not running (" + textsafe.Line(st.State) + ")")
		case st.Health.Status == "unhealthy":
			return down("was unhealthy")
		case st.Health.Status != "starting":
			return restartCheck{up: true}
		}
		if time.Since(start)+d.healthPoll() > d.healthWait() {
			return restartCheck{up: true, note: "its health check was still starting " + d.healthWait().String() + " after the restart"}
		}
		if !sleep(ctx, d.healthPoll()) {
			return down("was not checked to the end: the change ran out of time;")
		}
	}
}

// logsSince returns the target's log lines written since start.
func (d *Deps) logsSince(ctx context.Context, t restartTarget, start time.Time) []string {
	type line struct{ at, text string }
	var lines []line
	if t.kind == "container" {
		l, err := d.Docker.Logs(ctx, t.name, restartLogLines, restartLogBytes)
		if err != nil {
			return []string{"its logs could not be read"}
		}
		for _, e := range l.Entries {
			lines = append(lines, line{e.Time, e.Text})
		}
	} else {
		l, err := d.Systemd.Logs(ctx, t.name, restartLogLines, restartLogBytes)
		if err != nil {
			return []string{"its logs could not be read"}
		}
		for _, e := range l.Entries {
			lines = append(lines, line{e.Time, e.Text})
		}
	}
	out := []string{}
	for _, l := range lines {
		at, err := time.Parse(time.RFC3339Nano, l.at)
		if err != nil || at.Before(start.Add(-time.Second)) {
			continue
		}
		out = append(out, "  "+at.UTC().Format("15:04:05")+" "+clip(textsafe.Line(l.text)))
	}
	if len(out) == 0 {
		return []string{"no log lines since the restart"}
	}
	return append([]string{"log lines since the restart:"}, out...)
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

func restartPlan(t *restartTarget, undoWait time.Duration) []string {
	if t == nil {
		return nil
	}
	return []string{"then " + t.verb() + " and check it; if it is not up, keep the change and file a request " +
		"to undo it, which runs by itself after " + undoWait.String() + " unless denied"}
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
	// isUndo marks Latch's own undo request. An undo whose target is still
	// not up files no further undo.
	isUndo bool
}

// undoChange is the undo request Latch files when the target of a change is
// not up afterwards. It runs by itself unless the owner denies it.
type undoChange struct {
	*fileChange
	wait time.Duration
}

func (u undoChange) Timeout() time.Duration { return u.wait }

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
	switch {
	case a.isUndo:
		verb = "undid the change to "
	case a.kind == "file_restore":
		verb = "restored " + a.backupID + " to "
	}
	sum := verb + a.file.Path + ", backup " + res.BackupID
	note, err := a.d.reloadFor(ctx, a.file.Path)
	sum += note
	if err != nil || a.restart == nil {
		return approval.Result{Summary: sum}, err
	}
	c := a.d.restartAndCheck(ctx, *a.restart)
	if c.up {
		sum += "; " + a.restart.done() + ", up"
		if c.note != "" {
			sum += ", but " + c.note
		}
		return approval.Result{Summary: sum, Details: c.logs}, nil
	}
	sum += "; " + c.why
	if a.isUndo {
		return approval.Result{Summary: sum + "; nothing more is undone automatically", Details: c.logs}, errStillDown
	}

	// Keep the change and let the owner choose: approve the undo, deny it
	// and fix forward, or let it run by itself.
	undo := a.undo(res)
	r, err := a.d.Actions.Submit(ctx, approval.Requester{}, undo)
	if err != nil {
		return approval.Result{Summary: sum + "; the change stays and no undo request could be filed (backup " + res.BackupID +
			" has the old content)", Details: c.logs}, errTargetDown
	}
	sum += fmt.Sprintf("; the change stays for now: request %d undoes it by itself at %s unless the owner denies it",
		r.ID, r.ExpiresAt.UTC().Format("15:04 UTC"))
	return approval.Result{Summary: sum, Details: c.logs}, errTargetDown
}

// undo builds the request that puts back what Execute replaced, but only
// over exactly what Execute wrote.
func (a *fileChange) undo(res files.ReplaceResult) undoChange {
	written := files.File{Path: a.file.Path, Hash: sha256.Sum256(a.content)}
	details := []string{"undo the change to " + a.file.Path + " (put back the content from backup " + res.BackupID + ")"}
	details = append(details, diffLines(a.file.Path, files.ViewLines(a.content), files.ViewLines(res.Old))...)
	if files.IsUnitPath(a.file.Path) {
		details = append(details, "then run systemd daemon-reload")
	}
	details = append(details, "then "+a.restart.verb()+" and check it",
		"Latch filed this because "+a.restart.label()+" was not up after the change; it runs by itself unless denied")
	return undoChange{
		fileChange: &fileChange{d: a.d, kind: "file_restore", path: a.path, file: written, content: res.Old,
			restart: a.restart, backupID: res.BackupID, details: details, isUndo: true},
		wait: a.d.undoWait(),
	}
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
			"afterwards" + afterRestartNote + alwaysNote + approvalNote,
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
		if t, err = d.withCompose(ctx, t, file.Path); err != nil {
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
		details = append(details, restartPlan(t, d.undoWait())...)
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
			"'restart' restarts that container or unit afterwards" + afterRestartNote + alwaysNote + approvalNote,
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
		if t, err = d.withCompose(ctx, t, file.Path); err != nil {
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
		details = append(details, restartPlan(t, d.undoWait())...)
		out, err := d.submit(ctx, &fileChange{d: d, kind: "file_restore", path: in.Path, file: file, content: content, restart: t, backupID: in.Backup, details: details})
		return out, in.Path, err
	}))
}
