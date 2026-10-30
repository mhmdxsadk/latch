package tgchat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mhmdxsadk/latch/internal/textsafe"
)

// Turn is one agent turn: a message for a session. DB is the session's own
// database file, or "" for the agent's shared default database.
type Turn struct {
	Session string
	DB      string
	Message string
	// Images are files attached to the message.
	Images []string
}

// Runner runs one agent turn and returns its final answer, reporting
// progress (when not nil) while it works.
type Runner interface {
	Run(ctx context.Context, t Turn, progress func(Progress)) (string, error)
}

// Account is the Linux account the agent runs as.
type Account struct {
	Name   string
	UID    uint32
	GID    uint32
	Groups []uint32
	Home   string
}

// LookupAccount finds name and its supplementary groups. The groups matter:
// Latch's agent socket checks the peer's groups, so the agent must keep its
// membership of the socket group.
func LookupAccount(name string) (Account, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return Account{}, err
	}
	a := Account{Name: u.Username, Home: u.HomeDir}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return Account{}, fmt.Errorf("user %s: bad uid %q", name, u.Uid)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return Account{}, fmt.Errorf("user %s: bad gid %q", name, u.Gid)
	}
	a.UID, a.GID = uint32(uid), uint32(gid)
	if a.UID == 0 {
		return Account{}, fmt.Errorf("user %s is root; the agent must run as a normal user", name)
	}
	gids, err := u.GroupIds()
	if err != nil {
		return Account{}, fmt.Errorf("user %s: groups: %w", name, err)
	}
	for _, g := range gids {
		n, err := strconv.ParseUint(g, 10, 32)
		if err != nil {
			return Account{}, fmt.Errorf("user %s: bad group id %q", name, g)
		}
		if n == 0 {
			return Account{}, fmt.Errorf("user %s is in group 0 (root); Latch refuses such agents", name)
		}
		a.Groups = append(a.Groups, uint32(n))
	}
	return a, nil
}

// Run runs a short command as the account, with a clean environment and its
// home as the working directory. The caller builds name and args; nothing
// from the agent or from Telegram reaches them.
func (a Account) Run(ctx context.Context, name string, args ...string) error {
	return a.run(ctx, nil, name, args...)
}

func (a Account) run(ctx context.Context, stdin io.Reader, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed tools and paths built by the bridge
	cmd.Stdin = stdin
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: a.UID, Gid: a.GID, Groups: a.Groups}}
	var stderr capped
	stderr.max, stderr.keepTail = 4<<10, true
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, lastLines(stderr.String(), 2))
	}
	return nil
}

// MakeDir creates dir (and parents) readable by the account only.
func (a Account) MakeDir(ctx context.Context, dir string) error {
	return a.Run(ctx, "/bin/mkdir", "-p", "-m", "700", "--", dir)
}

// WriteFile creates path as the account with data, readable by the account
// only. It fails when path already exists. The bridge cannot write in the
// agent's private home itself (its service has no CAP_DAC_OVERRIDE).
func (a Account) WriteFile(ctx context.Context, path string, data []byte) error {
	// noclobber (set -C) makes the redirection fail on an existing file.
	return a.run(ctx, bytes.NewReader(data), "/bin/sh", "-c", `set -C; umask 077; cat > "$1"`, "sh", path)
}

// ReplaceFile writes data to path as the account, readable by the account
// only, replacing a file that is already there. It writes a new file beside
// path and renames it over, so a reader never sees half a file.
func (a Account) ReplaceFile(ctx context.Context, path string, data []byte) error {
	return a.run(ctx, bytes.NewReader(data), "/bin/sh", "-c",
		`set -C; umask 077; tmp="$1.new.$$"; if cat > "$tmp" && mv -f -- "$tmp" "$1"; then exit 0; fi; rm -f -- "$tmp"; exit 1`, "sh", path)
}

// Remove deletes files as the account that owns them; missing ones are fine.
func (a Account) Remove(ctx context.Context, paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	return a.Run(ctx, "/bin/rm", append([]string{"-f", "--"}, paths...)...)
}

// ExecRunner runs Docker Agent:
//
//	docker-agent run CONFIG --exec --json --session ID [--session-db DB] [EXTRA...] -
//
// with the message on standard input, so it can never be read as a flag and
// never shows in the process list. The child runs as Account with a clean
// environment (nothing of the bridge's, such as the bot token, reaches it)
// in its own process group, which is killed when the run is stopped.
//
// --json makes Docker Agent print one event per line while it works. The
// runner reads the answer text of the team's entry agent as it is written
// and the names of the tools and agents it uses, for the live preview; tool
// output in the events is never passed on. The answer is the entry agent's
// last message, the same one --last prints.
type ExecRunner struct {
	Command   string
	Config    string
	ExtraArgs []string
	// Account is who the agent runs as; nil runs it as the bridge's own
	// user (tests only).
	Account *Account
	// MaxOutput bounds the answer that is kept, in bytes.
	MaxOutput int
}

// maxEventLine bounds one event line; longer ones (huge tool output) are
// skipped.
const maxEventLine = 16 << 20

