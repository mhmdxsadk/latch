// Package tgchat lets the owner talk to the agent team from Telegram.
//
// Each Telegram message becomes one agent turn in the active session. The
// conversation itself lives in Docker Agent's session database; this package
// only keeps an index of sessions (id, title, start time) so the owner can
// switch between them without ever seeing an id. Turns run one at a time, in
// order. Agent answers are untrusted text: they are sent as plain text,
// cleaned of control characters and always start with the agent's name, so
// they cannot pose as a message from Latch.
package tgchat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mhmdxsadk/latch/internal/textsafe"
	"github.com/mhmdxsadk/latch/internal/tgapi"
	"github.com/mhmdxsadk/latch/internal/tgapprove"
)

// Bot is the part of the Telegram client the chat uses.
type Bot interface {
	Send(ctx context.Context, chat int64, text string, kb tgapi.Keyboard) (tgapi.Message, error)
	Edit(ctx context.Context, chat, message int64, text string, kb tgapi.Keyboard) error
	Answer(ctx context.Context, callbackID, text string) error
	Typing(ctx context.Context, chat int64) error
}

// Defaults for Options.
const (
	DefaultTimeout  = 10 * time.Minute
	DefaultMaxQueue = 10
	DefaultName     = "agent"
	titleLen        = 40
	shownSessions   = 10
	maxMessage      = 8 << 10
)

// Commands is the bot's command menu.
var Commands = []tgapi.Command{
	{Command: "new", Description: "Start a new conversation: /new [name]"},
	{Command: "sessions", Description: "Switch to an earlier conversation"},
	{Command: "rename", Description: "Rename this conversation: /rename name"},
	{Command: "stop", Description: "Stop the running answer and drop queued messages"},
	{Command: "help", Description: "What this bot does"},
}

// Options configure a Chat.
type Options struct {
	Bot    Bot
	Chat   int64
	Runner Runner
	// IndexPath is a root-only file holding the session index.
	IndexPath string
	// Name labels the agent's answers, for example "keeper".
	Name     string
	Timeout  time.Duration
	MaxQueue int
	Now      func() time.Time
	Logger   *slog.Logger
}

// Chat runs agent turns for the owner's messages.
type Chat struct {
	o     Options
	queue chan job

	mu     sync.Mutex
	index  index
	cancel context.CancelFunc // of the running turn, nil when idle
}

type job struct {
	session string
	text    string
	auto    bool // sent by the bridge, not typed by the owner
}

type index struct {
	Active   string    `json:"active"`
	Sessions []session `json:"sessions"`
}

type session struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Started time.Time `json:"started"`
}

// New loads the session index (a missing file is an empty index).
func New(o Options) (*Chat, error) {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.MaxQueue <= 0 {
		o.MaxQueue = DefaultMaxQueue
	}
	if o.Name == "" {
		o.Name = DefaultName
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	c := &Chat{o: o, queue: make(chan job, o.MaxQueue)}
	b, err := os.ReadFile(o.IndexPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &c.index); err != nil {
			return nil, fmt.Errorf("%s: %w", o.IndexPath, err)
		}
	}
	return c, nil
}

// Run runs queued turns until ctx ends.
func (c *Chat) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-c.queue:
			c.turn(ctx, j)
		}
	}
}

// HandleText handles a message from the owner: a command, or a message for
// the agent.
func (c *Chat) HandleText(ctx context.Context, text string) {
	if cmd, arg, ok := command(text); ok {
		c.handleCommand(ctx, cmd, arg)
		return
	}
	if len(text) > maxMessage {
		c.say(ctx, "Latch: that message is too long; keep it under 8 KB.")
		return
	}
	c.mu.Lock()
	s := c.activeLocked()
	if s.Title == "" {
		s.Title = title(text)
	}
	id := s.ID
	err := c.saveLocked()
	c.mu.Unlock()
	if err != nil {
		c.o.Logger.Warn("saving the session index failed", "err", err)
	}
	c.enqueue(ctx, job{session: id, text: text})
}

