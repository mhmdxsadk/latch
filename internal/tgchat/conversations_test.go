package tgchat

import (
	"context"
	"fmt"
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

const threadDir = "/data/latch-sessions"

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

// clock is a clock the tests move by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// convHarness is a chat with per-thread databases, transcripts and a
// clock of its own.
type convHarness struct {
	*harness
	files *store
	hist  string
	clock *clock
}

func newConvHarness(t *testing.T) *convHarness {
	t.Helper()
	files := &store{}
	hist := t.TempDir()
	clk := &clock{t: time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)}
	h := newHarnessWith(t, time.Minute, func(o *Options) {
		o.ThreadDir, o.HistoryDir, o.Remove, o.Now = threadDir, hist, files.remove, clk.Now
	})
	return &convHarness{harness: h, files: files, hist: hist, clock: clk}
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
	answer := h.waitText("answer to " + text)
	// The answer is on screen; wait until the chat has noted it too.
	tgtest.WaitFor(h.t, "the answer to be noted", func() bool {
		return slices.Contains(h.chat.screen(), answer.ID) && !h.chat.busy()
	})
	return id
}

// waitAllNoted waits until the chat has noted every message the bot has on
// screen, so a command that clears the screen does not miss one still being
// noted.
func (h *convHarness) waitAllNoted() {
	h.t.Helper()
	tgtest.WaitFor(h.t, "every message on screen to be noted", func() bool {
		noted := map[int64]bool{}
		for _, id := range h.chat.screen() {
			noted[id] = true
		}
		for _, m := range h.tg.Visible() {
			if !noted[m.ID] {
				return false
			}
		}
		return true
	})
}

// newThread sends /new once everything on screen has been noted.
func (h *convHarness) newThread() {
	h.t.Helper()
	h.waitAllNoted()
	n := len(h.tg.Sent())
	h.say("/new")
	tgtest.WaitFor(h.t, "the /new note", func() bool { return len(h.tg.Sent()) > n })
	h.waitAllNoted()
}

func (h *convHarness) threads() []thread {
	h.chat.mu.Lock()
	defer h.chat.mu.Unlock()
	return append([]thread(nil), h.chat.index.Threads...)
}

func (h *convHarness) visibleTexts() []string {
	var out []string
	for _, m := range h.tg.Visible() {
		out = append(out, m.Text)
	}
	return out
}

func (h *convHarness) picker() tgtest.Sent {
	h.t.Helper()
	n := len(h.tg.Sent())
	h.say("/threads")
	tgtest.WaitFor(h.t, "the list", func() bool { return len(h.tg.Sent()) > n })
	sent := h.tg.Sent()
	return sent[len(sent)-1]
}

func (h *convHarness) press(list tgtest.Sent, row int) {
	h.t.Helper()
	cb := tgapi.CallbackQuery{ID: "cb1", Data: list.Keyboard[row][0].Data, Message: &tgapi.Message{MessageID: list.ID}}
	if !h.chat.HandlePress(context.Background(), cb) {
		h.t.Fatal("not handled")
	}
}

func TestNewClearsTheScreenAndKeepsTheThread(t *testing.T) {
	h := newConvHarness(t)
	id1 := h.ask("is caddy up?")
	id2 := h.ask("and jellyfin?")
	first := h.threads()[0]
	if first.Title != "is caddy up?" || len(first.ID) != 36 || first.Started.IsZero() {
		t.Fatalf("first thread %+v", first)
	}
	if c := h.run.Calls(); c[0].db != filepath.Join(threadDir, first.ID+".db") || c[1].db != c[0].db {
		t.Fatalf("turns used databases %q, %q", c[0].db, c[1].db)
	}

	cmd := h.say("/new")
	h.waitText("New thread.")
	for _, id := range []int64{id1, id2, cmd} {
		if !h.tg.Deleted(id) {
			t.Errorf("the owner's message %d is still on screen", id)
		}
	}
	if got := h.visibleTexts(); len(got) != 1 || got[0] != "New thread." {
		t.Fatalf("screen after /new: %q", got)
	}
	if got := h.files.removed; len(got) != 0 {
		t.Errorf("/new deleted %v", got)
	}

	// The next message starts a fresh thread with its own database.
	h.ask("disk space?")
	second := h.run.Calls()[2]
	if second.thread == first.ID || second.db != filepath.Join(threadDir, second.thread+".db") {
		t.Errorf("turn after /new ran in %+v", second)
	}
	if got := h.threads(); len(got) != 2 || got[1].Title != "disk space?" {
		t.Errorf("threads %+v", got)
	}
}

