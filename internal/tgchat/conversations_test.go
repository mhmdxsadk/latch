package tgchat

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhmdxsadk/latch/internal/tgapi"
	"github.com/mhmdxsadk/latch/internal/tgapi/tgtest"
)

const sessionDir = "/data/latch-sessions"

// store is a fake agent home: it records the files the chat asked to delete.
type store struct {
	mu      sync.Mutex
	removed []string
}

func (s *store) remove(_ context.Context, paths []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removed = append(s.removed, paths...)
	return nil
}

func (s *store) removedDBs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var dbs []string
	for _, p := range s.removed {
		if strings.HasSuffix(p, ".db") {
			dbs = append(dbs, filepath.Base(p))
		}
	}
	return dbs
}

// convHarness is a chat with per-conversation databases, transcripts and a
// clock that moves a minute on every reading.
type convHarness struct {
	*harness
	files *store
	hist  string
}

func newConvHarness(t *testing.T, keep int) *convHarness {
	t.Helper()
	files := &store{}
	hist := t.TempDir()
	var mu sync.Mutex
	clock := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	h := newHarnessWith(t, time.Minute, func(o *Options) {
		o.SessionDir, o.HistoryDir, o.Keep, o.Remove = sessionDir, hist, keep, files.remove
		o.Now = func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			clock = clock.Add(time.Minute)
			return clock
		}
	})
	return &convHarness{harness: h, files: files, hist: hist}
}

// say sends a message from the owner with a message id, like Telegram does,
// and returns the id.
func (h *convHarness) say(text string) int64 {
	id := h.tg.MessageID(owner, text)
	h.chat.HandleText(context.Background(), text, id)
	return id
}

func (h *convHarness) ask(text string) int64 {
	id := h.say(text)
	h.waitText("answer to " + text)
	// The answer is on screen; wait until the chat has noted it too.
	tgtest.WaitFor(h.t, "the answer to be noted", func() bool {
		var n int
		for _, s := range h.sessions() {
			n += len(s.Messages)
		}
		n += len(h.chat.loose())
		return n >= 2 && !h.chat.busy()
	})
	return id
}

func (h *convHarness) sessions() []session {
	h.chat.mu.Lock()
	defer h.chat.mu.Unlock()
	return append([]session(nil), h.chat.index.Sessions...)
}

func (h *convHarness) visibleTexts() []string {
	var out []string
	for _, m := range h.tg.Visible() {
		out = append(out, m.Text)
	}
	return out
}

func TestNewClearsTheScreenAndResumeShowsItAgain(t *testing.T) {
	h := newConvHarness(t, 5)
	id1 := h.ask("is caddy up?")
	id2 := h.ask("and jellyfin?")
	tgtest.WaitFor(t, "both answers to be noted", func() bool { return len(h.sessions()[0].Messages) >= 4 })
	first := h.sessions()[0]
	if first.Title != "is caddy up?" || !first.Own || first.Started.IsZero() || len(first.Messages) < 4 {
		t.Fatalf("first conversation %+v", first)
	}
	if c := h.run.Calls(); c[0].db != filepath.Join(sessionDir, first.ID+".db") {
		t.Fatalf("turn used database %q", c[0].db)
	}

	cmd := h.say("/new")
	h.waitText("New conversation.")
	for _, id := range []int64{id1, id2, cmd} {
		if !h.tg.Deleted(id) {
			t.Errorf("the owner's message %d is still on screen", id)
		}
	}
	if got := h.visibleTexts(); len(got) != 1 || got[0] != "New conversation." {
		t.Fatalf("screen after /new: %q", got)
	}

	h.ask("disk space?")
	h.say("/sessions")
	list := h.waitText("Pick a conversation")
	if len(list.Keyboard) != 2 || !strings.HasPrefix(list.Keyboard[0][0].Text, "* disk space?") || !strings.HasPrefix(list.Keyboard[1][0].Text, "is caddy up?") {
		t.Fatalf("list %+v", list.Keyboard)
	}
	cb := tgapi.CallbackQuery{ID: "cb1", Data: list.Keyboard[1][0].Data, Message: &tgapi.Message{MessageID: list.ID}}
	if !h.chat.HandlePress(context.Background(), cb) {
		t.Fatal("not handled")
	}
	tgtest.WaitFor(t, "the resume note", func() bool {
		v := h.visibleTexts()
		return len(v) > 0 && strings.HasPrefix(v[len(v)-1], "Back in: is caddy up?")
	})
	// The screen shows the first conversation again, nothing of the second.
	var shown strings.Builder
	for _, m := range h.tg.Visible() {
		shown.WriteString(m.Text + "\n")
	}
	for _, want := range []string{"is caddy up?", "answer to is caddy up?", "and jellyfin?", "answer to and jellyfin?"} {
		if !strings.Contains(shown.String(), want) {
			t.Errorf("history lacks %q:\n%s", want, shown.String())
		}
	}
	for _, gone := range []string{"disk space?", "answer to disk space?", "Pick a conversation"} {
		if strings.Contains(shown.String(), gone) {
			t.Errorf("the other conversation still shows %q", gone)
		}
	}
	// The owner's own messages come back as quotes, the answers as they were.
	var quotes []string
	for _, m := range h.tg.Visible() {
		f := m.Formatted()
		for _, e := range f.Entities {
			if e.Type == "blockquote" || e.Type == "expandable_blockquote" {
				quotes = append(quotes, f.Slice(e))
			}
		}
	}
	if len(quotes) < 2 {
		t.Errorf("the owner's messages are not quotes: %q", quotes)
	}
	// The conversation continues where it was: same session, same database.
	h.ask("any news?")
	last := h.run.Calls()[len(h.run.Calls())-1]
	if last.session != first.ID || last.db != filepath.Join(sessionDir, first.ID+".db") {
		t.Errorf("resumed turn ran in %+v, want session %s", last, first.ID)
	}
}

