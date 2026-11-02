package tgchat

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mhmdxsadk/latch/internal/textsafe"
	"github.com/mhmdxsadk/latch/internal/tgapi"
	"github.com/mhmdxsadk/latch/internal/tgapprove"
)

// A conversation is a Docker Agent session plus what this package keeps for
// it: the messages on the owner's screen (so /new can clear them) and a
// transcript (so resuming can show them again). The screen always belongs to
// the active conversation. A conversation is meant to solve one issue and
// then be settled: /settle (or the Settle button under the agent's offer)
// closes it, clearing the screen like /new, and /sessions lists it under the
// settled ones, from where the owner can reopen it. A conversation the owner
// has not written to for IdleAfter counts as settled too, and the next
// message starts a new one. At most MaxSessions conversations are kept, open
// and settled; the least recently used ones beyond that are deleted,
// database and transcript included.

const (
	// MaxSessions is how many conversations are kept.
	MaxSessions = 10
	// IdleAfter is how long a conversation can go without a message from
	// the owner before it counts as settled.
	IdleAfter = 3 * 24 * time.Hour
	// titleLen is the longest conversation title.
	titleLen = 40
	// maxScreen bounds the message ids remembered per conversation.
	maxScreen = 2000
	// maxHistory and maxEntry bound a transcript.
	maxHistory = 60
	maxEntry   = 16 << 10
	// replayChars is how much of a transcript is shown when resuming.
	replayChars = 12000
)

// uuid is the shape of a conversation id; ids become file names.
var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type session struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Started  time.Time `json:"started"`
	LastUsed time.Time `json:"last_used,omitzero"`
	// Settled is when the owner (or the agent's offer) closed it. Reopening
	// clears it.
	Settled time.Time `json:"settled,omitzero"`
	// Messages are the ids of the messages on the owner's screen.
	Messages []int64 `json:"messages,omitempty"`
}

// entry is one line of a transcript.
type entry struct {
	From string `json:"from"` // "owner" or "agent"
	Text string `json:"text"` // the owner's text, or the agent's Markdown
}

func (s session) used() time.Time {
	if s.LastUsed.IsZero() {
		return s.Started
	}
	return s.LastUsed
}

// isSettled reports whether s is closed: settled on purpose, or quiet for
// IdleAfter.
func (c *Chat) isSettled(s session) bool {
	return !s.Settled.IsZero() || c.o.Now().Sub(s.used()) >= IdleAfter
}

func (s session) label(loc *time.Location) string {
	t := s.Title
	if t == "" {
		t = "untitled"
	}
	return t + " (" + s.Started.In(loc).Format("Jan 2 15:04") + ")"
}

// title makes a short one-line title from text.
func title(text string) string {
	t := strings.Join(strings.Fields(textsafe.Line(strings.ReplaceAll(text, "\n", " "))), " ")
	if utf8.RuneCountInString(t) > titleLen {
		r := []rune(t)
		t = strings.TrimSpace(string(r[:titleLen])) + "..."
	}
	return t
}

// dbFor is the Docker Agent database of conversation id: its own file, or ""
// for the agent's shared default database (when no session directory is set).
func (c *Chat) dbFor(id string) string {
	if c.o.SessionDir == "" || id == "" {
		return ""
	}
	return filepath.Join(c.o.SessionDir, id+".db")
}

// track remembers messages on the owner's screen as part of the active
// conversation, so /new can delete them.
func (c *Chat) track(ids ...int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Without an active conversation the messages are loose; the next
	// conversation takes them over.
	list := &c.index.Loose
	if s := c.currentLocked(); s != nil {
		list = &s.Messages
	}
	for _, id := range ids {
		if id != 0 {
			*list = append(*list, id)
		}
	}
	if over := len(*list) - maxScreen; over > 0 {
		*list = slices.Clone((*list)[over:])
	}
	if err := c.saveLocked(); err != nil {
		c.o.Logger.Warn("saving the chat state failed", "err", err)
	}
}

// post sends t to the owner and remembers the message.
func (c *Chat) post(ctx context.Context, t tgapi.Text, kb tgapi.Keyboard) (tgapi.Message, error) {
	m, err := c.o.Bot.Send(ctx, c.o.Chat, t, kb)
	if err == nil {
		c.track(m.MessageID)
	}
	return m, err
}

func (c *Chat) isActive(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.index.Active == id
}

// exists reports whether conversation id is still kept.
func (c *Chat) exists(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.ContainsFunc(c.index.Sessions, func(s session) bool { return s.ID == id })
}