func TestClearClearsTheScreenAndStaysInTheThread(t *testing.T) {
	h := newConvHarness(t)
	id1 := h.ask("is caddy up?")
	first := h.threads()[0]
	h.waitAllNoted()
	cmd := h.say("/clear")
	note := h.waitText("Cleared.")
	for _, id := range []int64{id1, cmd} {
		if !h.tg.Deleted(id) {
			t.Errorf("message %d is still on screen", id)
		}
	}
	if got := h.visibleTexts(); len(got) != 1 || got[0] != note.Text {
		t.Fatalf("screen after /clear: %q", got)
	}
	if len(note.Entities) != 1 || note.Entities[0].Type != "italic" {
		t.Errorf("the note is not italic: %+v", note.Entities)
	}
	if h.chat.index.Active != first.ID || len(h.files.removed) != 0 {
		t.Errorf("active %q, deleted %v", h.chat.index.Active, h.files.removed)
	}
	// The next message goes on in the same thread and database.
	h.ask("and jellyfin?")
	calls := h.run.Calls()
	if got := h.threads(); len(got) != 1 || calls[1].thread != first.ID || calls[1].db != calls[0].db {
		t.Errorf("threads %+v, turns %+v", got, calls)
	}
}

func TestThreadsListsAndResumesWithTheHistory(t *testing.T) {
	h := newConvHarness(t)
	h.ask("is caddy up?")
	h.ask("and jellyfin?")
	first := h.threads()[0]
	h.clock.Advance(time.Hour)
	h.newThread()
	h.ask("disk space?")

	list := h.picker()
	if len(list.Keyboard) != 2 || !strings.HasPrefix(list.Keyboard[0][0].Text, "* disk space?") || !strings.HasPrefix(list.Keyboard[1][0].Text, "is caddy up?") {
		t.Fatalf("list %+v", list.Keyboard)
	}
	h.press(list, 1)
	tgtest.WaitFor(t, "the resume note", func() bool {
		v := h.visibleTexts()
		return len(v) > 0 && strings.HasPrefix(v[len(v)-1], "Back in: is caddy up?")
	})
	// The screen shows the first thread again, nothing of the second.
	var shown strings.Builder
	for _, m := range h.tg.Visible() {
		shown.WriteString(m.Text + "\n")
	}
	for _, want := range []string{"is caddy up?", "answer to is caddy up?", "and jellyfin?", "answer to and jellyfin?"} {
		if !strings.Contains(shown.String(), want) {
			t.Errorf("history lacks %q:\n%s", want, shown.String())
		}
	}
	for _, gone := range []string{"disk space?", "answer to disk space?", "Pick a thread"} {
		if strings.Contains(shown.String(), gone) {
			t.Errorf("the other thread still shows %q", gone)
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
	// The thread continues where it was: same thread, same database.
	h.ask("any news?")
	last := h.run.Calls()[len(h.run.Calls())-1]
	if last.thread != first.ID || last.db != filepath.Join(threadDir, first.ID+".db") {
		t.Errorf("resumed turn ran in %+v, want thread %s", last, first.ID)
	}
	if h.files.removed != nil {
		t.Errorf("deleted %v", h.files.removed)
	}
}

func TestThreadsListWithNoThreads(t *testing.T) {
	h := newConvHarness(t)
	h.say("/threads")
	h.waitText("No threads yet.")
	h.say("/new")
	h.waitText("New thread.")
	if n := len(h.threads()); n != 0 || len(h.files.removed) != 0 {
		t.Errorf("%d threads, deleted %v", n, h.files.removed)
	}
}

func TestOnlyTenThreadsAreKept(t *testing.T) {
	h := newConvHarness(t)
	var ids []string
	for i := range MaxThreads + 2 {
		q := fmt.Sprintf("question %d", i)
		h.ask(q)
		h.clock.Advance(time.Minute)
		ids = append(ids, h.threads()[len(h.threads())-1].ID)
		h.newThread()
	}
	h.ask("last question")
	got := h.threads()
	if len(got) != MaxThreads || got[0].Title != "question 3" || got[len(got)-1].Title != "last question" {
		t.Fatalf("kept %d: %+v", len(got), got)
	}
	// 13 threads began, so the three oldest are gone.
	removed := h.files.removedDBs()
	if len(removed) != 3 || removed[0] != ids[0]+".db" || removed[1] != ids[1]+".db" || removed[2] != ids[2]+".db" {
		t.Errorf("removed databases %v, want the oldest three (%v)", removed, ids[:3])
	}
	for _, p := range h.files.removed {
		if !strings.HasPrefix(p, threadDir+"/") {
			t.Errorf("deleted outside the thread folder: %s", p)
		}
	}
	for _, id := range ids[:3] {
		if _, err := os.Stat(filepath.Join(h.hist, id+".json")); !os.IsNotExist(err) {
			t.Errorf("transcript of %s was kept", id)
		}
	}
	for _, id := range ids[3:] {
		if _, err := os.Stat(filepath.Join(h.hist, id+".json")); err != nil {
			t.Errorf("transcript of %s is gone: %v", id, err)
		}
	}
	list := h.picker()
	if len(list.Keyboard) != MaxThreads {
		t.Errorf("list has %d buttons", len(list.Keyboard))
	}
}

func TestPruningSparesTheActiveThread(t *testing.T) {
	h := newConvHarness(t)
	h.ask("the old one")
	old := h.threads()[0].ID
	for i := range MaxThreads {
		h.clock.Advance(time.Hour)
		h.newThread()
		h.ask(fmt.Sprintf("later %d", i))
	}
	// The oldest thread was the first to go.
	for _, s := range h.threads() {
		if s.ID == old {
			t.Fatalf("the oldest thread is still kept: %+v", h.threads())
		}
	}
	// Resuming the oldest kept one makes it the newest, so it outlives the next.
	list := h.picker()
	h.press(list, len(list.Keyboard)-1)
	tgtest.WaitFor(t, "the resume note", func() bool {
		v := h.visibleTexts()
		return len(v) > 0 && strings.HasPrefix(v[len(v)-1], "Back in: later 0")
	})
	resumed := h.chat.index.Active
	h.clock.Advance(time.Hour)
	h.newThread()
	h.ask("one more")
	found := false
	for _, s := range h.threads() {
		found = found || s.ID == resumed
	}
	if !found || len(h.threads()) != MaxThreads {
		t.Errorf("the resumed thread was pruned or the count is off: %+v", h.threads())
	}
}

func TestAQuietThreadIsLeftAfterThreeDays(t *testing.T) {
	h := newConvHarness(t)
	h.ask("first")
	first := h.threads()[0].ID

	// Just under three days: still the same thread.
	h.clock.Advance(IdleAfter - time.Minute)
	h.ask("still there?")
	if got := h.run.Calls()[1].thread; got != first {
		t.Fatalf("a message after almost three days started %s", got)
	}

	// Three days without a message to it: the next one starts a new one.
	h.clock.Advance(IdleAfter)
	h.ask("hello again")
	second := h.run.Calls()[2]
	if second.thread == first || second.db != filepath.Join(threadDir, second.thread+".db") {
		t.Fatalf("the message after three days ran in %+v", second)
	}
	got := h.threads()
	if len(got) != 2 || got[0].ID != first || got[1].Title != "hello again" || len(h.files.removed) != 0 {
		t.Errorf("threads %+v, deleted %v", got, h.files.removed)
	}
	// The old messages were too old to delete anyway: they are not tracked.
	if len(got[0].Messages) != 0 {
		t.Errorf("the old thread still tracks %v", got[0].Messages)
	}
	// The old thread can be resumed, and resuming restarts its clock.
	list := h.picker()
	h.press(list, 1)
	tgtest.WaitFor(t, "the resume note", func() bool {
		v := h.visibleTexts()
		return len(v) > 0 && strings.HasPrefix(v[len(v)-1], "Back in: first")
	})
	h.clock.Advance(IdleAfter - time.Minute)
	h.ask("back")
	if got := h.run.Calls()[3].thread; got != first {
		t.Errorf("a message after resuming ran in %s, not the resumed %s", got, first)
	}
}

func TestAnswerToAnOldThreadIsKeptNotShown(t *testing.T) {
	h := newConvHarness(t)
	h.run.gate = make(chan struct{})
	h.say("slow question")
	tgtest.WaitFor(t, "the run to start", func() bool { return len(h.run.Calls()) == 1 })
	old := h.threads()[0].ID

	h.say("/new")
	h.waitText("New thread.")
	close(h.run.gate) // the old turn finishes after the owner moved on
	tgtest.WaitFor(t, "the answer to be kept", func() bool {
		return len(h.chat.loadHistory(old)) == 2
	})
	tgtest.WaitFor(t, "the turn to end", func() bool { return !h.chat.busy() })
	if got := h.visibleTexts(); len(got) != 1 || got[0] != "New thread." {
		t.Errorf("a late answer reached the new screen: %q", got)
	}
	if h := h.chat.loadHistory(old); h[1].From != "agent" || h[1].Text != "answer to slow question" {
		t.Errorf("history %+v", h)
	}
}

func TestMessagesWaitingForAnOldThreadAreDropped(t *testing.T) {
	h := newConvHarness(t)
	h.run.gate = make(chan struct{})
	h.say("first")
	tgtest.WaitFor(t, "the run to start", func() bool { return len(h.run.Calls()) == 1 })
	h.say("second") // waits behind the first
	h.say("/new")
	h.waitText("New thread.")
	close(h.run.gate)
	tgtest.WaitFor(t, "the turn to end", func() bool { return !h.chat.busy() })
	if n := len(h.run.Calls()); n != 1 {
		t.Errorf("%d turns ran; the waiting message should have been dropped", n)
	}
}

func TestClearingGoesOnWhenTelegramRefusesOldMessages(t *testing.T) {
	h := newConvHarness(t)
	old := h.ask("old question")
	recent := h.ask("recent question")
	h.tg.MarkTooOld(old) // more than 48 hours ago
	h.say("/new")
	note := h.waitText("New thread.")
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
		// 900 is a request already decided; the waiting one (901) is not
		// offered, because the approver never returns it.
		o.Finished = func() []int64 { asked++; return []int64{900} }
	})
	h.chat.HandleText(context.Background(), "/new", h.tg.MessageID(owner, "/new"))
	h.waitText("New thread.")
	if asked != 1 || !h.tg.Deleted(900) || h.tg.Deleted(901) {
		t.Errorf("asked %d, 900 deleted %v, 901 deleted %v", asked, h.tg.Deleted(900), h.tg.Deleted(901))
	}
}

