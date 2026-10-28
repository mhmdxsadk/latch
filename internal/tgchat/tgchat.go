// Package tgchat lets the owner talk to the agent team from Telegram.
//
// Each Telegram message becomes one agent turn in the active conversation.
// There is one conversation at a time. It lives in Docker Agent's session
// database, a file of its own; this package keeps the conversation's id and
// the messages on screen, so /new can clear the screen and delete the
// database. Turns run one at a time, in order. Agent answers are untrusted
// text: they are cleaned of control characters and their Markdown becomes
// formatting this package computes
// (never links). A turn that files a request shows the request alone, in one
// message that is shown once and edited in place when it is decided; the
// agent's answer to the decision is added under the outcome. An agent cannot
// pose as Latch where it matters: a request waiting for a decision always
// has buttons, which agent text never gets, and its words never sit next to
// them.
package tgchat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	DeleteMessages(ctx context.Context, chat int64, ids []int64) error
}

// Defaults for Options.
const (
	DefaultTimeout  = 10 * time.Minute
	DefaultMaxQueue = 10
	DefaultName     = "agent"
	maxMessage      = 8 << 10
	// MaxImage bounds a photo the owner sends, in bytes.
	MaxImage = 10 << 20
	// imageText is the message for a photo sent without a caption.
	imageText = "[User attached one or more files without additional text. Respond using the conversation context and the attached files.]"
)

// Commands is the bot's command menu.
var Commands = []tgapi.Command{
	{Command: "new", Description: "Clears the chat and starts a new conversation"},
	{Command: "stop", Description: "Stop the running answer and drop waiting messages"},
	{Command: "check", Description: "Checks the server's current state"},
	{Command: "help", Description: "Shows this message"},
}

// Options configure a Chat.
type Options struct {
	Bot    Bot
	Chat   int64
	Runner Runner
	// IndexPath is a root-only file holding the chat state.
	IndexPath string
	// Name labels the agent's answers, for example "keeper".
	Name    string
	Timeout time.Duration
	// Location is the owner's time zone for conversation names.
	Location *time.Location
	// Host names the server in the welcome text.
	Host string
	// Intro opens the welcome text; empty means a one-line default.
	Intro string
	// CheckRunner runs scheduled checks every CheckEvery (0: none). It must
	// not be able to change anything.
	CheckRunner Runner
	CheckEvery  time.Duration
	// SessionDir is where each conversation's Docker Agent database lives
	// ("" uses the agent's shared database and never deletes anything).
	SessionDir string
	// Remove deletes files in SessionDir, as the agent's account.
	Remove func(ctx context.Context, paths []string) error
	// SaveImage writes a photo where the agent can read it and returns its
	// path (nil: photos are refused). The chat removes it with Remove.
	SaveImage func(ctx context.Context, data []byte, ext string) (string, error)
	// Finished returns the chat messages of requests already decided, for
	// /new to delete too; requests still waiting are never in it.
	Finished func() []int64
	// Filed reports whether the turn that started at since filed a request
	// for the owner: the request is the whole reply then and the agent's
	// words are not shown. Follow adds the agent's answer to a decision under
	// the outcome in the request's own message and reports whether it did.
	// Both may be nil.
	Filed    func(ctx context.Context, since time.Time) bool
	Follow   func(ctx context.Context, message int64, answer tgapi.Text) bool
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
	draft  int64              // live preview id of the running turn

	drafts atomic.Int64 // last draft id handed out
}

type job struct {
	session string
	text    string
	note    string // shown to the owner for a message the bridge sends
	images  []string
	// messageID is the message the answer replies to: the request's message
	// for the outcome of a decision, 0 otherwise.
	messageID int64
	// check is a scheduled check; manual when the owner asked for it.
	check, manual bool
}

type index struct {
	// Conversation is the current one ("" until the first message after
	// /new). Messages are the ids of the messages on the owner's screen.
	Conversation string  `json:"conversation,omitempty"`
	Messages     []int64 `json:"messages,omitempty"`
	// LastCheck and Reported belong to the scheduled checks.
	LastCheck time.Time `json:"last_check,omitzero"`
	Reported  []report  `json:"reported,omitempty"`
}

