package tgchat

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhmdxsadk/latch/internal/tgapi/tgtest"
)

// clock is a time the test moves by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newResetHarness(t *testing.T, after time.Duration) (*convHarness, *clock) {
	t.Helper()
	clk := &clock{t: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	files := &store{}
	h := newHarnessWith(t, time.Minute, func(o *Options) {
		o.SessionDir, o.Remove = sessionDir, files.remove
		o.Now, o.ResetAfter = clk.Now, after
	})
	return &convHarness{harness: h, files: files}, clk
}

func (h *convHarness) reset() { h.chat.maybeReset(context.Background()) }

func TestIdleConversationEndsAfterTheSetTime(t *testing.T) {
	h, clk := newResetHarness(t, 24*time.Hour)
	id1 := h.ask("is caddy up?")
	id2 := h.ask("and jellyfin?")
	first := h.chat.conversation()

	clk.advance(23 * time.Hour)
	h.reset()
	if h.tg.Deleted(id1) || h.chat.conversation() != first {
		t.Fatal("cleared before the time was up")
	}

	clk.advance(2 * time.Hour)
	h.reset()
	tgtest.WaitFor(t, "the closing note", func() bool {
		v := h.visibleTexts()
		return len(v) == 1 && v[0] == "Conversation ended after 24 hours."
	})
	for _, id := range []int64{id1, id2} {
		if !h.tg.Deleted(id) {
			t.Errorf("message %d is still on screen", id)
		}
	}
	if got := h.files.removedDBs(); len(got) != 1 || got[0] != first+".db" {
		t.Errorf("removed databases %v, want %s.db", got, first)
	}
	vis := h.tg.Visible()
	if len(vis) != 1 || !vis[0].Silent {
		t.Errorf("the closing note must not buzz the phone: %+v", vis)
	}
	if h.chat.conversation() != "" {
		t.Error("the conversation was not ended")
	}

	// The note stays so the chat is never empty, and nothing more happens
	// while the owner is away.
	for range 3 {
		clk.advance(48 * time.Hour)
		h.reset()
	}
	if v := h.visibleTexts(); len(v) != 1 {
		t.Errorf("repeated resets must not pile up notes: %q", v)
	}

	// The next message starts a new conversation.
	h.ask("disk space?")
	if h.chat.conversation() == first {
		t.Error("the new message continued the ended conversation")
	}
}

func TestUseKeepsTheConversationAlive(t *testing.T) {
	h, clk := newResetHarness(t, 24*time.Hour)
	h.ask("first")
	clk.advance(20 * time.Hour)
	id := h.ask("second")
	clk.advance(10 * time.Hour) // 30 hours after the first message, 10 after the last
	h.reset()
	if h.tg.Deleted(id) {
		t.Fatal("cleared a conversation that was used 10 hours ago")
	}
	clk.advance(15 * time.Hour)
	h.reset()
	tgtest.WaitFor(t, "the reset", func() bool { return h.tg.Deleted(id) })
}

func TestKeepPostponesTheReset(t *testing.T) {
	h, clk := newResetHarness(t, 24*time.Hour)
	id := h.ask("keep this one")

	clk.advance(10 * time.Hour)
	h.say("/keep")
	m := h.waitText("Keeping this conversation until")
	if !strings.HasSuffix(m.Text, ". Send /keep again to extend.") {
		t.Errorf("text %q", m.Text)
	}
	var timed bool
	for _, e := range m.Entities {
		if e.Type == "date_time" && e.UnixTime == clk.Now().Add(24*time.Hour).Unix() {
			timed = true
		}
	}
	if !timed {
		t.Errorf("the end time is not a time entity for 24 hours from now: %+v", m.Entities)
	}

	clk.advance(20 * time.Hour) // hour 30: past 24 hours idle, inside the keep (until hour 34)
	h.reset()
	if h.tg.Deleted(id) {
		t.Fatal("cleared while kept")
	}
	h.say("/keep")              // 30 hours in: kept until hour 54
	clk.advance(20 * time.Hour) // hour 50
	h.reset()
	if h.tg.Deleted(id) {
		t.Fatal("cleared after /keep was repeated")
	}
	clk.advance(5 * time.Hour) // hour 55
	h.reset()
	tgtest.WaitFor(t, "the reset after the keep ran out", func() bool { return h.tg.Deleted(id) })
}

func TestNothingEndsWhileAnAnswerIsRunning(t *testing.T) {
	h, clk := newResetHarness(t, time.Hour)
	h.run.gate = make(chan struct{})
	id := h.say("slow question")
	tgtest.WaitFor(t, "the run to start", func() bool { return len(h.run.Calls()) == 1 })
	clk.advance(3 * time.Hour)
	h.reset()
	if h.tg.Deleted(id) || h.chat.conversation() == "" {
		t.Fatal("ended a conversation in the middle of an answer")
	}
	close(h.run.gate)
}

func TestResetIsOffUnlessSet(t *testing.T) {
	h, clk := newResetHarness(t, 0)
	id := h.ask("hello")
	clk.advance(1000 * time.Hour)
	h.reset()
	if h.tg.Deleted(id) {
		t.Fatal("cleared with reset_after unset")
	}
	h.say("/keep")
	h.waitText("not cleared automatically")
}

func TestHours(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Hour: "1 hour", 24 * time.Hour: "24 hours", 90 * time.Minute: "90 minutes",
	} {
		if got := hours(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}