func TestHistoryIsBounded(t *testing.T) {
	h := newConvHarness(t)
	h.ask("start")
	id := h.threads()[0].ID
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
	h.newThread()
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

func TestThreadIDsBecomeFileNames(t *testing.T) {
	h := newConvHarness(t)
	for _, bad := range []string{"", "../../etc/passwd", "x", "00000000-0000-0000-0000-00000000000g", strings.Repeat("a", 36)} {
		if p := h.chat.historyPath(bad); p != "" {
			t.Errorf("historyPath(%q) = %q", bad, p)
		}
	}
	if p := h.chat.historyPath("0123abcd-0123-4abc-8def-0123456789ab"); !strings.HasPrefix(p, h.hist+"/") {
		t.Errorf("valid id: %q", p)
	}
}

// screen, loose and busy read the chat's state for the tests. The screen is
// the active thread's messages and the loose ones.
func (c *Chat) screen() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids := slices.Clone(c.index.Loose)
	if s := c.currentLocked(); s != nil {
		ids = append(ids, s.Messages...)
	}
	return ids
}

func (c *Chat) busy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cancel != nil
}

// jpeg is enough of a JPEG for content sniffing.
var jpeg = []byte{0xff, 0xd8, 0xff, 0xe0, 0, 0x10, 'J', 'F', 'I', 'F', 0}