// New loads the chat state (a missing file is an empty state).
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
// the agent. messageID is the owner's message, only noted for /new to clear;
// the answer is a plain message, not a reply to it.
func (c *Chat) HandleText(ctx context.Context, text string, messageID int64) {
	if cmd, ok := command(text); ok {
		c.handleCommand(ctx, cmd, messageID)
		return
	}
	c.track(messageID) // the owner's message is on screen too
	if len(text) > maxMessage {
		c.say(ctx, "That message is too long; keep it under 8 KB.")
		return
	}
	c.mu.Lock()
	id := c.activeLocked()
	err := c.saveLocked()
	c.mu.Unlock()
	if err != nil {
		c.o.Logger.Warn("saving the chat state failed", "err", err)
	}
	c.enqueue(ctx, job{session: id, text: text})
}

// Notify sends text to the agent in the current conversation as an automatic
// message, for example the outcome of a request the owner decided, and shows
// the owner note instead of the whole text. The agent's answer replies to
// message replyTo (the request's message; 0 for none). It never blocks;
// when the queue is full the message is dropped and logged.
func (c *Chat) Notify(text, note string, replyTo int64) {
	c.mu.Lock()
	id := c.index.Conversation
	c.mu.Unlock()
	if id == "" {
		return // no conversation: nobody to tell
	}
	select {
	case c.queue <- job{session: id, text: text, note: note, messageID: replyTo}:
	default:
		c.o.Logger.Warn("queue full; dropped an automatic message")
	}
}

// HandleImages handles photos from the owner: one message for the agent with
// them attached. caption is the text sent with them ("" for none);
// messageIDs are the owner's messages, noted for /new to clear.
func (c *Chat) HandleImages(ctx context.Context, caption string, images [][]byte, messageIDs []int64) {
	c.track(messageIDs...)
	if c.o.SaveImage == nil {
		c.say(ctx, "Photos are not available; the agent's conversation folder is not set up.")
		return
	}
	if len(caption) > maxMessage {
		c.say(ctx, "That caption is too long; keep it under 8 KB.")
		return
	}
	exts := make([]string, len(images))
	for i, data := range images {
		switch http.DetectContentType(data) {
		case "image/jpeg":
			exts[i] = ".jpg"
		case "image/png":
			exts[i] = ".png"
		default:
			c.say(ctx, "Only JPEG and PNG photos are read.")
			return
		}
	}
	var paths []string
	for i, data := range images {
		path, err := c.o.SaveImage(ctx, data, exts[i])
		if err != nil {
			c.discard(paths)
			c.o.Logger.Warn("saving a photo failed", "err", err)
			c.say(ctx, "Could not save the photo; see the latch-telegram log.")
			return
		}
		paths = append(paths, path)
	}
	if strings.TrimSpace(caption) == "" {
		caption = imageText
	}
	c.mu.Lock()
	id := c.activeLocked()
	err := c.saveLocked()
	c.mu.Unlock()
	if err != nil {
		c.o.Logger.Warn("saving the chat state failed", "err", err)
	}
	select {
	case c.queue <- job{session: id, text: caption, images: paths}:
	default:
		c.discard(paths)
		c.say(ctx, "Too many messages waiting. Wait for an answer or use /stop.")
	}
}

// discard deletes photo files that will not be used any more.
func (c *Chat) discard(paths []string) {
	if len(paths) == 0 || c.o.Remove == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.o.Remove(ctx, paths); err != nil {
		c.o.Logger.Warn("removing a photo failed", "err", err)
	}
}

