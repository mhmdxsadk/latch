package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mhmdxsadk/latch/internal/approval"
	"github.com/mhmdxsadk/latch/internal/textsafe"
	"github.com/mhmdxsadk/latch/internal/tgapi"
	"github.com/mhmdxsadk/latch/internal/tgapprove"
	"github.com/mhmdxsadk/latch/internal/tgchat"
)

// staleAfter is how old a message may be and still be acted on. Telegram
// keeps messages sent while the bridge was down; old ones are not run.
const staleAfter = 10 * time.Minute

const (
	// downloadTimeout bounds one photo download. Downloads happen in the
	// polling loop, which must not stall behind one.
	downloadTimeout = 20 * time.Second
	// albumWait is how long after the last photo of an album the bridge waits
	// for another before sending them together.
	albumWait = 1500 * time.Millisecond
	// maxAlbum is Telegram's own limit for an album.
	maxAlbum = 10
)

// bridge reads Telegram updates and hands the owner's to the approvals or
// the chat. Everything from anyone else is dropped here, in one place.
type bridge struct {
	bot       *tgapi.Client
	owner     int64
	approvals *tgapprove.Approver
	chat      *tgchat.Chat // nil: approvals only
	login     *loginDesk   // nil: no /login (agent.login)
	log       *slog.Logger
	now       func() time.Time
	loc       *time.Location
	host      string // the server's short name, for the bot's description
	images    bool   // the agent's model reads photos (agent.images)

	mu         sync.Mutex
	albums     map[string]*album
	albumDelay time.Duration // albumWait, shorter in tests
}

// album is the photos of one Telegram album received so far.
type album struct {
	caption string
	images  [][]byte
	ids     []int64
	timer   *time.Timer
}

// description is what the bot's profile and empty chat say.
func (b *bridge) description() (long, short string) { return describe(b.host, b.chat != nil) }

// describe words the long and short description for a host (its short name,
// or empty) with or without the chat.
func describe(host string, chat bool) (long, short string) {
	server := "server"
	if host != "" {
		server = strings.ToUpper(host[:1]) + host[1:] + " server"
	}
	if chat {
		engineer := "On-call engineer for the " + server + "."
		return engineer + " Monitors containers and services, looks into incidents, and handles requested changes.", engineer
	}
	return "Approval requests for changes to the " + server + ".",
		strings.ToUpper(server[:1]) + server[1:] + " approvals."
}

func (b *bridge) commands() []tgapi.Command {
	if b.chat != nil {
		if b.login != nil {
			return append(slices.Clone(tgchat.Commands), loginCommand)
		}
		return tgchat.Commands
	}
	return []tgapi.Command{{Command: "help", Description: "What this bot does"}}
}

// run polls Telegram and Latch until ctx ends.
func (b *bridge) run(ctx context.Context) {
	done := make(chan struct{})
	go func() { b.approvals.Run(ctx); done <- struct{}{} }()
	workers := 1
	if b.chat != nil {
		workers++
		go func() { b.chat.Run(ctx); done <- struct{}{} }()
	}
	var offset int64
	for ctx.Err() == nil {
		ups, err := b.bot.Updates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			wait := 5 * time.Second
			var te *tgapi.Error
			if errors.As(err, &te) && te.RetryAfter > 0 {
				wait = te.RetryAfter
			}
			b.log.Warn("reading Telegram updates failed", "err", err, "retry_in", wait)
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
			continue
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			b.dispatch(ctx, u)
		}
	}
	if offset > 0 {
		// Without this, the last batch would be delivered again on the
		// next start and its messages run twice.
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := b.bot.Confirm(cctx, offset); err != nil {
			b.log.Warn("confirming the last updates failed", "err", err)
		}
		cancel()
	}
	for range workers {
		<-done
	}
}

// dispatch handles one update. Only the owner, in their private chat, is
// answered; anyone else gets no reply at all.
func (b *bridge) dispatch(ctx context.Context, u tgapi.Update) {
	switch {
	case u.Stopped != nil:
		if u.Stopped.Chat.ID != b.owner || b.chat == nil {
			return
		}
		b.chat.StopDraft(u.Stopped.DraftID)
	case u.CallbackQuery != nil:
		cb := *u.CallbackQuery
		if cb.From.ID != b.owner || cb.Message == nil || cb.Message.Chat.ID != b.owner {
			b.log.Info("ignored a button press from someone else", "from", cb.From.ID)
			return
		}
		if b.approvals.HandlePress(ctx, cb) {
			return
		}
		_ = b.bot.Answer(ctx, cb.ID, "This button no longer does anything.")
	case u.Message != nil:
		m := *u.Message
		if m.From == nil || m.From.ID != b.owner || m.Chat.ID != b.owner || m.Chat.Type != "private" {
			from := int64(0)
			if m.From != nil {
				from = m.From.ID
			}
			b.log.Info("ignored a message from someone else", "from", from, "chat", m.Chat.ID)
			return
		}
		switch {
		case len(m.ForwardOrigin) > 0:
			b.say(ctx, "Forwarded messages are ignored. Type the message yourself.")
		case m.Text == "" && len(m.Photo) == 0 && !isImageFile(m.Document):
			b.say(ctx, "Only text messages, photos and JPEG or PNG files are read.")
		case m.Date > 0 && b.now().Sub(time.Unix(m.Date, 0)) > staleAfter:
			b.say(ctx, fmt.Sprintf("Ignored a message sent at %s while the bridge was not running. Send it again if it still matters.",
				time.Unix(m.Date, 0).In(b.location()).Format("15:04")))
		case b.login != nil && isLoginCommand(m.Text):
			b.login.Start(ctx)
		case b.login != nil && b.login.Handle(ctx, m):
			// The pasted address never reaches the agent.
		case b.chat != nil && (len(m.Photo) > 0 || isImageFile(m.Document)):
			b.picture(ctx, m)
		case b.chat != nil:
			b.chat.HandleText(ctx, m.Text, m.MessageID)
		default:
			b.say(ctx, "This bot handles approvals only; chatting with the agent is not set up (agent in telegram.yaml).")
		}
	}
}

