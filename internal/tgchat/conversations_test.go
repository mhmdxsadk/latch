package tgchat

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// convHarness is a chat with per-conversation databases.
type convHarness struct {
	*harness
	files *store
}

func newConvHarness(t *testing.T) *convHarness {
	t.Helper()
	files := &store{}
	h := newHarnessWith(t, time.Minute, func(o *Options) {
		o.SessionDir, o.Remove = sessionDir, files.remove
	})
	return &convHarness{harness: h, files: files}
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
		return len(h.chat.screen()) >= 2 && !h.chat.busy()
	})
	return id
}

func (h *convHarness) visibleTexts() []string {
	var out []string
	for _, m := range h.tg.Visible() {
		out = append(out, m.Text)
	}
	return out
}

func TestNewClearsTheScreenAndDeletesTheConversation(t *testing.T) {
	h := newConvHarness(t)
	id1 := h.ask("is caddy up?")
	id2 := h.ask("and jellyfin?")
	first := h.chat.conversation()
	if len(first) != 36 {
		t.Fatalf("conversation %q", first)
	}
	if c := h.run.Calls(); c[0].db != filepath.Join(sessionDir, first+".db") || c[1].db != c[0].db {
		t.Fatalf("turns used databases %q, %q", c[0].db, c[1].db)
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
	if got := h.files.removedDBs(); len(got) != 1 || got[0] != first+".db" {
		t.Errorf("removed databases %v, want %s.db", got, first)
	}
	for _, p := range h.files.removed {
		if !strings.HasPrefix(p, sessionDir+"/") {
			t.Errorf("deleted outside the session folder: %s", p)
		}
	}

	// The next message starts a fresh conversation with its own database.
	h.ask("disk space?")
	second := h.run.Calls()[2]
	if second.session == first || second.db != filepath.Join(sessionDir, second.session+".db") {
		t.Errorf("turn after /new ran in %+v", second)
	}
}

func TestNewWithNoConversationDeletesNothing(t *testing.T) {
	h := newConvHarness(t)
	h.say("/new")
	h.waitText("New conversation.")
	if got := h.files.removed; len(got) != 0 {
		t.Errorf("deleted %v", got)
	}
}

func TestAnswerToAnOldConversationIsDropped(t *testing.T) {
	h := newConvHarness(t)
	h.run.gate = make(chan struct{})
	h.say("slow question")
	tgtest.WaitFor(t, "the run to start", func() bool { return len(h.run.Calls()) == 1 })
	old := h.run.Calls()[0].session

	h.say("/new")
	h.waitText("New conversation.")
	close(h.run.gate) // the old turn finishes after the owner moved on
	tgtest.WaitFor(t, "the old database to be removed again", func() bool {
		n := 0
		for _, db := range h.files.removedDBs() {
			if db == old+".db" {
				n++
			}
		}
		return n == 2 // by /new, and again in case the run wrote it after that
	})
	tgtest.WaitFor(t, "the turn to end", func() bool { return !h.chat.busy() })
	if got := h.visibleTexts(); len(got) != 1 || got[0] != "New conversation." {
		t.Errorf("a late answer reached the new screen: %q", got)
	}
}

func TestMessagesWaitingForAnOldConversationAreDropped(t *testing.T) {
	h := newConvHarness(t)
	h.run.gate = make(chan struct{})
	h.say("first")
	tgtest.WaitFor(t, "the run to start", func() bool { return len(h.run.Calls()) == 1 })
	h.say("second") // waits behind the first
	h.say("/new")
	h.waitText("New conversation.")
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
	h := newConvHarness(t)
	checks := &fakeRunner{reply: func(string) (string, error) { return "ALL_CLEAR", nil }}
	h.chat.o.CheckRunner = checks
	h.say("/check")
	h.waitText("All clear.")
	if id := h.chat.conversation(); id != "" {
		t.Fatalf("conversation %s after an all-clear check", id)
	}
	call := checks.Calls()[0]
	if !strings.HasPrefix(call.db, sessionDir+"/") {
		t.Fatalf("the check had no database of its own: %q", call.db)
	}
	if got := h.files.removedDBs(); len(got) != 1 || got[0] != filepath.Base(call.db) {
		t.Errorf("the check's database was not deleted: %v", got)
	}
}

func TestAFindingReplacesTheConversationAndKeepsTheScreen(t *testing.T) {
	h := newConvHarness(t)
	h.ask("hello")
	before := len(h.chat.screen())
	chatID := h.chat.conversation()
	checks := &fakeRunner{reply: func(string) (string, error) { return "Jellyfin is down. It crashed at 09:12.", nil }}
	h.chat.o.CheckRunner = checks
	h.say("/check")
	h.waitText("Jellyfin is down.")
	found := filepath.Base(checks.Calls()[0].db)
	if got := h.chat.conversation() + ".db"; got != found {
		t.Errorf("current conversation %s, want the check's %s", got, found)
	}
	if got := h.files.removedDBs(); len(got) != 1 || got[0] != chatID+".db" {
		t.Errorf("removed %v, want only the replaced conversation %s.db", got, chatID)
	}
	if len(h.chat.screen()) < before {
		t.Error("the screen was forgotten")
	}
	// /new clears everything on screen: the first chat and the finding.
	h.say("/new")
	h.waitText("New conversation.")
	tgtest.WaitFor(t, "only the new conversation on screen", func() bool { return len(h.visibleTexts()) == 1 })
}

// screen, conversation and busy read the chat's state for the tests.
func (c *Chat) screen() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int64(nil), c.index.Messages...)
}

func (c *Chat) conversation() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.index.Conversation
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
		o.SessionDir, o.Remove = sessionDir, files.remove
		o.SaveImage = func(data []byte, ext string) (string, error) {
			p := sessionDir + "/photo-" + string(rune('a'+len(saved))) + ext
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