func TestOnlyTheRecentConversationsAreKept(t *testing.T) {
	h := newConvHarness(t, 2)
	var ids []string
	for _, q := range []string{"one", "two", "three", "four"} {
		h.ask(q)
		ids = append(ids, h.sessions()[len(h.sessions())-1].ID)
		if q != "four" {
			h.say("/new")
			h.waitText("New conversation.")
		}
	}
	got := h.sessions()
	if len(got) != 2 || got[0].Title != "three" || got[1].Title != "four" {
		t.Fatalf("kept %+v", got)
	}
	removed := h.files.removedDBs()
	if len(removed) != 2 || removed[0] != ids[0]+".db" || removed[1] != ids[1]+".db" {
		t.Errorf("removed databases %v, want conversations one and two (%v)", removed, ids[:2])
	}
	for _, p := range h.files.removed {
		if !strings.HasPrefix(p, sessionDir+"/") {
			t.Errorf("deleted outside the session folder: %s", p)
		}
	}
	for _, id := range ids[:2] {
		if _, err := os.Stat(filepath.Join(h.hist, id+".json")); !os.IsNotExist(err) {
			t.Errorf("transcript of %s was kept", id)
		}
	}
	for _, id := range ids[2:] {
		if _, err := os.Stat(filepath.Join(h.hist, id+".json")); err != nil {
			t.Errorf("transcript of %s is gone: %v", id, err)
		}
	}
	// Switching uses the transcripts: a deleted one is gone from the list.
	h.say("/sessions")
	list := h.waitText("Pick a conversation")
	if len(list.Keyboard) != 2 {
		t.Errorf("list %+v", list.Keyboard)
	}
}

func TestNewReusesAnUnusedConversation(t *testing.T) {
	h := newConvHarness(t, 5)
	for range 4 {
		h.say("/new")
	}
	tgtest.WaitFor(t, "four notes", func() bool { return len(h.tg.Sent()) >= 4 })
	if n := len(h.sessions()); n != 1 {
		t.Errorf("%d empty conversations piled up", n)
	}
	h.say("/new renamed")
	h.waitText("New conversation: renamed")
	if got := h.sessions(); len(got) != 1 || got[0].Title != "renamed" {
		t.Errorf("sessions %+v", got)
	}
}

func TestAnswerToAnOldConversationIsKeptNotShown(t *testing.T) {
	h := newConvHarness(t, 5)
	h.run.gate = make(chan struct{})
	h.say("slow question")
	tgtest.WaitFor(t, "the run to start", func() bool { return len(h.run.Calls()) == 1 })
	old := h.sessions()[0].ID

	h.say("/new")
	h.waitText("New conversation.")
	close(h.run.gate) // the old turn finishes after the owner moved on
	tgtest.WaitFor(t, "the answer to be kept", func() bool {
		return len(h.chat.loadHistory(old)) == 2
	})
	time.Sleep(100 * time.Millisecond)
	if got := h.visibleTexts(); len(got) != 1 || got[0] != "New conversation." {
		t.Errorf("a late answer reached the new screen: %q", got)
	}
	if h := h.chat.loadHistory(old); h[1].From != "agent" || h[1].Text != "answer to slow question" {
		t.Errorf("history %+v", h)
	}
}

func TestClearingGoesOnWhenTelegramRefusesOldMessages(t *testing.T) {
	h := newConvHarness(t, 5)
	old := h.ask("old question")
	recent := h.ask("recent question")
	h.tg.MarkTooOld(old) // more than 48 hours ago
	h.say("/new")
	note := h.waitText("New conversation.")
	if !h.tg.Deleted(recent) {
		t.Error("the recent message was not deleted")
	}
	if h.tg.Deleted(old) {
		t.Error("the old message was deleted")
	}
	if !strings.Contains(note.Text, "48 hours") {
		t.Errorf("the owner is not told: %q", note.Text)
	}
}

