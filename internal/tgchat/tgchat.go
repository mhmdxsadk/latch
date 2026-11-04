// Package tgchat lets the owner talk to the agent team from Telegram.
//
// Each Telegram message becomes one agent turn in the active thread.
// A thread lives in Docker Agent's session database, one file per
// thread; this package keeps an index (id, title, times, the messages
// on screen) and a transcript per thread, so the owner can switch
// between them without ever seeing an id: /new clears the screen and starts
// over, /threads goes back to an earlier thread and shows its messages
// again. At most MaxThreads threads are kept, and a thread
// quiet for IdleAfter is left behind by the next message. Turns run one at a
// time, in order. Agent answers are untrusted text: they are cleaned of
// control characters and their Markdown becomes formatting this package
// computes (never links). A turn that files a request shows the request alone, in one
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
	"slices"
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
	{Command: "new", Description: "Clears the chat and starts a new thread"},
	{Command: "threads", Description: "Shows earlier threads to go back to"},
	{Command: "settle", Description: "Closes this thread and clears the chat"},
	{Command: "stop", Description: "Stops the running answer and drops waiting messages"},
	{Command: "help", Description: "Shows this message"},
}

// Options configure a Chat.
type Options struct {
	// LoginCommand says the bridge handles /login, so a turn that fails
	// for want of a sign-in points to it. ExtraCommands are listed in /help
	// after the chat's own.
	LoginCommand  bool
	ExtraCommands []tgapi.Command
	Bot           Bot
	Chat          int64
	Runner        Runner
	// IndexPath is a root-only file holding the chat state.
	IndexPath string
	// Name labels the agent's answers, for example "keeper".
	Name    string
	Timeout time.Duration
	// Location is the owner's time zone for thread names.
	Location *time.Location
	// ThreadDir is where each thread's Docker Agent database lives
	// ("" uses the agent's shared database and never deletes anything).
	ThreadDir string
	// HistoryDir holds the transcripts, root-only ("" shows no history).
	HistoryDir string
	// Remove deletes files in ThreadDir, as the agent's account.
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

	hmu    sync.Mutex // transcripts
	mu     sync.Mutex
	index  index
	cancel context.CancelFunc // of the running turn, nil when idle
	draft  int64              // live preview id of the running turn
	held   []held             // finished answers waiting for the rest of a burst

	drafts atomic.Int64 // last draft id handed out
}

type job struct {
	thread string
	text   string
	note   string // shown to the owner for a message the bridge sends
	images []string
	// messageID is the message the answer replies to: the request's message
	// for the outcome of a decision, 0 otherwise.
	messageID int64
	// source is the owner's message this job answers (0: none), the one a
	// reply goes under when several messages are answered together.
	source int64
}

// held is a finished answer (or note) kept until every message of the burst
// has one, so they go out together.
type held struct {
	thread string
	source int64  // the owner's message it answers
	fixed  int64  // the decided request's message; the reply always goes there
	out    string // the agent's answer, Markdown
	note   string // a note from the bridge instead of an answer
}

type index struct {
	// Active is the current thread ("" until the first message after
	// /new).
	Active  string   `json:"active,omitempty"`
	Threads []thread `json:"sessions,omitempty"` // key kept: renaming it would orphan saved indexes
	// Loose are screen messages sent while no thread was active.
	Loose []int64 `json:"loose,omitempty"`
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
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-c.queue:
			c.turn(ctx, j)
			if len(c.queue) == 0 {
				c.flush(ctx)
			}
		}
	}
}

// HandleText handles a message from the owner: a command, or a message for
// the agent. messageID is the owner's message, only noted for /new to clear;
// the answer is a plain message, not a reply to it.
func (c *Chat) HandleText(ctx context.Context, text string, messageID int64) {
	c.rollOver()
	if cmd, ok := command(text); ok {
		c.handleCommand(ctx, cmd, messageID)
		return
	}
	c.track(messageID) // the owner's message is on screen too
	if len(text) > maxMessage {
		c.say(ctx, "That message is too long; keep it under 8 KB.")
		return
	}
	id, victims := c.begin(text)
	c.dropThreads(ctx, victims)
	c.appendHistory(id, "owner", text)
	c.enqueue(ctx, job{thread: id, text: text, source: messageID})
}

// Notify sends text to the agent in the current thread as an automatic
// message, for example the outcome of a request the owner decided, and shows
// the owner note instead of the whole text. The agent's answer replies to
// message replyTo (the request's message; 0 for none). It never blocks;
// when the queue is full the message is dropped and logged.
func (c *Chat) Notify(text, note string, replyTo int64) {
	c.mu.Lock()
	id := c.index.Active
	c.mu.Unlock()
	if id == "" {
		return // no thread: nobody to tell
	}
	select {
	case c.queue <- job{thread: id, text: text, note: note, messageID: replyTo}:
	default:
		c.o.Logger.Warn("queue full; dropped an automatic message")
	}
}