// Run runs one turn, calling progress (when not nil) as the answer grows.
func (r *ExecRunner) Run(ctx context.Context, t Turn, progress func(Progress)) (string, error) {
	args := []string{"run", r.Config, "--exec", "--json", "--session", t.Session}
	if t.DB != "" {
		args = append(args, "--session-db", t.DB)
	}
	for _, img := range t.Images {
		args = append(args, "--attach", img)
	}
	args = append(args, r.ExtraArgs...)
	args = append(args, "-")
	// The command, config and extra args come from the owner's root-only
	// config; the agent's text only reaches stdin.
	cmd := exec.CommandContext(ctx, r.Command, args...) //nolint:gosec // owner-set argv, see above
	cmd.Stdin = strings.NewReader(t.Message)
	limit := r.MaxOutput
	if limit <= 0 {
		limit = 1 << 20
	}
	stderr := &capped{max: 16 << 10, keepTail: true}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	cmd.Env = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG=C.UTF-8",
		"TERM=dumb",
		"NO_COLOR=1",
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if a := r.Account; a != nil {
		cmd.Dir = a.Home
		cmd.Env = append(cmd.Env, "HOME="+a.Home, "USER="+a.Name, "LOGNAME="+a.Name)
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: a.UID, Gid: a.GID, Groups: a.Groups}
	}
	cmd.Cancel = func() error {
		// Kill the whole group: Docker Agent may have started helpers.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return "", err
	}
	ev := &events{limit: limit, progress: progress}
	readLines(stdout, ev.handle)
	err = cmd.Wait()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			msg := ev.errText
			if msg == "" {
				msg = lastLines(stderr.String(), 5)
			}
			return "", &RunError{Code: ee.ExitCode(), Stderr: msg}
		}
		return "", err
	}
	if ev.errText != "" && ev.answer.Len() == 0 {
		return "", &RunError{Code: 1, Stderr: ev.errText}
	}
	out := ev.answer.String()
	if ev.cut {
		out += "\n... (answer cut, too long)"
	}
	return out, nil
}

// Progress is the live state of a turn.
type Progress struct {
	// Text is the entry agent's answer so far (Markdown); empty until the
	// agent starts writing. Tool calls and helpers are not shown: what the
	// agent runs is noise on a phone.
	Text string
}

// events follows Docker Agent's event stream for one turn.
type events struct {
	limit    int
	progress func(Progress)

	root    string // the team's entry agent
	message string // id of the entry agent's current message
	answer  strings.Builder
	cut     bool
	errText string
}

type event struct {
	Type         string `json:"type"`
	AgentName    string `json:"agent_name"`
	CurrentAgent string `json:"current_agent"`
	Content      string `json:"content"`
	MessageID    string `json:"message_id"`
	Error        string `json:"error"`
	ToolCall     struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tool_call"`
}

func (e *events) handle(line []byte) {
	var ev event
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	switch ev.Type {
	case "team_info":
		if e.root == "" {
			e.root = ev.CurrentAgent
		}
	case "stream_started":
		if e.root == "" {
			e.root = ev.AgentName
		}
	case "agent_choice":
		if ev.AgentName != e.root {
			return // a helper agent's words are not the answer
		}
		if ev.MessageID != e.message {
			// A new message: the answer is the last one, like --last.
			e.message = ev.MessageID
			e.answer.Reset()
			e.cut = false
		}
		if e.answer.Len()+len(ev.Content) > e.limit {
			e.cut = true
			return
		}
		e.answer.WriteString(ev.Content)
		e.report()
	case "error":
		if e.errText == "" {
			e.errText = lastLines(textsafe.Head(ev.Error, 500), 3)
		}
	}
}

func (e *events) report() {
	if e.progress != nil {
		e.progress(Progress{Text: e.answer.String()})
	}
}

// readLines calls fn for each line of r, skipping lines over maxEventLine.
func readLines(r io.Reader, fn func([]byte)) {
	br := bufio.NewReaderSize(r, 64<<10)
	var line []byte
	skip := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !skip {
			if len(line)+len(chunk) > maxEventLine {
				skip, line = true, line[:0]
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if !skip && len(line) > 0 {
			fn(line)
		}
		line, skip = line[:0], false
		if err != nil {
			return
		}
	}
}

// RunError is an agent run that exited with an error.
type RunError struct {
	Code   int
	Stderr string // last lines, cleaned
}

func (e *RunError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("exit status %d", e.Code)
	}
	return fmt.Sprintf("exit status %d: %s", e.Code, e.Stderr)
}

// UsageLimit reports whether the agent's model refused the run because the
// account's usage limit is used up (HTTP 429), which clears on its own.
func (e *RunError) UsageLimit() bool {
	s := strings.ToLower(e.Stderr)
	return strings.Contains(s, "usage limit") || strings.Contains(s, "http 429")
}

// LoginExpired reports whether the model refused the run because its sign-in
// is gone or no longer valid (HTTP 401, or Docker Agent saying so on refresh).
func (e *RunError) LoginExpired() bool {
	s := strings.ToLower(e.Stderr)
	return strings.Contains(s, "http 401") || strings.Contains(s, "invalidated oauth token") ||
		strings.Contains(s, "session is no longer valid") || strings.Contains(s, "not signed in with a chatgpt account")
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, l := range lines {
		lines[i] = textsafe.Line(l)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// capped keeps the first (or, with keepTail, the last) max bytes written.
type capped struct {
	buf      bytes.Buffer
	max      int
	keepTail bool
	cut      bool
}

func (c *capped) Write(p []byte) (int, error) {
	n := len(p)
	if c.keepTail {
		c.buf.Write(p)
		if over := c.buf.Len() - c.max; over > 0 {
			b := c.buf.Bytes()[over:]
			c.buf = *bytes.NewBuffer(append([]byte(nil), b...))
			c.cut = true
		}
		return n, nil
	}
	if room := c.max - c.buf.Len(); room < len(p) {
		p = p[:max(room, 0)]
		c.cut = true
	}
	c.buf.Write(p)
	return n, nil
}

func (c *capped) String() string { return c.buf.String() }
