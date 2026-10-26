package tgchat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhmdxsadk/latch/internal/tgapi"
	"github.com/mhmdxsadk/latch/internal/tgapi/tgtest"
)

const owner = 4242

type call struct{ session, message string }

// fakeRunner answers with reply(message), or blocks until ctx ends when
// block is set. steps are reported as progress first.
type fakeRunner struct {
	mu    sync.Mutex
	calls []call
	reply func(string) (string, error)
	block bool
	steps []Progress
}

func (f *fakeRunner) Run(ctx context.Context, session, message string, progress func(Progress)) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{session, message})
	block, reply, steps := f.block, f.reply, f.steps
	f.mu.Unlock()
	for _, p := range steps {
		progress(p)
		time.Sleep(draftEvery + 100*time.Millisecond)
	}
	if block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if reply == nil {
		return "answer to " + message, nil
	}
	return reply(message)
}

func (f *fakeRunner) Calls() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

type harness struct {
	t     *testing.T
	tg    *tgtest.Server
	run   *fakeRunner
	chat  *Chat
	index string
}

func newHarness(t *testing.T, timeout time.Duration) *harness {
	t.Helper()
	h := &harness{t: t, tg: tgtest.New(t), run: &fakeRunner{}, index: filepath.Join(t.TempDir(), "sessions.json")}
	h.chat = h.newChat(timeout)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go h.chat.Run(ctx)
	return h
}

func (h *harness) newChat(timeout time.Duration) *Chat {
	h.t.Helper()
	c, err := New(Options{Bot: h.tg.Client(), Chat: owner, Runner: h.run, IndexPath: h.index, Name: "keeper", Timeout: timeout})
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *harness) send(text string) { h.chat.HandleText(context.Background(), text, 0) }

// waitText waits for a sent message containing want and returns it.
func (h *harness) waitText(want string) tgtest.Sent {
	h.t.Helper()
	var got tgtest.Sent
	tgtest.WaitFor(h.t, "a message with "+want, func() bool {
		for _, m := range h.tg.Sent() {
			if strings.Contains(m.Text, want) {
				got = m
				return true
			}
		}
		return false
	})
	return got
}

func TestMessagesRunInTheActiveSession(t *testing.T) {
	h := newHarness(t, time.Minute)
	h.send("is caddy healthy?")
	m := h.waitText("answer to is caddy healthy?")
	if m.Text != "answer to is caddy healthy?" {
		t.Errorf("answer %q", m.Text)
	}
	h.send("and jellyfin?")
	h.waitText("answer to and jellyfin?")
	calls := h.run.Calls()
	if len(calls) != 2 || calls[0].session != calls[1].session || len(calls[0].session) != 36 {
		t.Fatalf("calls %+v", calls)
	}

	h.send("/new disk check")
	h.waitText("New conversation: disk check")
	h.send("how full is /srv?")
	h.waitText("answer to how full is /srv?")
	if c := h.run.Calls(); c[2].session == c[0].session {
		t.Fatal("/new kept the old session")
	}

	h.send("/sessions")
	list := h.waitText("Pick a conversation")
	if len(list.Keyboard) != 2 || !strings.HasPrefix(list.Keyboard[0][0].Text, "* disk check") || !strings.HasPrefix(list.Keyboard[1][0].Text, "is caddy healthy?") {
		t.Fatalf("keyboard %+v", list.Keyboard)
	}
	cb := tgapi.CallbackQuery{ID: "cb1", Data: list.Keyboard[1][0].Data, Message: &tgapi.Message{MessageID: list.ID}}
	if !h.chat.HandlePress(context.Background(), cb) {
		t.Fatal("press not handled")
	}
	h.send("back again")
	h.waitText("answer to back again")
	if c := h.run.Calls(); c[3].session != c[0].session {
		t.Fatal("switch did not take")
	}

	h.send("/rename caddy and jellyfin")
	h.waitText("Renamed to: caddy and jellyfin")
	// The index survives a bridge restart.
	c := h.newChat(time.Minute)
	if c.index.Active != h.run.Calls()[0].session || len(c.index.Sessions) != 2 || c.index.Sessions[0].Title != "caddy and jellyfin" {
		t.Fatalf("index %+v", c.index)
	}
	if st, err := os.Stat(h.index); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("index file: %v", err)
	}
}

