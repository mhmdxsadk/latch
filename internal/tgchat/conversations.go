package tgchat

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/mhmdxsadk/latch/internal/tgapi"
)

// There is one conversation at a time: a Docker Agent session with its own
// database file, plus the messages on the owner's screen, so /new can delete
// them and the old conversation's database with them.

// maxScreen bounds the message ids remembered.
const maxScreen = 2000

// dbFor is the Docker Agent database of conversation id: its own file, or ""
// for the agent's shared default database (when no session directory is set).
func (c *Chat) dbFor(id string) string {
	if c.o.SessionDir == "" || id == "" {
		return ""
	}
	return filepath.Join(c.o.SessionDir, id+".db")
}

// track remembers messages on the owner's screen, so /new can delete them.
func (c *Chat) track(ids ...int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		if id != 0 {
			c.index.Messages = append(c.index.Messages, id)
		}
	}
	if over := len(c.index.Messages) - maxScreen; over > 0 {
		c.index.Messages = slices.Clone(c.index.Messages[over:])
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
	return c.index.Conversation == id
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

// newConversation clears the screen, which is everything the owner and the
// bot said, deletes the old conversation's database and starts over. The
// next message begins a fresh conversation.
func (c *Chat) newConversation(ctx context.Context, cmdMessage int64) {
	c.endConversation(ctx, cmdMessage, "New conversation.", false)
}

// endConversation is newConversation with its own closing note. The note
// stays on screen, so the chat never ends up empty and keeps its place in the
// owner's chat list; silent sends it without a notification sound.
func (c *Chat) endConversation(ctx context.Context, cmdMessage int64, text string, silent bool) {
	c.mu.Lock()
	screen := slices.Clone(c.index.Messages)
	if cmdMessage != 0 {
		screen = append(screen, cmdMessage)
	}
	old := c.index.Conversation
	c.index.Messages, c.index.Conversation = nil, ""
	c.index.LastActive, c.index.KeepUntil, c.index.Note = time.Time{}, time.Time{}, 0
	if err := c.saveLocked(); err != nil {
		c.o.Logger.Warn("saving the chat state failed", "err", err)
	}
	c.mu.Unlock()

	c.stopRunning()
	c.removeDB(ctx, c.dbFor(old))
	if c.clearScreen(ctx, screen) > 0 {
		text += " Telegram only deletes messages from the last 48 hours, so some older ones stay."
	}
	if silent {
		ctx = tgapi.Silent(ctx)
	}
	m, err := c.post(ctx, note(text), nil)
	if err != nil {
		c.o.Logger.Warn("sending failed", "err", err)
		return
	}
	c.mu.Lock()
	c.index.Note = m.MessageID
	if err := c.saveLocked(); err != nil {
		c.o.Logger.Warn("saving the chat state failed", "err", err)
	}
	c.mu.Unlock()
}

// ResetEnabled reports whether conversations end by themselves.
func (c *Chat) ResetEnabled() bool { return c.o.ResetAfter > 0 }

// touch notes that the owner just wrote, which restarts the reset clock.
func (c *Chat) touch() {
	if !c.ResetEnabled() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.index.LastActive = c.o.Now()
	if err := c.saveLocked(); err != nil {
		c.o.Logger.Warn("saving the chat state failed", "err", err)
	}
}

// keep postpones the automatic reset by ResetAfter from now.
func (c *Chat) keep(ctx context.Context) {
	if !c.ResetEnabled() {
		c.say(ctx, "Conversations are not cleared automatically (agent.reset_after in telegram.yaml).")
		return
	}
	until := c.o.Now().Add(c.o.ResetAfter)
	c.mu.Lock()
	c.index.KeepUntil = until
	if err := c.saveLocked(); err != nil {
		c.o.Logger.Warn("saving the chat state failed", "err", err)
	}
	c.mu.Unlock()
	var b tgapi.Builder
	b.Italic("Keeping this conversation until ")
	b.Time(until.In(c.o.Location).Format("Mon 15:04"), until, "wt")
	b.Italic(". Send /keep again to extend.")
	if _, err := c.post(ctx, b.Done(), nil); err != nil {
		c.o.Logger.Warn("sending failed", "err", err)
	}
}

// resetPoll is how often the reset clock is checked.
const resetPoll = time.Minute

func (c *Chat) resetLoop(ctx context.Context) {
	tick := time.NewTicker(resetPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			c.maybeReset(ctx)
		}
	}
}

// maybeReset ends the conversation when ResetAfter has passed since the
// owner's last message (and since any /keep). It waits while an answer is
// running or queued, and does nothing when the only thing on screen is the
// note from the last reset.
func (c *Chat) maybeReset(ctx context.Context) {
	if !c.ResetEnabled() {
		return
	}
	now := c.o.Now()
	c.mu.Lock()
	if c.cancel != nil || len(c.queue) > 0 {
		c.mu.Unlock()
		return
	}
	has := c.index.Conversation != ""
	for _, id := range c.index.Messages {
		if id != c.index.Note {
			has = true
			break
		}
	}
	if !has {
		c.mu.Unlock()
		return
	}
	if c.index.LastActive.IsZero() {
		// Messages with no owner message yet (for example a check report):
		// the clock starts now.
		c.index.LastActive = now
		if err := c.saveLocked(); err != nil {
			c.o.Logger.Warn("saving the chat state failed", "err", err)
		}
		c.mu.Unlock()
		return
	}
	deadline := c.index.LastActive.Add(c.o.ResetAfter)
	if k := c.index.KeepUntil; k.After(deadline) {
		deadline = k
	}
	c.mu.Unlock()
	if now.Before(deadline) {
		return
	}
	c.endConversation(ctx, 0, "Conversation ended after "+hours(c.o.ResetAfter)+".", true)
}

// hours words d as hours when it is a whole number of them.
func hours(d time.Duration) string {
	switch {
	case d%time.Hour == 0 && d == time.Hour:
		return "1 hour"
	case d%time.Hour == 0:
		return fmt.Sprintf("%d hours", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%d minutes", d/time.Minute)
	}
	return d.String()
}

// activeLocked is the current conversation's id, starting a conversation if
// there is none.
func (c *Chat) activeLocked() string {
	if c.index.Conversation == "" {
		c.index.Conversation = newID()
	}
	return c.index.Conversation
}