// isImageFile reports whether d is a file the chat reads as a photo.
func isImageFile(d *tgapi.Document) bool {
	return d != nil && (d.MimeType == "image/jpeg" || d.MimeType == "image/png")
}

// picture downloads the photo (its largest size) or image file in m and hands
// it to the chat, alone or with the rest of its album.
func (b *bridge) picture(ctx context.Context, m tgapi.Message) {
	if !b.images {
		b.say(ctx, "Photos are turned off (agent.images in telegram.yaml).")
		return
	}
	fileID := ""
	if m.Document != nil {
		fileID = m.Document.FileID
	} else {
		best := m.Photo[0]
		for _, p := range m.Photo[1:] {
			if p.Width*p.Height > best.Width*best.Height {
				best = p
			}
		}
		fileID = best.FileID
	}
	dctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	data, err := b.bot.Download(dctx, fileID, tgchat.MaxImage)
	cancel()
	if err != nil {
		b.log.Warn("downloading a photo failed", "err", err)
		b.say(ctx, "Could not download the photo.")
		return
	}
	if m.MediaGroupID == "" {
		b.chat.HandleImages(ctx, m.Caption, [][]byte{data}, []int64{m.MessageID})
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.albums == nil {
		b.albums = map[string]*album{}
	}
	a := b.albums[m.MediaGroupID]
	if a == nil {
		a = &album{}
		b.albums[m.MediaGroupID] = a
	}
	if len(a.images) < maxAlbum {
		a.images = append(a.images, data)
		a.ids = append(a.ids, m.MessageID)
	}
	if a.caption == "" {
		a.caption = m.Caption
	}
	if a.timer != nil {
		a.timer.Stop()
	}
	delay := b.albumDelay
	if delay == 0 {
		delay = albumWait
	}
	group := m.MediaGroupID
	a.timer = time.AfterFunc(delay, func() { b.sendAlbum(ctx, group) })
}

// sendAlbum gives a finished album to the chat as one message.
func (b *bridge) sendAlbum(ctx context.Context, group string) {
	b.mu.Lock()
	a := b.albums[group]
	delete(b.albums, group)
	b.mu.Unlock()
	if a != nil {
		b.chat.HandleImages(ctx, a.caption, a.images, a.ids)
	}
}

func (b *bridge) location() *time.Location {
	if b.loc == nil {
		return time.Local
	}
	return b.loc
}

func (b *bridge) say(ctx context.Context, text string) {
	var t tgapi.Builder
	if _, err := b.bot.Send(ctx, b.owner, t.Italic(text).Done(), nil); err != nil {
		b.log.Warn("sending failed", "err", err)
	}
}

// outcome is the automatic message telling the agent how a request it
// filed ended, so it can check its fix. It never names Latch: the owner
// knows one thing, the agent, and the agent repeats what it reads.
func outcome(r approval.Request) string {
	what := fmt.Sprintf("request %d (%s on %s %s)", r.ID, textsafe.Line(r.Action), textsafe.Line(r.Resource.Type), textsafe.Line(r.Resource.Name))
	var s string
	switch r.Status {
	case approval.StatusDenied:
		s = "Automatic message: the owner denied " + what + ". It did not run."
	case approval.StatusExpired:
		s = "Automatic message: " + what + " expired without a decision. It did not run."
	case approval.StatusDone, approval.StatusFailed:
		how := "The owner approved"
		if r.DecidedBy == nil {
			how = "Nobody decided in time, so the server ran"
		}
		s = fmt.Sprintf("Automatic message: %s %s. It finished: %s.", how, what, r.Status)
	default:
		s = "Automatic message: " + what + " is " + string(r.Status) + "."
	}
	if r.Result != nil {
		s += " Result: " + textsafe.Line(r.Result.Summary)
		if len(r.Result.Details) > 0 {
			s += "\n" + textsafe.Line(strings.Join(r.Result.Details, " | "))
		}
	}
	if r.Status == approval.StatusDone || r.Status == approval.StatusFailed {
		s += "\nCheck whether the problem is fixed and tell the owner."
	}
	return s
}