func newPhotoHarness(t *testing.T) (*convHarness, *[]string) {
	t.Helper()
	var saved []string
	files := &store{}
	h := newHarnessWith(t, time.Minute, func(o *Options) {
		o.ThreadDir, o.Remove = threadDir, files.remove
		o.SaveImage = func(_ context.Context, data []byte, ext string) (string, error) {
			p := threadDir + "/photo-" + string(rune('a'+len(saved))) + ext
			saved = append(saved, p)
			return p, nil
		}
	})
	return &convHarness{harness: h, files: files}, &saved
}

func TestPhotoIsAttached(t *testing.T) {
	h, saved := newPhotoHarness(t)
	h.chat.HandleImages(context.Background(), "what is this?", [][]byte{jpeg}, []int64{7})
	h.waitText("answer to what is this?")
	calls := h.run.Calls()
	if len(calls) != 1 || len(calls[0].images) != 1 || calls[0].images[0] != (*saved)[0] {
		t.Fatalf("calls %+v, saved %v", calls, *saved)
	}
	tgtest.WaitFor(t, "the photo to be deleted", func() bool {
		h.files.mu.Lock()
		defer h.files.mu.Unlock()
		return len(h.files.removed) == 1 && h.files.removed[0] == (*saved)[0]
	})
}