// Notify sends text to the agent in the active session as an automatic
// message, for example the outcome of a request the owner decided. It never
// blocks; when the queue is full the message is dropped and logged.
func (c *Chat) Notify(text string) {
	c.mu.Lock()
	id := c.index.Active
	c.mu.Unlock()
	if id == "" {
		return // no conversation yet: nobody to tell
	}
	select {
	case c.queue <- job{session: id, text: text, auto: true}:
	default:
		c.o.Logger.Warn("queue full; dropped an automatic message")
	}
}

// HandlePress handles a session button. It reports false for other buttons.
func (c *Chat) HandlePress(ctx context.Context, cb tgapi.CallbackQuery) bool {
	id, ok := strings.CutPrefix(cb.Data, "s:")
	if !ok {
		return false
	}
	c.mu.Lock()
	i := slices.IndexFunc(c.index.Sessions, func(s session) bool { return s.ID == id })
	answer, label := "That conversation is no longer in the list.", ""
	if i >= 0 {
		c.index.Active = id
		label = c.index.Sessions[i].label()
		answer = "Switched to: " + label
		if err := c.saveLocked(); err != nil {
			c.o.Logger.Warn("saving the session index failed", "err", err)
		}
	}
	c.mu.Unlock()
	if err := c.o.Bot.Answer(ctx, cb.ID, answer); err != nil {
		c.o.Logger.Warn("answering a button failed", "err", err)
	}
	if i >= 0 && cb.Message != nil {
		_ = c.o.Bot.Edit(ctx, c.o.Chat, cb.Message.MessageID, "Latch: now in "+label, nil)
	}
	return true
}

func (c *Chat) handleCommand(ctx context.Context, cmd, arg string) {
	switch cmd {
	case "start", "help":
		c.say(ctx, "Latch: send a message and "+c.o.Name+" answers it. Changes it asks for show up here with Approve and Deny buttons.\n"+
			"/new [name] starts a new conversation, /sessions switches, /rename names this one, /stop stops the running answer.")
	case "new":
		c.mu.Lock()
		s := c.newLocked(title(arg))
		err := c.saveLocked()
		c.mu.Unlock()
		if err != nil {
			c.o.Logger.Warn("saving the session index failed", "err", err)
		}
		c.say(ctx, "Latch: new conversation: "+s.label())
	case "rename":
		if strings.TrimSpace(arg) == "" {
			c.say(ctx, "Latch: usage: /rename name")
			return
		}
		c.mu.Lock()
		s := c.activeLocked()
		s.Title = title(arg)
		err := c.saveLocked()
		c.mu.Unlock()
		if err != nil {
			c.o.Logger.Warn("saving the session index failed", "err", err)
		}
		c.say(ctx, "Latch: renamed to: "+s.label())
	case "sessions":
		c.mu.Lock()
		list := slices.Clone(c.index.Sessions)
		active := c.index.Active
		c.mu.Unlock()
		if len(list) == 0 {
			c.say(ctx, "Latch: no conversations yet. Send a message to start one.")
			return
		}
		slices.Reverse(list) // newest first
		var kb tgapi.Keyboard
		for _, s := range list[:min(len(list), shownSessions)] {
			text := s.label()
			if s.ID == active {
				text = "* " + text
			}
			kb = append(kb, []tgapi.Button{{Text: text, Data: "s:" + s.ID}})
		}
		if _, err := c.o.Bot.Send(ctx, c.o.Chat, "Latch: pick a conversation", kb); err != nil {
			c.o.Logger.Warn("sending failed", "err", err)
		}
	case "stop":
		dropped := 0
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
		running := c.cancel != nil
		if running {
			c.cancel()
		}
		c.mu.Unlock()
		switch {
		case running:
			c.say(ctx, fmt.Sprintf("Latch: stopping %s (dropped %d queued).", c.o.Name, dropped))
		default:
			c.say(ctx, fmt.Sprintf("Latch: nothing running (dropped %d queued).", dropped))
		}
	default:
		c.say(ctx, "Latch: unknown command. Try /help.")
	}
}

func (c *Chat) enqueue(ctx context.Context, j job) {
	select {
	case c.queue <- j:
		c.mu.Lock()
		busy := c.cancel != nil
		c.mu.Unlock()
		if busy {
			c.say(ctx, "Latch: queued; "+c.o.Name+" is still answering.")
		}
	default:
		c.say(ctx, "Latch: too many messages waiting. Wait for an answer or use /stop.")
	}
}

