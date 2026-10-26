// Package tgchat lets the owner talk to the agent team from Telegram.
//
// Each Telegram message becomes one agent turn in the active session. The
// conversation itself lives in Docker Agent's session database; this package
// only keeps an index of sessions (id, title, start time) so the owner can
// switch between them without ever seeing an id. Turns run one at a time, in
// order. Agent answers are untrusted text: they are cleaned of control
// characters and their Markdown becomes formatting this package computes
// (never links). Answers reply to the owner's message. Latch marks its own
// messages with tgapi.Mark, which is removed from agent text, so an agent
// cannot pose as Latch.
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
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/mhmdxsadk/latch/internal/textsafe"
	"github.com/mhmdxsadk/latch/internal/tgapi"
	"github.com/mhmdxsadk/latch/internal/tgapprove"
)

// Bot is the part of the Telegram client the chat uses.
type Bot interface {
	Send(ctx context.Context, chat int64, t tgapi.Text, kb tgapi.Keyboard) (tgapi.Message, error)
	Edit(ctx context.Context, chat, message int64, t tgapi.Text, kb tgapi.Keyboard) error
	Answer(ctx context.Context, callbackID, text string) error
	Typing(ctx context.Context, chat int64) error
	Reply(ctx context.Context, chat, replyTo int64, t tgapi.Text) (tgapi.Message, error)
	Draft(ctx context.Context, chat, draftID int64, t tgapi.Text, canStop bool) error
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
	{Command: "stop", Description: "Stop the running answer and drop waiting messages"},
	{Command: "check", Description: "Check the server now"},
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
	Name    string
	Timeout time.Duration
	// Location is the owner's time zone for conversation names.
	Location *time.Location
	// Host names the server in the welcome text.
	Host string
	// CheckRunner runs scheduled checks every CheckEvery (0: none). It must
	// not be able to change anything.
	CheckRunner Runner
	CheckEvery  time.Duration
	MaxQueue    int
	Now         func() time.Time
	Logger      *slog.Logger
}

// Chat runs agent turns for the owner's messages.
type Chat struct {
	o     Options
	queue chan job

	mu     sync.Mutex
	index  index
	cancel context.CancelFunc // of the running turn, nil when idle
	draft  int64              // live preview id of the running turn

	drafts atomic.Int64 // last draft id handed out
}

type job struct {
	session string
	text    string
	note    string // shown to the owner for a message the bridge sends
	// messageID is the owner's message, answered as a reply; 0 for
	// messages the bridge sends.
	messageID int64
	// check is a scheduled check; manual when the owner asked for it.
	check, manual bool
}