func TestStop(t *testing.T) {
	h := newHarness(t, time.Minute)
	h.run.block = true
	h.send("long job")
	tgtest.WaitFor(t, "the run to start", func() bool { return len(h.run.Calls()) == 1 })
	h.send("queued one")
	tgtest.WaitFor(t, "the message to queue", func() bool { return len(h.chat.queue) == 1 })
	h.send("/stop")
	h.waitText("Stopping keeper (dropped 1 waiting)")
	h.waitText("Stopped.")
	h.send("/stop")
	h.waitText("Nothing running")
	if n := len(h.run.Calls()); n != 1 {
		t.Errorf("the dropped message ran: %d calls", n)
	}
}

func TestTimeoutAndFailure(t *testing.T) {
	h := newHarness(t, 50*time.Millisecond)
	h.run.block = true
	h.send("slow")
	h.waitText("did not finish within 50ms")
	h.run.mu.Lock()
	h.run.block = false
	h.run.reply = func(string) (string, error) { return "", &RunError{Code: 2, Stderr: "no model"} }
	h.run.mu.Unlock()
	h.send("again")
	h.waitText("keeper failed (exit status 2: no model).")
	h.run.mu.Lock()
	h.run.reply = func(string) (string, error) { return "", errors.New("fork/exec: secret detail") }
	h.run.mu.Unlock()
	h.send("third")
	m := h.waitText("Could not start keeper")
	if strings.Contains(m.Text, "secret detail") {
		t.Error("internal error text shown")
	}
}

func TestAnswersAreCleanedAndSplit(t *testing.T) {
	h := newHarness(t, time.Minute)
	long := strings.Repeat("a line of the answer\n", 400) // over 8000 characters
	h.run.reply = func(string) (string, error) { return "\x1b[31mred\x1b[0m \u202eevil\n" + long, nil }
	h.send("hi")
	tgtest.WaitFor(t, "three parts", func() bool { return len(h.tg.Sent()) >= 3 })
	sent := h.tg.Sent()
	if !strings.HasPrefix(sent[0].Text, "red evil\n") {
		t.Errorf("not cleaned: %q", sent[0].Text[:30])
	}
	for _, m := range sent {
		if tgapi.TextLen(m.Text) > tgapi.MaxText || tgtest.CheckEntities(m.Text, m.Entities) != "" {
			t.Errorf("bad part: %d %q", tgapi.TextLen(m.Text), m.Text[:20])
		}
	}
}

func TestNotify(t *testing.T) {
	h := newHarness(t, time.Minute)
	h.chat.Notify("ignored: no conversation yet", "note", 0)
	h.send("restart caddy please")
	h.waitText("answer to restart caddy please")
	h.chat.Notify("Latch: request 3 was approved and finished: done.", "Sent keeper the result of request 3.", 77)
	h.waitText("Sent keeper the result of request 3.")
	if m := h.waitText("answer to Latch: request 3"); m.ReplyTo != 77 {
		t.Errorf("follow-up does not reply to the request message: %+v", m)
	}
	c := h.run.Calls()
	if len(c) != 2 || c[1].session != c[0].session {
		t.Fatalf("calls %+v", c)
	}
}

func TestCommandParsing(t *testing.T) {
	for _, tc := range []struct{ in, cmd, arg string }{
		{"/new", "new", ""},
		{"/new  my  name ", "new", "my  name"},
		{"/Sessions@latch_bot", "sessions", ""},
	} {
		cmd, arg, ok := command(tc.in)
		if !ok || cmd != tc.cmd || arg != tc.arg {
			t.Errorf("%q: %q %q %v", tc.in, cmd, arg, ok)
		}
	}
	if _, _, ok := command("hello /new"); ok {
		t.Error("text taken as command")
	}
	if got := title("  first\nline  of a much longer message that goes on and on "); got != "first line of a much longer message that..." {
		t.Errorf("title %q", got)
	}
}