// HandleImages handles photos from the owner: one message for the agent with
// them attached. caption is the text sent with them ("" for none);
// messageIDs are the owner's messages, noted for /new to clear.
func (c *Chat) HandleImages(ctx context.Context, caption string, images [][]byte, messageIDs []int64) {
	c.rollOver()
	c.track(messageIDs...)
	if c.o.SaveImage == nil {
		c.say(ctx, "Photos are not available; the agent's thread folder is not set up.")
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
	var source int64
	if len(messageIDs) > 0 {
		source = messageIDs[0]
	}
	titleText, shown := caption, caption+" [photo]"
	if strings.TrimSpace(caption) == "" {
		caption, titleText, shown = imageText, "Photo", "[photo]"
	}
	id, victims := c.begin(titleText)
	c.dropThreads(ctx, victims)
	c.appendHistory(id, "owner", shown)
	select {
	case c.queue <- job{thread: id, text: caption, images: paths, source: source}:
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
	if cmd != "new" && cmd != "settle" {
		c.track(messageID) // /new and /settle delete their own message with the rest
	}
	switch cmd {
	case "start":
		c.plain(ctx, startText)
	case "help":
		c.plain(ctx, c.welcome())
	case "new":
		c.leave(ctx, messageID, false)
	case "settle":
		c.leave(ctx, messageID, true)
	case "threads":
		c.threadList(ctx)
	case "stop":
		dropped, running := c.stopRunning()
		if running {
			c.say(ctx, fmt.Sprintf("Stopping (dropped %d waiting).", dropped))
		} else if dropped > 0 {
			c.say(ctx, fmt.Sprintf("Nothing running (dropped %d waiting).", dropped))
		} else {
			c.say(ctx, "Nothing running.")
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
	if !c.isActive(j.thread) {
		return // the owner moved to another thread while this waited
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
	out, err := c.o.Runner.Run(ctx, Turn{Thread: j.thread, Message: j.text, Images: j.images, DB: c.dbFor(j.thread)}, lv.update)
	lv.stop()
	c.mu.Lock()
	c.cancel, c.draft = nil, 0 // idle again before any message goes out
	c.mu.Unlock()

	if parent.Err() != nil {
		return
	}
	if !c.isActive(j.thread) {
		// The owner moved to another thread meanwhile: nothing goes to
		// the screen, but the answer is kept for when they come back. A
		// thread deleted meanwhile loses the database the run wrote.
		switch {
		case !c.exists(j.thread):
			c.removeDB(parent, c.dbFor(j.thread))
		case err == nil:
			c.appendHistory(j.thread, "agent", out)
		}
		return
	}
	select {
	case <-stopped:
		c.hold(j, held{note: "Stopped."})
		return
	default:
	}
	var re *RunError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		c.hold(j, held{note: fmt.Sprintf("Stopped after %s without an answer.", c.o.Timeout)})
		return
	case errors.As(err, &re) && re.UsageLimit():
		c.hold(j, held{note: usageLimitText})
		return
	case errors.As(err, &re) && re.LoginExpired() && c.o.LoginCommand:
		c.hold(j, held{note: loginExpiredText})
		return
	case errors.As(err, &re):
		c.hold(j, held{note: fmt.Sprintf("Error: %s.", re.Error())})
		return
	case err != nil:
		c.o.Logger.Warn("agent run failed", "err", err)
		c.hold(j, held{note: "Could not start; see the latch-telegram log."})
		return
	}
	c.deliver(parent, out, j, started)
}

// deliver keeps the agent's answer for flush. When the turn filed a request,
// the request is the reply and the answer is dropped.
func (c *Chat) deliver(ctx context.Context, out string, j job, started time.Time) {
	if c.o.Filed != nil && c.o.Filed(ctx, started) {
		c.o.Logger.Debug("dropped the answer of a turn that filed a request")
		return
	}
	c.appendHistory(j.thread, "agent", out)
	c.hold(j, held{out: out})
}

// hold keeps what a job produced until the burst it belongs to is done.
func (c *Chat) hold(j job, h held) {
	h.thread, h.source, h.fixed = j.thread, j.source, j.messageID
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held = append(slices.DeleteFunc(c.held, func(o held) bool { return o.thread != c.index.Active }), h)
}

// flush sends everything held. One answer is a plain message. Several, from a
// burst of messages, are each a reply to the message they answer. An answer to
// a decision goes under the decided request instead. Answers of a thread
// the owner left meanwhile are dropped (the transcript has them).
func (c *Chat) flush(ctx context.Context) {
	c.mu.Lock()
	items := slices.DeleteFunc(c.held, func(h held) bool { return h.thread != c.index.Active })
	c.held = nil
	c.mu.Unlock()
	for _, h := range items {
		replyTo := h.fixed
		if replyTo == 0 && len(items) > 1 {
			replyTo = h.source
		}
		if h.note != "" {
			c.sayTo(ctx, replyTo, h.note)
			continue
		}
		body := Markdown(h.out)
		switch {
		case h.fixed != 0 && c.o.Follow != nil && strings.TrimSpace(body.S) != "" && c.o.Follow(ctx, h.fixed, body):
		default:
			c.answer(ctx, h.out, replyTo)
		}
	}
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

// loginExpiredText is shown when the model's sign-in stopped working and the
// bridge can sign in again (Options.LoginCommand).
const loginExpiredText = "The model sign-in expired. Send /login to sign in again."

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

// startText is the /start answer. The bot description already shows in the
// empty chat, so it only points to /help.
const startText = "Send /help to see the commands."

// welcome is the /help text: each command with its menu description, one
// per paragraph.
func (c *Chat) welcome() string {
	var lines []string
	for _, cmd := range append(slices.Clone(Commands), c.o.ExtraCommands...) {
		lines = append(lines, "/"+cmd.Command+" - "+cmd.Description)
	}
	return strings.Join(lines, "\n\n")
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