func (c *Chat) handleCommand(ctx context.Context, cmd string, messageID int64) {
	if cmd != "new" {
		c.track(messageID) // /new deletes its own message with the rest
	}
	switch cmd {
	case "start", "help":
		c.plain(ctx, c.welcome())
	case "new":
		c.newConversation(ctx, messageID)
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
	case "stop":
		dropped, running := c.stopRunning()
		if running {
			c.say(ctx, fmt.Sprintf("Stopping (dropped %d waiting).", dropped))
		} else {
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
	defer c.discard(j.images)
	if !c.isActive(j.session) {
		return // the owner started over while this waited
	}
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

	started := c.o.Now()
	// The answer to a decision is added to the request's message, so it has
	// no live preview (one would stay on screen next to it).
	lv := c.startLive(parent, draftID, j.messageID != 0)
	out, err := c.o.Runner.Run(ctx, Turn{Session: j.session, Message: j.text, Images: j.images, DB: c.dbFor(j.session)}, lv.update)
	lv.stop()
	c.mu.Lock()
	c.cancel, c.draft = nil, 0 // idle again before any message goes out
	c.mu.Unlock()

	if parent.Err() != nil {
		return
	}
	if !c.isActive(j.session) {
		// The owner started over meanwhile: nothing goes to the screen, and
		// the database the run may have written again goes too.
		c.removeDB(parent, c.dbFor(j.session))
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
		c.sayTo(parent, j.messageID, fmt.Sprintf("Stopped after %s without an answer.", c.o.Timeout))
		return
	case errors.As(err, &re) && re.UsageLimit():
		c.sayTo(parent, j.messageID, usageLimitText)
		return
	case errors.As(err, &re):
		c.sayTo(parent, j.messageID, fmt.Sprintf("Error: %s.", re.Error()))
		return
	case err != nil:
		c.o.Logger.Warn("agent run failed", "err", err)
		c.sayTo(parent, j.messageID, "Could not start; see the latch-telegram log.")
		return
	}
	c.deliver(parent, out, j, started)
}

// deliver shows the agent's answer. When the turn filed a request, the
// request is the reply and the answer is dropped; the answer to a decision
// goes under the outcome in the decided request's message; only otherwise is
// it a message of its own.
func (c *Chat) deliver(ctx context.Context, out string, j job, started time.Time) {
	if c.o.Filed != nil && c.o.Filed(ctx, started) {
		c.o.Logger.Debug("dropped the answer of a turn that filed a request")
		return
	}
	body := Markdown(out)
	if j.messageID != 0 && c.o.Follow != nil && strings.TrimSpace(body.S) != "" && c.o.Follow(ctx, j.messageID, body) {
		return
	}
	c.answer(ctx, out, j.messageID)
}

// Busy reports whether an agent turn is running.
func (c *Chat) Busy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cancel != nil
}

// usageLimitText is what the owner reads when the model's usage limit is
// used up: no error text, and nothing to fix.
const usageLimitText = "Usage limit reached. Try again after it resets."

// live shows a turn as it happens: a draft with a Stop button that grows
// with the answer, or the typing indicator when drafts do not work.
type live struct {
	c          *Chat
	ctx        context.Context
	id         int64
	typingOnly bool // only the typing indicator, never a draft

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

func (c *Chat) startLive(ctx context.Context, id int64, typingOnly bool) *live {
	lv := &live{c: c, ctx: ctx, id: id, typingOnly: typingOnly, done: make(chan struct{}), stopped: make(chan struct{})}
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
	drafts, shown := !lv.typingOnly, false
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
		if !drafts || strings.TrimSpace(p.Text) == "" {
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

// preview is the draft for p: the answer so far.
func (c *Chat) preview(p Progress) tgapi.Text {
	var b tgapi.Builder
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
	if _, err := c.post(ctx, note(text), nil); err != nil {
		c.o.Logger.Warn("sending failed", "err", err)
	}
}

// welcome is the /start and /help text: what the bot is, then each command
// on its own line with a blank line between.
func (c *Chat) welcome() string {
	intro := c.o.Intro
	if intro == "" {
		where := "this server"
		if h := c.o.Host; h != "" {
			where = "the " + strings.ToUpper(h[:1]) + h[1:] + " server"
		}
		intro = "On-call engineer for " + where + "."
	}
	var b strings.Builder
	b.WriteString(intro + "\n\nCommands:")
	for _, cmd := range Commands {
		b.WriteString("\n\n/" + cmd.Command + " - " + cmd.Description)
	}
	return b.String()
}

// plain sends text without the italics of a note.
func (c *Chat) plain(ctx context.Context, text string) {
	if _, err := c.post(ctx, tgapi.Plain(text), nil); err != nil {
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
	if replyTo == 0 {
		_, err := c.post(ctx, t, nil)
		return err
	}
	m, err := c.o.Bot.Reply(ctx, c.o.Chat, replyTo, t)
	if err == nil {
		c.track(m.MessageID)
	}
	return err
}

func note(s string) tgapi.Text {
	var b tgapi.Builder
	return b.Italic(s).Done()
}

func (c *Chat) saveLocked() error {
	b, err := json.Marshal(c.index)
	if err != nil {
		return err
	}
	return tgapprove.WriteFile(c.o.IndexPath, b)
}

// command splits "/name" (also "/name@bot") from any arguments, which no
// command takes.
func command(text string) (cmd string, ok bool) {
	if !strings.HasPrefix(text, "/") {
		return "", false
	}
	head, _, _ := strings.Cut(text[1:], " ")
	head, _, _ = strings.Cut(head, "@")
	return strings.ToLower(head), true
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
