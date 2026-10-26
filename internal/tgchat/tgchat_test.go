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
// block is set.
type fakeRunner struct {
	mu    sync.Mutex
	calls []call
	reply func(string) (string, error)
	block bool
}

func (f *fakeRunner) Run(ctx context.Context, session, message string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{session, message})
	block, reply := f.block, f.reply
	f.mu.Unlock()
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

func (h *harness) send(text string) { h.chat.HandleText(context.Background(), text) }

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
	if !strings.HasPrefix(m.Text, "keeper answer") || len(m.Entities) == 0 || m.Entities[0].Type != "bold" || m.Formatted().Slice(m.Entities[0]) != "keeper" {
		t.Errorf("no bold agent label: %q %+v", m.Text, m.Entities)
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
	h.waitText("Queued: keeper is still answering")
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
	if !strings.HasPrefix(sent[0].Text, "keeper red evil\n") {
		t.Errorf("not cleaned: %q", sent[0].Text[:30])
	}
	for _, m := range sent {
		if !strings.HasPrefix(m.Text, "keeper ") || tgapi.TextLen(m.Text) > tgapi.MaxText || tgtest.CheckEntities(m.Text, m.Entities) != "" {
			t.Errorf("bad part: %d %q", tgapi.TextLen(m.Text), m.Text[:20])
		}
	}
}

func TestNotify(t *testing.T) {
	h := newHarness(t, time.Minute)
	h.chat.Notify("ignored: no conversation yet", "note")
	h.send("restart caddy please")
	h.waitText("answer to restart caddy please")
	h.chat.Notify("Latch: request 3 was approved and finished: done.", "Sent keeper the result of request 3.")
	h.waitText("Sent keeper the result of request 3.")
	h.waitText("answer to Latch: request 3")
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
	err := os.WriteFile(script, []byte(`#!/bin/sh
echo "args: $*"
echo "stdin: $(cat)"
echo "token: ${BOT_TOKEN:-none}"
`), 0o700)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOT_TOKEN", "must-not-leak")
	r := &ExecRunner{Command: script, Config: "/home/keeper/keeper.yaml", ExtraArgs: []string{"--safety", "restricted"}}
	out, err := r.Run(context.Background(), "sess-1", "--yolo is just text")
	if err != nil {
		t.Fatal(err)
	}
	want := "args: run /home/keeper/keeper.yaml --exec --last --session sess-1 --safety restricted -\nstdin: --yolo is just text\ntoken: none\n"
	if out != want {
		t.Fatalf("got %q\nwant %q", out, want)
	}

	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'bad config' >&2\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = r.Run(context.Background(), "s", "m")
	var re *RunError
	if !errors.As(err, &re) || re.Code != 3 || re.Stderr != "bad config" {
		t.Fatalf("err = %v", err)
	}

	// Stopping kills the whole process group, children included.
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30 &\nsleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := r.Run(ctx, "s", "m"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("stop did not kill the group quickly")
	}
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