func TestExecRunner(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "docker-agent")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A stand-in that prints Docker Agent's events: the entry agent thinks,
	// calls a tool, asks the researcher, then writes its final message.
	write(`cat > "$0.stdin"
printf '%s\n' "$*" > "$0.args"
env > "$0.env"
cat <<'END'
{"type":"team_info","agent_name":"inspector","current_agent":"inspector"}
{"type":"stream_started","agent_name":"inspector"}
{"type":"agent_choice","agent_name":"inspector","content":"Let me check.","message_id":"m1"}
{"type":"tool_call","agent_name":"inspector","tool_call":{"function":{"name":"docker_status","arguments":"{}"}}}
{"type":"tool_call_response","agent_name":"inspector","response":"SECRET TOOL OUTPUT"}
{"type":"agent_choice","agent_name":"researcher","content":"docs say","message_id":"r1"}
not json at all
{"type":"agent_choice","agent_name":"inspector","content":"All ","message_id":"m2"}
{"type":"agent_choice","agent_name":"inspector","content":"**good**.","message_id":"m2"}
{"type":"stream_stopped","agent_name":"inspector"}
END
`)
	t.Setenv("BOT_TOKEN", "must-not-leak")
	r := &ExecRunner{Command: script, Config: "/home/keeper/keeper.yaml", ExtraArgs: []string{"--safety", "restricted"}}
	var steps []Progress
	out, err := r.Run(context.Background(), "sess-1", "--yolo is just text", func(p Progress) { steps = append(steps, p) })
	if err != nil {
		t.Fatal(err)
	}
	if out != "All **good**." {
		t.Fatalf("answer %q", out)
	}
	read := func(suffix string) string {
		b, _ := os.ReadFile(script + suffix)
		return string(b)
	}
	if got := read(".args"); got != "run /home/keeper/keeper.yaml --exec --json --session sess-1 --safety restricted -\n" {
		t.Errorf("args %q", got)
	}
	if read(".stdin") != "--yolo is just text" || strings.Contains(read(".env"), "must-not-leak") {
		t.Errorf("stdin or env wrong: %q", read(".stdin"))
	}
	var seen []string
	for _, p := range steps {
		if strings.Contains(p.Text+p.Status, "SECRET") {
			t.Fatalf("tool output reached the preview: %+v", p)
		}
		seen = append(seen, p.Status+"|"+p.Text)
	}
	want := "|Let me check.,Checking docker_status|,Asking researcher|,|All ,|All **good**."
	if got := strings.Join(seen, ","); got != want {
		t.Errorf("progress\n got  %s\n want %s", got, want)
	}

	write("echo '{\"type\":\"error\",\"error\":\"model refused: quota\"}'\necho 'bad config' >&2\nexit 3\n")
	_, err = r.Run(context.Background(), "s", "m", nil)
	var re *RunError
	if !errors.As(err, &re) || re.Code != 3 || re.Stderr != "model refused: quota" {
		t.Fatalf("err = %v", err)
	}
	write("echo 'bad config' >&2\nexit 3\n")
	if _, err = r.Run(context.Background(), "s", "m", nil); !errors.As(err, &re) || re.Stderr != "bad config" {
		t.Fatalf("err = %v", err)
	}

	// Stopping kills the whole process group, children included.
	write("sleep 30 &\nsleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := r.Run(ctx, "s", "m", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("stop did not kill the group quickly")
	}
}

func TestLiveDraftsAndReply(t *testing.T) {
	h := newHarness(t, time.Minute)
	h.run.steps = []Progress{{Status: "Checking docker_status"}, {Text: "Caddy is **up**"}}
	id := h.tg.MessageID(owner, "is caddy up?")
	h.chat.HandleText(context.Background(), "is caddy up?", id)
	m := h.waitText("answer to is caddy up?")
	if m.ReplyTo != id {
		t.Errorf("answer is not a reply: %+v", m)
	}
	var texts []string
	for _, d := range h.tg.Drafts() {
		if !d.CanStop {
			t.Errorf("draft without Stop: %+v", d)
		}
		if msg := tgtest.CheckEntities(d.Text, d.Entities); msg != "" {
			t.Errorf("draft %q: %s", d.Text, msg)
		}
		texts = append(texts, d.Text)
	}
	got := strings.Join(texts, "|")
	if !strings.HasPrefix(got, "Checking docker_status...|") || !strings.Contains(got, "|Caddy is up") {
		t.Errorf("drafts %q", got)
	}
}