func TestSeveralPhotosAreOneTurn(t *testing.T) {
	h, saved := newPhotoHarness(t)
	h.chat.HandleImages(context.Background(), "compare these", [][]byte{jpeg, jpeg, jpeg}, []int64{1, 2, 3})
	h.waitText("answer to compare these")
	calls := h.run.Calls()
	if len(calls) != 1 || len(calls[0].images) != 3 || len(*saved) != 3 {
		t.Fatalf("calls %+v, saved %v", calls, *saved)
	}
}

func TestPhotoWithoutCaptionGetsDefaultText(t *testing.T) {
	h, _ := newPhotoHarness(t)
	h.chat.HandleImages(context.Background(), "  ", [][]byte{jpeg}, []int64{7})
	h.waitText("answer to [User attached one or more files without additional text.")
}

func TestPhotoRefusals(t *testing.T) {
	h, saved := newPhotoHarness(t)
	h.chat.HandleImages(context.Background(), "x", [][]byte{jpeg, []byte("<html>not an image</html>")}, []int64{7})
	h.waitText("Only JPEG and PNG")
	h.chat.HandleImages(context.Background(), strings.Repeat("x", maxMessage+1), [][]byte{jpeg}, []int64{8})
	h.waitText("caption is too long")
	if len(*saved) != 0 || len(h.run.Calls()) != 0 {
		t.Fatalf("saved %v, calls %v", *saved, h.run.Calls())
	}

	plain := newHarness(t, time.Minute) // no SaveImage
	plain.chat.HandleImages(context.Background(), "x", [][]byte{jpeg}, []int64{9})
	plain.waitText("Photos are not available")
}

func TestStopDeletesWaitingPhotos(t *testing.T) {
	h, saved := newPhotoHarness(t)
	h.run.gate = make(chan struct{})
	h.chat.HandleImages(context.Background(), "first", [][]byte{jpeg}, []int64{1})
	tgtest.WaitFor(t, "the first turn to start", func() bool { return len(h.run.Calls()) == 1 })
	h.chat.HandleImages(context.Background(), "second", [][]byte{jpeg}, []int64{2})
	h.chat.HandleText(context.Background(), "/stop", 3)
	h.waitText("dropped 1 waiting")
	close(h.run.gate)
	tgtest.WaitFor(t, "both photos to be deleted", func() bool {
		h.files.mu.Lock()
		defer h.files.mu.Unlock()
		return len(h.files.removed) == 2
	})
	if len(*saved) != 2 {
		t.Fatalf("saved %v", *saved)
	}
}