// stopRunning stops the running turn and drops the waiting ones. It returns
// how many were dropped and whether a turn was running.
func (c *Chat) stopRunning() (dropped int, running bool) {
drain:
	for {
		select {
		case j := <-c.queue:
			dropped++
			c.discard(j.images)
		default:
			break drain
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		c.cancel()
		running = true
	}
	return dropped, running
}

// --- transcripts ---

func (c *Chat) historyPath(id string) string {
	if c.o.HistoryDir == "" || !uuid.MatchString(id) {
		return ""
	}
	return filepath.Join(c.o.HistoryDir, id+".json")
}

func (c *Chat) loadHistory(id string) []entry {
	p := c.historyPath(id)
	if p == "" {
		return nil
	}
	b, err := os.ReadFile(p) //nolint:gosec // built from a validated id under the state directory
	if err != nil {
		return nil
	}
	var h []entry
	if json.Unmarshal(b, &h) != nil {
		return nil
	}
	return h
}

// appendHistory adds a line to conversation id's transcript, unless the
// conversation was deleted meanwhile.
func (c *Chat) appendHistory(id, from, text string) {
	p := c.historyPath(id)
	if p == "" || strings.TrimSpace(text) == "" || !c.exists(id) {
		return
	}
	c.hmu.Lock()
	defer c.hmu.Unlock()
	h := c.loadHistory(id)
	if len(text) > maxEntry {
		text = text[:maxEntry]
		for len(text) > 0 && text[len(text)-1]&0xC0 == 0x80 { // keep valid UTF-8
			text = text[:len(text)-1]
		}
		text += "\n..."
	}
	h = append(h, entry{From: from, Text: text})
	if len(h) > maxHistory {
		h = h[len(h)-maxHistory:]
	}
	b, err := json.Marshal(h)
	if err == nil {
		err = tgapprove.WriteFile(p, b)
	}
	if err != nil {
		c.o.Logger.Warn("saving the conversation history failed", "err", err)
	}
}

// replay shows a conversation's earlier messages again: the owner's in a
// quote, the agent's answers as they were. Only the most recent part is
// shown.
func (c *Chat) replay(ctx context.Context, id string) {
	c.hmu.Lock()
	h := c.loadHistory(id)
	c.hmu.Unlock()
	if len(h) == 0 {
		return
	}
	// The newest entries that fit replayChars, oldest first.
	start, total := len(h), 0
	for start > 0 && total+len(h[start-1].Text) <= replayChars {
		start--
		total += len(h[start].Text)
	}
	if start == len(h) {
		start = len(h) - 1 // the newest entry alone is longer: show its start
	}
	var parts []tgapi.Text
	if start > 0 {
		parts = append(parts, note("Earlier messages are not shown."))
	}
	for _, e := range h[start:] {
		switch e.From {
		case "owner":
			var b tgapi.Builder
			parts = append(parts, b.Quote(tgapi.Plain(strings.TrimSpace(e.Text)), len(e.Text) > 600).Done())
		default:
			parts = append(parts, tgapi.SplitText(Markdown(e.Text), tgapi.MaxText)...)
		}
	}
	// Pack the parts into as few messages as fit.
	var cur tgapi.Builder
	flush := func() {
		if cur.Len() > 0 {
			if _, err := c.post(ctx, cur.Done(), nil); err != nil {
				c.o.Logger.Warn("showing the history failed", "err", err)
			}
			cur = tgapi.Builder{}
		}
	}
	for _, p := range parts {
		if cur.Len() > 0 && cur.Len()+2+p.Len() > tgapi.MaxText {
			flush()
		}
		if cur.Len() > 0 {
			cur.Plain("\n\n")
		}
		cur.Text(p)
	}
	flush()
}

// --- clearing the screen ---

// clearScreen deletes messages from the owner's chat, plus the finished
// requests' messages. Requests still waiting stay: they have buttons. It
// returns how many messages Telegram would not delete (older than 48 hours).
func (c *Chat) clearScreen(ctx context.Context, ids []int64) (failed int) {
	if c.o.Finished != nil {
		ids = append(ids, c.o.Finished()...)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for chunk := range slices.Chunk(ids, tgapi.MaxDelete) {
		if err := c.o.Bot.DeleteMessages(ctx, c.o.Chat, chunk); err == nil {
			continue
		}
		// One message that cannot be deleted can fail the whole call: go
		// one by one so the rest still go.
		for _, id := range chunk {
			if err := c.o.Bot.DeleteMessages(ctx, c.o.Chat, []int64{id}); err != nil {
				failed++
			}
		}
	}
	return failed
}

// --- keeping only the recent conversations ---

// pruneLocked removes the conversations beyond MaxSessions, least recently
// used first, never the active one, and returns them so their files can be
// deleted.
func (c *Chat) pruneLocked() []session {
	var victims []session
	for len(c.index.Sessions) > MaxSessions {
		oldest := -1
		for i, s := range c.index.Sessions {
			if s.ID == c.index.Active {
				continue
			}
			if oldest < 0 || s.used().Before(c.index.Sessions[oldest].used()) {
				oldest = i
			}
		}
		if oldest < 0 {
			break
		}
		victims = append(victims, c.index.Sessions[oldest])
		c.index.Sessions = slices.Delete(c.index.Sessions, oldest, oldest+1)
	}
	return victims
}

// dropSessions deletes the files of conversations that were pruned.
func (c *Chat) dropSessions(ctx context.Context, victims []session) {
	for _, s := range victims {
		c.removeDB(ctx, c.dbFor(s.ID))
		if p := c.historyPath(s.ID); p != "" {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				c.o.Logger.Warn("removing a conversation history failed", "err", err)
			}
		}
	}
}

// removeDB deletes a conversation's database file and the files SQLite
// keeps beside it.
func (c *Chat) removeDB(ctx context.Context, db string) {
	if db == "" || c.o.Remove == nil {
		return
	}
	if err := c.o.Remove(ctx, []string{db, db + "-wal", db + "-shm", db + "-journal"}); err != nil {
		c.o.Logger.Warn("removing a conversation database failed", "err", err)
	}
}

// --- starting, /new and switching ---

// rollOver leaves the active conversation when the owner has been quiet for
// IdleAfter, so the next message starts a new one. Its messages are too old
// for Telegram to delete, so they are no longer tracked.
func (c *Chat) rollOver() {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.currentLocked()
	if cur == nil || c.o.Now().Sub(cur.used()) < IdleAfter {
		return
	}
	cur.Messages = nil
	c.index.Active = ""
	if err := c.saveLocked(); err != nil {
		c.o.Logger.Warn("saving the chat state failed", "err", err)
	}
}

// begin notes that the owner wrote text to the agent. It returns the
// conversation the message belongs to, starting one if there is none, and
// the conversations pruned to make room, which the caller deletes.
func (c *Chat) begin(text string) (id string, victims []session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.currentLocked()
	if s == nil {
		c.newLocked()
		victims = c.pruneLocked()
		s = c.currentLocked() // pruning moves the sessions in the list
	}
	if s.Title == "" {
		s.Title = title(text)
	}
	s.LastUsed = c.o.Now().UTC()
	if err := c.saveLocked(); err != nil {
		c.o.Logger.Warn("saving the chat state failed", "err", err)
	}
	return s.ID, victims
}

// newLocked starts a conversation and makes it the active one. It takes
// over the loose messages on the screen.
func (c *Chat) newLocked() {
	now := c.o.Now().UTC()
	c.index.Sessions = append(c.index.Sessions, session{ID: newID(), Started: now, LastUsed: now})
	s := &c.index.Sessions[len(c.index.Sessions)-1]
	c.index.Active = s.ID
	s.Messages, c.index.Loose = c.index.Loose, nil
}

// currentLocked is the active conversation, or nil when there is none.
func (c *Chat) currentLocked() *session {
	for i := range c.index.Sessions {
		if c.index.Sessions[i].ID == c.index.Active {
			return &c.index.Sessions[i]
		}
	}
	return nil
}

// screenLocked takes the ids on the owner's screen out of the index: the
// active conversation's messages and the loose ones.
func (c *Chat) screenLocked() []int64 {
	ids := slices.Clone(c.index.Loose)
	c.index.Loose = nil
	if cur := c.currentLocked(); cur != nil {
		ids = append(ids, cur.Messages...)
		cur.Messages = nil
	}
	return ids
}

// leave clears the screen, which is everything the owner and the bot said in
// the active conversation, and leaves the conversation: the next message
// starts a new one. The conversation stays and can be opened again with
// /sessions. With settle it is also marked settled, and the note names it.
func (c *Chat) leave(ctx context.Context, cmdMessage int64, settle bool) {
	c.mu.Lock()
	text := "New conversation."
	if settle {
		cur := c.currentLocked()
		if cur == nil {
			c.mu.Unlock()
			c.track(cmdMessage)
			c.say(ctx, "No open conversation to settle.")
			return
		}
		cur.Settled = c.o.Now().UTC()
		text = "Settled: " + cur.label(c.o.Location)
	}
	screen := append(c.screenLocked(), cmdMessage)
	c.index.Active = ""
	if err := c.saveLocked(); err != nil {
		c.o.Logger.Warn("saving the chat state failed", "err", err)
	}
	c.mu.Unlock()

	c.stopRunning()
	if c.clearScreen(ctx, screen) > 0 {
		text += " Telegram only deletes messages from the last 48 hours, so some older ones stay."
	}
	c.say(ctx, text)
}

// offerSettle asks the owner whether to settle conversation id, with a
// button. The words are the bridge's own: the agent only says it is done.
func (c *Chat) offerSettle(ctx context.Context, id string) {
	kb := tgapi.Keyboard{{{Text: "Settle", Data: "x:" + id}}}
	if _, err := c.post(ctx, note("Settle this conversation?"), kb); err != nil {
		c.o.Logger.Warn("sending failed", "err", err)
	}
}

// sessionList sends the kept conversations as buttons: the open ones, then
// the settled ones, each newest first.
func (c *Chat) sessionList(ctx context.Context) {
	c.mu.Lock()
	list := slices.Clone(c.index.Sessions)
	active := c.index.Active
	c.mu.Unlock()
	if len(list) == 0 {
		c.say(ctx, "No conversations yet. Send a message to start one.")
		return
	}
	slices.SortStableFunc(list, func(a, b session) int {
		if sa, sb := c.isSettled(a), c.isSettled(b); sa != sb {
			if sa {
				return 1
			}
			return -1
		}
		return b.used().Compare(a.used())
	})
	var kb tgapi.Keyboard
	for _, s := range list {
		text := s.label(c.o.Location)
		if s.ID == active {
			text = "* " + text
		}
		if c.isSettled(s) {
			text += " - settled"
		}
		kb = append(kb, []tgapi.Button{{Text: text, Data: "s:" + s.ID}})
	}
	if _, err := c.post(ctx, note("Pick a conversation:"), kb); err != nil {
		c.o.Logger.Warn("sending failed", "err", err)
	}
}

// HandlePress handles a session button and a Settle button. It reports
// false for other buttons.
func (c *Chat) HandlePress(ctx context.Context, cb tgapi.CallbackQuery) bool {
	if id, ok := strings.CutPrefix(cb.Data, "x:"); ok {
		c.rollOver()
		if !c.isActive(id) {
			if err := c.o.Bot.Answer(ctx, cb.ID, "That conversation is no longer open."); err != nil {
				c.o.Logger.Warn("answering a button failed", "err", err)
			}
			return true
		}
		if err := c.o.Bot.Answer(ctx, cb.ID, ""); err != nil {
			c.o.Logger.Warn("answering a button failed", "err", err)
		}
		c.leave(ctx, 0, true)
		return true
	}
	id, ok := strings.CutPrefix(cb.Data, "s:")
	if !ok {
		return false
	}
	c.rollOver()
	var listMessage int64
	if cb.Message != nil {
		listMessage = cb.Message.MessageID
	}
	if err := c.o.Bot.Answer(ctx, cb.ID, ""); err != nil {
		c.o.Logger.Warn("answering a button failed", "err", err)
	}
	label, found := c.resume(ctx, id, listMessage)
	if !found {
		c.say(ctx, "That conversation is no longer in the list.")
		return true
	}
	c.say(ctx, "Back in: "+label)
	return true
}

// resume makes conversation id the active one, open again if it was
// settled: it clears the screen and shows the conversation's earlier
// messages. It reports whether id exists.
func (c *Chat) resume(ctx context.Context, id string, extra int64) (label string, ok bool) {
	c.mu.Lock()
	i := slices.IndexFunc(c.index.Sessions, func(s session) bool { return s.ID == id })
	if i < 0 {
		c.mu.Unlock()
		return "", false
	}
	if c.index.Active == id {
		c.mu.Unlock()
		return c.index.Sessions[i].label(c.o.Location), true
	}
	screen := append(c.screenLocked(), extra)
	target := &c.index.Sessions[i]
	target.Messages = nil
	target.Settled = time.Time{}
	target.LastUsed = c.o.Now().UTC()
	c.index.Active = id
	label = target.label(c.o.Location)
	if err := c.saveLocked(); err != nil {
		c.o.Logger.Warn("saving the chat state failed", "err", err)
	}
	c.mu.Unlock()

	c.stopRunning()
	c.clearScreen(ctx, screen)
	c.replay(ctx, id)
	return label, true
}