func TestStopButton(t *testing.T) {
	h := newHarness(t, time.Minute)
	h.run.steps = []Progress{{Status: "Checking docker_logs"}}
	h.run.block = true
	h.send("long job")
	tgtest.WaitFor(t, "a draft", func() bool { return len(h.tg.Drafts()) > 0 })
	d := h.tg.Drafts()[0]
	if h.chat.StopDraft(d.ID + 1) {
		t.Fatal("stopped by another draft id")
	}
	if !h.chat.StopDraft(d.ID) {
		t.Fatal("not stopped")
	}
	h.waitText("Stopped.")
}

func TestCapped(t *testing.T) {
	c := &capped{max: 5}
	c.Write([]byte("abc"))
	c.Write([]byte("defgh"))
	if c.String() != "abcde" || !c.cut {
		t.Errorf("%q %v", c.String(), c.cut)
	}
	tail := &capped{max: 5, keepTail: true}
	tail.Write([]byte("abc"))
	tail.Write([]byte("defgh"))
	if tail.String() != "defgh" {
		t.Errorf("%q", tail.String())
	}
}

func TestQuickAnswerHasNoDraft(t *testing.T) {
	h := newHarness(t, time.Minute)
	h.send("hello")
	h.waitText("answer to hello")
	if d := h.tg.Drafts(); len(d) != 0 {
		t.Errorf("drafts for a quick answer: %+v", d)
	}
	if h.tg.Typing() == 0 {
		t.Error("no typing indicator")
	}
}

func TestWelcome(t *testing.T) {
	h := newHarness(t, time.Minute)
	h.chat.o.Host = "lisbon"
	h.send("/start")
	m := h.waitText("Ask me anything about lisbon")
	if len(m.Entities) != 0 || !strings.Contains(m.Text, "/new starts over") {
		t.Errorf("%q %+v", m.Text, m.Entities)
	}
}

func TestChecks(t *testing.T) {
	h := newHarness(t, time.Minute)
	checks := &fakeRunner{reply: func(string) (string, error) { return "ALL_CLEAR", nil }}
	h.chat.o.CheckRunner = checks

	h.send("/check")
	h.waitText("All clear.")
	if n := len(h.chat.index.Sessions); n != 0 {
		t.Fatalf("an all-clear check left a conversation: %d", n)
	}

	checks.mu.Lock()
	checks.reply = func(string) (string, error) {
		return "**Jellyfin keeps restarting.** Its health check fails since 09:12.\n\nI would restart it if you ask.", nil
	}
	checks.mu.Unlock()
	h.chat.queue <- job{check: true} // a scheduled one: silent unless it finds something
	h.waitText("Jellyfin keeps restarting.")
	h.chat.mu.Lock()
	sessions, active, reported := h.chat.index.Sessions, h.chat.index.Active, h.chat.index.Reported
	h.chat.mu.Unlock()
	if len(sessions) != 1 || active != sessions[0].ID || !strings.HasPrefix(sessions[0].Title, "Check: Jellyfin keeps restarting.") {
		t.Fatalf("sessions %+v active %s", sessions, active)
	}
	if len(reported) != 1 || reported[0].Headline != "Jellyfin keeps restarting. Its health check fails since 09:12." {
		t.Errorf("reported %+v", reported)
	}
	// The owner's reply continues the check's conversation.
	h.send("fix it")
	h.waitText("answer to fix it")
	if c := h.run.Calls(); c[len(c)-1].session != sessions[0].ID {
		t.Errorf("reply went to %s, not the check's conversation", c[len(c)-1].session)
	}
	// The next check is told what was already reported.
	h.send("/check")
	tgtest.WaitFor(t, "a third check", func() bool { return len(checks.Calls()) == 3 })
	last := checks.Calls()[2]
	if !strings.Contains(last.message, "Already reported") || !strings.Contains(last.message, "Jellyfin keeps restarting") ||
		!strings.Contains(last.message, "Do not file") {
		t.Errorf("prompt:\n%s", last.message)
	}
}
