package tgchat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mhmdxsadk/latch/internal/textsafe"
)

// Runner runs one agent turn in a session and returns its final answer.
type Runner interface {
	Run(ctx context.Context, session, message string) (string, error)
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
//	docker-agent run CONFIG --exec --last --session ID [EXTRA...] -
//
// with the message on standard input, so it can never be read as a flag and
// never shows in the process list. The child runs as Account with a clean
// environment (nothing of the bridge's, such as the bot token, reaches it)
// in its own process group, which is killed when the run is stopped.
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

// Run runs one turn.
func (r *ExecRunner) Run(ctx context.Context, session, message string) (string, error) {
	args := append([]string{"run", r.Config, "--exec", "--last", "--session", session}, r.ExtraArgs...)
	args = append(args, "-")
	// The command, config and extra args come from the owner's root-only
	// config; the agent's text only reaches stdin.
	cmd := exec.CommandContext(ctx, r.Command, args...) //nolint:gosec // owner-set argv, see above
	cmd.Stdin = strings.NewReader(message)
	limit := r.MaxOutput
	if limit <= 0 {
		limit = 1 << 20
	}
	stdout := &capped{max: limit}
	stderr := &capped{max: 16 << 10, keepTail: true}
	cmd.Stdout, cmd.Stderr = stdout, stderr
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
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", &RunError{Code: ee.ExitCode(), Stderr: lastLines(stderr.String(), 5)}
		}
		return "", err
	}
	out := stdout.String()
	if stdout.cut {
		out += "\n... (answer cut, too long)"
	}
	return out, nil
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