func TestFinishedRequestsAreClearedWaitingOnesStay(t *testing.T) {
	var asked int
	h := newHarnessWith(t, time.Minute, func(o *Options) {
		o.HistoryDir = t.TempDir()
		// 900 is a request already decided; the waiting one (901) is not
		// offered, because the approver never returns it.
		o.Finished = func() []int64 { asked++; return []int64{900} }
	})
	h.chat.HandleText(context.Background(), "/new", h.tg.MessageID(owner, "/new"))
	h.waitText("New conversation.")
	if asked != 1 || !h.tg.Deleted(900) || h.tg.Deleted(901) {
		t.Errorf("asked %d, 900 deleted %v, 901 deleted %v", asked, h.tg.Deleted(900), h.tg.Deleted(901))
	}
}

func TestChecksLeaveNothingBehindWhenAllClear(t *testing.T) {
	h := newConvHarness(t, 5)
	checks := &fakeRunner{reply: func(string) (string, error) { return "ALL_CLEAR", nil }}
	h.chat.o.CheckRunner = checks
	h.say("/check")
	h.waitText("All clear.")
	if n := len(h.sessions()); n != 0 {
		t.Fatalf("%d conversations after an all-clear check", n)
	}
	call := checks.Calls()[0]
	if !strings.HasPrefix(call.db, sessionDir+"/") {
		t.Fatalf("the check had no database of its own: %q", call.db)
	}
	if got := h.files.removedDBs(); len(got) != 1 || got[0] != filepath.Base(call.db) {
		t.Errorf("the check's database was not deleted: %v", got)
	}
}

func TestAFindingKeepsItsDatabaseAndTheScreen(t *testing.T) {
	h := newConvHarness(t, 5)
	h.ask("hello")
	before := len(h.sessions()[0].Messages)
	checks := &fakeRunner{reply: func(string) (string, error) { return "Jellyfin is down. It crashed at 09:12.", nil }}
	h.chat.o.CheckRunner = checks
	h.say("/check")
	h.waitText("Jellyfin is down.")
	got := h.sessions()
	if len(got) != 2 || !got[1].Own || len(h.files.removedDBs()) != 0 {
		t.Fatalf("sessions %+v removed %v", got, h.files.removedDBs())
	}
	if h.chat.index.Active != got[1].ID || len(got[0].Messages) != 0 || len(got[1].Messages) < before {
		t.Errorf("the screen did not move to the finding's conversation: %+v", got)
	}
	// /new clears everything on screen: the first chat and the finding.
	h.say("/new")
	h.waitText("New conversation.")
	if v := h.visibleTexts(); len(v) != 1 {
		t.Errorf("screen after /new: %q", v)
	}
}

func TestHistoryIsBounded(t *testing.T) {
	h := newConvHarness(t, 5)
	h.ask("start")
	id := h.sessions()[0].ID
	for i := range 100 {
		h.chat.appendHistory(id, "owner", strings.Repeat("x", 3000)+string(rune('a'+i%26)))
	}
	if n := len(h.chat.loadHistory(id)); n != maxHistory {
		t.Errorf("%d entries kept, want %d", n, maxHistory)
	}
	h.chat.appendHistory(id, "agent", strings.Repeat("y", 100000))
	hist := h.chat.loadHistory(id)
	if n := len(hist[len(hist)-1].Text); n > maxEntry+10 {
		t.Errorf("an entry of %d bytes was kept", n)
	}
	// Resuming shows the end of it and says that earlier messages are not shown.
	h.say("/new")
	h.waitText("New conversation.")
	h.chat.replay(context.Background(), id)
	var shown strings.Builder
	for _, m := range h.tg.Visible() {
		if tgapi.TextLen(m.Text) > tgapi.MaxText {
			t.Errorf("a message of %d characters", tgapi.TextLen(m.Text))
		}
		shown.WriteString(m.Text)
	}
	if !strings.Contains(shown.String(), "Earlier messages are not shown.") {
		t.Error("the owner is not told the replay is partial")
	}
}

func TestConversationIDsBecomeFileNames(t *testing.T) {
	h := newConvHarness(t, 5)
	for _, bad := range []string{"", "../../etc/passwd", "x", "00000000-0000-0000-0000-00000000000g", strings.Repeat("a", 36)} {
		if p := h.chat.historyPath(bad); p != "" {
			t.Errorf("historyPath(%q) = %q", bad, p)
		}
	}
	if p := h.chat.historyPath("0123abcd-0123-4abc-8def-0123456789ab"); !strings.HasPrefix(p, h.hist+"/") {
		t.Errorf("valid id: %q", p)
	}
}

// loose and busy read the chat's state for the tests.
func (c *Chat) loose() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.index.Loose)
}

func (c *Chat) busy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cancel != nil
}