func TestPhotoStartsAThreadTitledPhoto(t *testing.T) {
	h, _ := newPhotoHarness(t)
	h.chat.HandleImages(context.Background(), "", [][]byte{jpeg}, []int64{7})
	h.waitText("answer to [User attached one or more files")
	got := h.chat.index.Threads
	if len(got) != 1 || got[0].Title != "Photo" {
		t.Fatalf("threads %+v", got)
	}
	tgtest.WaitFor(t, "the photo turn to end", func() bool { return !h.chat.busy() })
}

func TestSettleClearsTheScreenAndKeepsTheThread(t *testing.T) {
	h := newConvHarness(t)
	id1 := h.ask("is caddy up?")
	first := h.threads()[0]
	h.waitAllNoted()
	cmd := h.say("/settle")
	note := h.waitText("Settled: is caddy up?")
	for _, id := range []int64{id1, cmd} {
		if !h.tg.Deleted(id) {
			t.Errorf("message %d is still on screen", id)
		}
	}
	if got := h.visibleTexts(); len(got) != 1 || got[0] != note.Text {
		t.Fatalf("screen after /settle: %q", got)
	}
	got := h.threads()
	if len(got) != 1 || got[0].Settled.IsZero() || h.chat.index.Active != "" || len(h.files.removed) != 0 {
		t.Fatalf("threads %+v, active %q, deleted %v", got, h.chat.index.Active, h.files.removed)
	}
	// The next message starts another thread; the settled one is listed
	// after the open one and can be opened again.
	h.clock.Advance(time.Hour)
	h.ask("disk space?")
	list := h.picker()
	if len(list.Keyboard) != 2 || !strings.HasPrefix(list.Keyboard[0][0].Text, "* disk space?") ||
		!strings.HasPrefix(list.Keyboard[1][0].Text, "is caddy up?") || !strings.HasSuffix(list.Keyboard[1][0].Text, " - settled") {
		t.Fatalf("list %+v", list.Keyboard)
	}
	h.press(list, 1)
	tgtest.WaitFor(t, "the resume note", func() bool {
		v := h.visibleTexts()
		return len(v) > 0 && strings.HasPrefix(v[len(v)-1], "Back in: is caddy up?")
	})
	if s := h.threads()[0]; !s.Settled.IsZero() || h.chat.index.Active != first.ID {
		t.Errorf("reopening left %+v active %q", s, h.chat.index.Active)
	}
	h.ask("one more thing")
	if last := h.run.Calls()[len(h.run.Calls())-1]; last.thread != first.ID {
		t.Errorf("a message after reopening ran in %s", last.thread)
	}
}

func TestSettleWithNothingOpen(t *testing.T) {
	h := newConvHarness(t)
	h.say("/settle")
	h.waitText("No open thread to settle.")
	if len(h.threads()) != 0 || len(h.files.removed) != 0 {
		t.Errorf("threads %+v, deleted %v", h.threads(), h.files.removed)
	}
}

func TestQuietThreadsCountAsSettled(t *testing.T) {
	h := newConvHarness(t)
	h.ask("first")
	h.clock.Advance(time.Hour)
	h.newThread()
	h.ask("second")
	h.clock.Advance(IdleAfter - time.Hour/2) // first: quiet for IdleAfter and more; second: not yet
	list := h.picker()
	if len(list.Keyboard) != 2 || !strings.HasPrefix(list.Keyboard[0][0].Text, "* second") || strings.HasSuffix(list.Keyboard[0][0].Text, "settled") ||
		!strings.HasPrefix(list.Keyboard[1][0].Text, "first") || !strings.HasSuffix(list.Keyboard[1][0].Text, " - settled") {
		t.Fatalf("list %+v", list.Keyboard)
	}
	h.clock.Advance(time.Hour) // now the second has been quiet that long too
	list = h.picker()
	if !strings.HasSuffix(list.Keyboard[0][0].Text, " - settled") || !strings.HasSuffix(list.Keyboard[1][0].Text, " - settled") {
		t.Errorf("list %+v", list.Keyboard)
	}
}
