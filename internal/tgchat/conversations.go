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

	"github.com/mhmdxsadk/latch/internal/tgapi"
	"github.com/mhmdxsadk/latch/internal/tgapprove"
)

// A conversation is a Docker Agent session plus what this package keeps for
// it: the messages on the owner's screen (so /new can clear them) and a
// transcript (so resuming can show them again). The screen always belongs to
// the active conversation. Only the most recent Keep conversations are kept;
// older ones are deleted, database and transcript included.

const (
	// DefaultKeep is how many conversations are kept.
	DefaultKeep = 5
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

// dbFor is the Docker Agent database of s: its own file, or "" for the
// agent's shared default database (conversations from before this existed,
// or when no session directory is set).
func (c *Chat) dbFor(s session) string {
	if s.Own && c.o.SessionDir != "" {
		return filepath.Join(c.o.SessionDir, s.ID+".db")
	}
	return ""
}

// track remembers messages on the owner's screen as part of the active
// conversation, so /new can delete them.
func (c *Chat) track(ids ...int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Before the first conversation the messages are loose; the first
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
		c.o.Logger.Warn("saving the session index failed", "err", err)
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

// stopRunning stops the running turn and drops the waiting ones. It returns
// how many were dropped and whether a turn was running.
func (c *Chat) stopRunning() (dropped int, running bool) {
drain:
	for {
		select {
		case <-c.queue:
			dropped++
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

// appendHistory adds a line to conversation id's transcript.
func (c *Chat) appendHistory(id, from, text string) {
	p := c.historyPath(id)
	if p == "" || strings.TrimSpace(text) == "" {
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

func (c *Chat) hasHistory(id string) bool {
	c.hmu.Lock()
	defer c.hmu.Unlock()
	return len(c.loadHistory(id)) > 0
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

// pruneLocked removes the conversations beyond Keep, oldest first, never
// the active one, and returns them so their files can be deleted.
func (c *Chat) pruneLocked() []session {
	keep := c.o.Keep
	var victims []session
	for len(c.index.Sessions) > keep {
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

// discard deletes the files of conversations that were pruned.
func (c *Chat) discard(ctx context.Context, victims []session) {
	for _, s := range victims {
		c.removeDB(ctx, c.dbFor(s))
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

// --- /new and switching ---

// newConversation starts a conversation: it clears the screen, which is
// everything the owner and the bot said in the old one (the old one stays
// and can be resumed), and deletes conversations beyond the ones kept. An
// active conversation that was never used is reused instead of piling up
// empty ones.
func (c *Chat) newConversation(ctx context.Context, name string, cmdMessage int64) {
	c.mu.Lock()
	screen := append(c.screenLocked(), cmdMessage)
	var victims []session
	if cur := c.currentLocked(); cur != nil && cur.Title == "" && !c.hasHistory(cur.ID) {
		cur.Title = name
	} else {
		c.newLocked(name)
		victims = c.pruneLocked()
	}
	if err := c.saveLocked(); err != nil {
		c.o.Logger.Warn("saving the session index failed", "err", err)
	}
	c.mu.Unlock()

	c.stopRunning()
	c.discard(ctx, victims)
	failed := c.clearScreen(ctx, screen)
	text := "New conversation."
	if name != "" {
		text = "New conversation: " + name
	}
	if failed > 0 {
		text += " Telegram only deletes messages from the last 48 hours, so some older ones stay."
	}
	c.say(ctx, text)
}

// resume makes conversation id the active one: it clears the screen and
// shows the conversation's earlier messages. It reports whether id exists.
func (c *Chat) resume(ctx context.Context, id string, extra int64) (label string, ok bool) {
	c.mu.Lock()
	i := slices.IndexFunc(c.index.Sessions, func(s session) bool { return s.ID == id })
	if i < 0 {
		c.mu.Unlock()
		return "", false
	}
	if cur := c.currentLocked(); cur != nil && cur.ID == id {
		c.mu.Unlock()
		return c.index.Sessions[i].label(c.o.Location), true
	}
	screen := append(c.screenLocked(), extra)
	target := &c.index.Sessions[i]
	target.Messages = nil
	target.LastUsed = c.o.Now().UTC()
	c.index.Active = id
	label = target.label(c.o.Location)
	if err := c.saveLocked(); err != nil {
		c.o.Logger.Warn("saving the session index failed", "err", err)
	}
	c.mu.Unlock()

	c.stopRunning()
	c.clearScreen(ctx, screen)
	c.replay(ctx, id)
	return label, true
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

// currentLocked is the active conversation, or nil when there is none yet.
func (c *Chat) currentLocked() *session {
	for i := range c.index.Sessions {
		if c.index.Sessions[i].ID == c.index.Active {
			return &c.index.Sessions[i]
		}
	}
	return nil
}
