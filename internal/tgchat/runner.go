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
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mhmdxsadk/latch/internal/textsafe"
)

// Runner runs one agent turn in a session and returns its final answer,
// reporting progress (when not nil) while it works.
type Runner interface {
	Run(ctx context.Context, session, message string, progress func(Progress)) (string, error)
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

// ExecRunner runs Docker Agent:
//
//	docker-agent run CONFIG --exec --json --session ID [EXTRA...] -
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
func (r *ExecRunner) Run(ctx context.Context, session, message string, progress func(Progress)) (string, error) {
	args := append([]string{"run", r.Config, "--exec", "--json", "--session", session}, r.ExtraArgs...)
	args = append(args, "-")
	// The command, config and extra args come from the owner's root-only
	// config; the agent's text only reaches stdin.
	cmd := exec.CommandContext(ctx, r.Command, args...) //nolint:gosec // owner-set argv, see above
	cmd.Stdin = strings.NewReader(message)
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
	// Text is the entry agent's answer so far (Markdown), empty while it
	// works with tools.
	Text string
	// Status says what it is doing, like "Checking docker_status".
	Status string
}

// events follows Docker Agent's event stream for one turn.
type events struct {
	limit    int
	progress func(Progress)

	root    string // the team's entry agent
	message string // id of the entry agent's current message
	answer  strings.Builder
	cut     bool
	status  string
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

var toolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

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
			e.setStatus("Asking " + cleanName(ev.AgentName))
			return
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
		e.status = ""
		e.report()
	case "tool_call":
		name := ev.ToolCall.Function.Name
		if !toolName.MatchString(name) {
			name = "a tool"
		}
		who := ""
		if ev.AgentName != "" && ev.AgentName != e.root {
			who = cleanName(ev.AgentName) + ": "
		}
		e.setStatus(who + "Checking " + name)
	case "error":
		if e.errText == "" {
			e.errText = lastLines(textsafe.Head(ev.Error, 500), 3)
		}
	}
}

func (e *events) setStatus(s string) {
	if s == e.status {
		return
	}
	e.status = s
	// Work after some text started (more tool calls before the final
	// message) shows the status again.
	e.answer.Reset()
	e.report()
}

func (e *events) report() {
	if e.progress != nil {
		e.progress(Progress{Text: e.answer.String(), Status: e.status})
	}
}

func cleanName(s string) string {
	if toolName.MatchString(s) {
		return s
	}
	return "another agent"
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