// turn runs one agent turn and sends the answer.
func (c *Chat) turn(parent context.Context, j job) {
	ctx, cancel := context.WithTimeout(parent, c.o.Timeout)
	defer cancel()
	stopped := make(chan struct{})
	var once sync.Once
	c.mu.Lock()
	c.cancel = func() { once.Do(func() { close(stopped); cancel() }) }
	c.mu.Unlock()
	if j.auto {
		c.say(parent, "Latch: told "+c.o.Name+": "+j.text)
	}
	defer func() {
		c.mu.Lock()
		c.cancel = nil
		c.mu.Unlock()
	}()

	typing := make(chan struct{})
	go func() {
		t := time.NewTicker(4 * time.Second)
		defer t.Stop()
		for {
			_ = c.o.Bot.Typing(parent, c.o.Chat)
			select {
			case <-typing:
				return
			case <-t.C:
			}
		}
	}()
	out, err := c.o.Runner.Run(ctx, j.session, j.text)
	close(typing)
	c.mu.Lock()
	c.cancel = nil // idle again before any message goes out
	c.mu.Unlock()

	if parent.Err() != nil {
		return
	}
	select {
	case <-stopped:
		c.say(parent, "Latch: stopped.")
		return
	default:
	}
	var re *RunError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		c.say(parent, fmt.Sprintf("Latch: %s did not finish within %s and was stopped.", c.o.Name, c.o.Timeout))
		return
	case errors.As(err, &re):
		c.say(parent, fmt.Sprintf("Latch: %s failed (%s).", c.o.Name, re.Error()))
		return
	case err != nil:
		c.o.Logger.Warn("agent run failed", "err", err)
		c.say(parent, "Latch: could not start "+c.o.Name+"; see the latch-telegram log.")
		return
	}
	c.answer(parent, out)
}

// answer sends the agent's answer, cleaned and split, each part labelled.
func (c *Chat) answer(ctx context.Context, out string) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i, l := range lines {
		lines[i] = textsafe.Line(l)
	}
	text := strings.TrimSpace(strings.Join(lines, "\n"))
	if text == "" {
		text = "(no answer)"
	}
	prefix := c.o.Name + ":\n"
	for _, part := range tgapi.Split(text, tgapi.MaxText-tgapi.TextLen(prefix)) {
		if _, err := c.o.Bot.Send(ctx, c.o.Chat, prefix+part, nil); err != nil {
			c.o.Logger.Warn("sending an answer failed", "err", err)
			return
		}
	}
}

func (c *Chat) say(ctx context.Context, text string) {
	if _, err := c.o.Bot.Send(ctx, c.o.Chat, text, nil); err != nil {
		c.o.Logger.Warn("sending failed", "err", err)
	}
}

// activeLocked returns the active session, starting one if there is none.
func (c *Chat) activeLocked() *session {
	for i := range c.index.Sessions {
		if c.index.Sessions[i].ID == c.index.Active {
			return &c.index.Sessions[i]
		}
	}
	return c.newLocked("")
}

func (c *Chat) newLocked(name string) *session {
	c.index.Sessions = append(c.index.Sessions, session{ID: newID(), Title: name, Started: c.o.Now().UTC()})
	s := &c.index.Sessions[len(c.index.Sessions)-1]
	c.index.Active = s.ID
	return s
}

func (c *Chat) saveLocked() error {
	b, err := json.Marshal(c.index)
	if err != nil {
		return err
	}
	return tgapprove.WriteFile(c.o.IndexPath, b)
}

func (s session) label() string {
	t := s.Title
	if t == "" {
		t = "untitled"
	}
	return t + " (" + s.Started.Local().Format("Jan 2 15:04") + ")"
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

// command splits "/name arg" (also "/name@bot arg").
func command(text string) (cmd, arg string, ok bool) {
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	head, arg, _ := strings.Cut(text[1:], " ")
	head, _, _ = strings.Cut(head, "@")
	return strings.ToLower(head), strings.TrimSpace(arg), true
}

// newID returns a random UUID (version 4). Docker Agent creates a session
// with a new id on first use.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