type index struct {
	Active   string    `json:"active"`
	Sessions []session `json:"sessions"`
	// LastCheck and Reported belong to the scheduled checks.
	LastCheck time.Time `json:"last_check,omitzero"`
	Reported  []report  `json:"reported,omitempty"`
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
	if o.Location == nil {
		o.Location = time.Local
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	c := &Chat{o: o, queue: make(chan job, o.MaxQueue)}
	// Draft ids must be non-zero; start from the clock so a restarted
	// bridge does not reuse one Telegram still animates.
	c.drafts.Store(time.Now().UnixMilli() % 1_000_000_000)
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
	if c.o.CheckEvery > 0 && c.o.CheckRunner != nil {
		c.mu.Lock()
		if c.index.LastCheck.IsZero() {
			c.index.LastCheck = c.o.Now().UTC() // the first check comes one interval after setup
		}
		c.mu.Unlock()
		go c.checkLoop(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-c.queue:
			if j.check {
				c.check(ctx, j.manual)
			} else {
				c.turn(ctx, j)
			}
		}
	}
}

// HandleText handles a message from the owner: a command, or a message for
// the agent. messageID is the owner's message, which the answer replies to.
func (c *Chat) HandleText(ctx context.Context, text string, messageID int64) {
	if cmd, arg, ok := command(text); ok {
		c.handleCommand(ctx, cmd, arg)
		return
	}
	if len(text) > maxMessage {
		c.say(ctx, "That message is too long; keep it under 8 KB.")
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
	c.enqueue(ctx, job{session: id, text: text, messageID: messageID})
}

// Notify sends text to the agent in the active session as an automatic
// message, for example the outcome of a request the owner decided, and shows
// the owner note instead of the whole text. The agent's answer replies to
// message replyTo (the request's message; 0 for none). It never blocks;
// when the queue is full the message is dropped and logged.
func (c *Chat) Notify(text, note string, replyTo int64) {
	c.mu.Lock()
	id := c.index.Active
	c.mu.Unlock()
	if id == "" {
		return // no conversation yet: nobody to tell
	}
	select {
	case c.queue <- job{session: id, text: text, note: note, messageID: replyTo}:
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
		label = c.index.Sessions[i].label(c.o.Location)
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
		_ = c.o.Bot.Edit(ctx, c.o.Chat, cb.Message.MessageID, note("Now in: "+label), nil)
	}
	return true
}

func (c *Chat) handleCommand(ctx context.Context, cmd, arg string) {
	switch cmd {
	case "start", "help":
		c.plain(ctx, c.welcome())
	case "new":
		c.mu.Lock()
		s := c.newLocked(title(arg))
		err := c.saveLocked()
		c.mu.Unlock()
		if err != nil {
			c.o.Logger.Warn("saving the session index failed", "err", err)
		}
		if s.Title == "" {
			c.say(ctx, "New conversation.")
		} else {
			c.say(ctx, "New conversation: "+s.Title)
		}
	case "check":
		if c.o.CheckRunner == nil {
			c.say(ctx, "Checks are not set up (agent.checks in telegram.yaml).")
			return
		}
		select {
		case c.queue <- job{check: true, manual: true}:
		default:
			c.say(ctx, "Too many messages waiting. Wait for an answer or use /stop.")
		}
	case "rename":
		if strings.TrimSpace(arg) == "" {
			c.say(ctx, "Usage: /rename name")
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
		c.say(ctx, "Renamed to: "+s.label(c.o.Location))
	case "sessions":
		c.mu.Lock()
		list := slices.Clone(c.index.Sessions)
		active := c.index.Active
		c.mu.Unlock()
		if len(list) == 0 {
			c.say(ctx, "No conversations yet. Send a message to start one.")
			return
		}
		slices.Reverse(list) // newest first
		var kb tgapi.Keyboard
		for _, s := range list[:min(len(list), shownSessions)] {
			text := s.label(c.o.Location)
			if s.ID == active {
				text = "* " + text
			}
			kb = append(kb, []tgapi.Button{{Text: text, Data: "s:" + s.ID}})
		}
		if _, err := c.o.Bot.Send(ctx, c.o.Chat, note("Pick a conversation:"), kb); err != nil {
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
			c.say(ctx, fmt.Sprintf("Stopping %s (dropped %d waiting).", c.o.Name, dropped))
		default:
			c.say(ctx, fmt.Sprintf("Nothing running (dropped %d waiting).", dropped))
		}
	default:
		c.say(ctx, "Unknown command. Try /help.")
	}
}

func (c *Chat) enqueue(ctx context.Context, j job) {
	select {
	case c.queue <- j:
	default:
		c.say(ctx, "Too many messages waiting. Wait for an answer or use /stop.")
	}
}

// StopDraft stops the running turn when id is its live preview, after the
// owner pressed Stop under it. It reports whether it stopped something.
func (c *Chat) StopDraft(id int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel == nil || c.draft != id {
		return false
	}
	c.cancel()
	return true
}

// turn runs one agent turn, shows it live and sends the answer.
func (c *Chat) turn(parent context.Context, j job) {
	ctx, cancel := context.WithTimeout(parent, c.o.Timeout)
	defer cancel()
	stopped := make(chan struct{})
	var once sync.Once
	draftID := c.drafts.Add(1)
	c.mu.Lock()
	c.cancel = func() { once.Do(func() { close(stopped); cancel() }) }
	c.draft = draftID
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.cancel, c.draft = nil, 0
		c.mu.Unlock()
	}()
	if j.note != "" {
		c.say(parent, j.note)
	}

	lv := c.startLive(parent, draftID)
	out, err := c.o.Runner.Run(ctx, j.session, j.text, lv.update)
	lv.stop()
	c.mu.Lock()
	c.cancel, c.draft = nil, 0 // idle again before any message goes out
	c.mu.Unlock()

	if parent.Err() != nil {
		return
	}
	select {
	case <-stopped:
		c.sayTo(parent, j.messageID, "Stopped.")
		return
	default:
	}
	var re *RunError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		c.sayTo(parent, j.messageID, fmt.Sprintf("%s did not finish within %s and was stopped.", c.o.Name, c.o.Timeout))
		return
	case errors.As(err, &re):
		c.sayTo(parent, j.messageID, fmt.Sprintf("%s failed (%s).", c.o.Name, re.Error()))
		return
	case err != nil:
		c.o.Logger.Warn("agent run failed", "err", err)
		c.sayTo(parent, j.messageID, "Could not start "+c.o.Name+"; see the latch-telegram log.")
		return
	}
	c.answer(parent, out, j.messageID)
}

// live shows a turn as it happens: a draft with a Stop button that grows
// with the answer, or the typing indicator when drafts do not work.
type live struct {
	c   *Chat
	ctx context.Context
	id  int64

	mu      sync.Mutex
	p       Progress
	dirty   bool
	done    chan struct{}
	stopped chan struct{}
}

// draftEvery is how often the preview is updated; drafts are previews of
// about 30 seconds, so an unchanged one is sent again every draftRefresh.
const (
	draftEvery   = 800 * time.Millisecond
	draftRefresh = 20 * time.Second
)

func (c *Chat) startLive(ctx context.Context, id int64) *live {
	lv := &live{c: c, ctx: ctx, id: id, done: make(chan struct{}), stopped: make(chan struct{})}
	go lv.loop()
	return lv
}

func (lv *live) update(p Progress) {
	lv.mu.Lock()
	lv.p, lv.dirty = p, true
	lv.mu.Unlock()
}

func (lv *live) stop() {
	close(lv.done)
	<-lv.stopped
}

func (lv *live) loop() {
	defer close(lv.stopped)
	c := lv.c
	// Until there is something to show, only Telegram's own "typing...".
	// The draft starts with the first tool or the first words of the
	// answer, so a quick reply to "hello" is just a reply.
	_ = c.o.Bot.Typing(lv.ctx, c.o.Chat)
	tick := time.NewTicker(draftEvery)
	defer tick.Stop()
	drafts, shown := true, false
	last := time.Now()
	for {
		select {
		case <-lv.done:
			return
		case <-tick.C:
		}
		lv.mu.Lock()
		p, dirty := lv.p, lv.dirty
		lv.dirty = false
		lv.mu.Unlock()
		empty := strings.TrimSpace(p.Text) == "" && p.Status == ""
		if !drafts || empty {
			if time.Since(last) >= 4*time.Second {
				_ = c.o.Bot.Typing(lv.ctx, c.o.Chat)
				last = time.Now()
			}
			continue
		}
		if !dirty && shown && time.Since(last) < draftRefresh {
			continue
		}
		if err := c.o.Bot.Draft(lv.ctx, c.o.Chat, lv.id, c.preview(p), true); err != nil {
			c.o.Logger.Debug("drafts unavailable; showing typing instead", "err", err)
			drafts = false
			continue
		}
		shown, last = true, time.Now()
	}
}

// preview is the draft for p: the answer so far, or what the agent is doing.
func (c *Chat) preview(p Progress) tgapi.Text {
	var b tgapi.Builder
	if strings.TrimSpace(p.Text) == "" {
		return b.Italic(p.Status + "...").Done()
	}
	body := Markdown(p.Text)
	if body.Len() > tgapi.MaxText-b.Len()-8 {
		body = tgapi.SplitText(body, tgapi.MaxText-b.Len()-8)[0]
	}
	return b.Text(body).Done()
}

// answer sends the agent's answer, formatted and split.
func (c *Chat) answer(ctx context.Context, out string, replyTo int64) {
	body := Markdown(out)
	if strings.TrimSpace(body.S) == "" {
		body = note("(no answer)")
	}
	for _, part := range tgapi.SplitText(body, tgapi.MaxText) {
		if err := c.send(ctx, replyTo, part); err != nil {
			c.o.Logger.Warn("sending an answer failed", "err", err)
			return
		}
		replyTo = 0 // only the first part is a reply
	}
}

// say sends a short note from the bridge, in italics.
func (c *Chat) say(ctx context.Context, text string) {
	if _, err := c.o.Bot.Send(ctx, c.o.Chat, note(text), nil); err != nil {
		c.o.Logger.Warn("sending failed", "err", err)
	}
}

// welcome is the /start and /help text.
func (c *Chat) welcome() string {
	where := "this server"
	if c.o.Host != "" {
		where = c.o.Host
	}
	return "Ask me anything about " + where + ", or tell me what to fix. Changes wait until you tap Approve.\n\n" +
		"/new starts over · /sessions goes back to an earlier chat · /stop stops an answer"
}

// plain sends text without the italics of a note.
func (c *Chat) plain(ctx context.Context, text string) {
	if _, err := c.o.Bot.Send(ctx, c.o.Chat, tgapi.Plain(text), nil); err != nil {
		c.o.Logger.Warn("sending failed", "err", err)
	}
}

// sayTo sends a note as a reply to the owner's message replyTo (0: none).
func (c *Chat) sayTo(ctx context.Context, replyTo int64, text string) {
	if err := c.send(ctx, replyTo, note(text)); err != nil {
		c.o.Logger.Warn("sending failed", "err", err)
	}
}

func (c *Chat) send(ctx context.Context, replyTo int64, t tgapi.Text) error {
	var err error
	if replyTo != 0 {
		_, err = c.o.Bot.Reply(ctx, c.o.Chat, replyTo, t)
	} else {
		_, err = c.o.Bot.Send(ctx, c.o.Chat, t, nil)
	}
	return err
}

func note(s string) tgapi.Text {
	var b tgapi.Builder
	return b.Italic(s).Done()
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
