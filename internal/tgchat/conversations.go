package tgchat

import (
	"context"
	"path/filepath"
	"slices"

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
	c.mu.Lock()
	screen := append(slices.Clone(c.index.Messages), cmdMessage)
	old := c.index.Conversation
	c.index.Messages, c.index.Conversation = nil, ""
	if err := c.saveLocked(); err != nil {
		c.o.Logger.Warn("saving the chat state failed", "err", err)
	}
	c.mu.Unlock()

	c.stopRunning()
	c.removeDB(ctx, c.dbFor(old))
	text := "New conversation."
	if c.clearScreen(ctx, screen) > 0 {
		text += " Telegram only deletes messages from the last 48 hours, so some older ones stay."
	}
	c.say(ctx, text)
}

// activeLocked is the current conversation's id, starting a conversation if
// there is none.
func (c *Chat) activeLocked() string {
	if c.index.Conversation == "" {
		c.index.Conversation = newID()
	}
	return c.index.Conversation
}
