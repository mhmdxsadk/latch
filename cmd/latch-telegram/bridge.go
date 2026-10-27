package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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

// bridge reads Telegram updates and hands the owner's to the approvals or
// the chat. Everything from anyone else is dropped here, in one place.
type bridge struct {
	bot       *tgapi.Client
	owner     int64
	approvals *tgapprove.Approver
	chat      *tgchat.Chat // nil: approvals only
	log       *slog.Logger
	now       func() time.Time
	loc       *time.Location
	host      string // the server's short name, for the bot's description
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
		case m.Text == "":
			b.say(ctx, "Only text messages are read.")
		case m.Date > 0 && b.now().Sub(time.Unix(m.Date, 0)) > staleAfter:
			b.say(ctx, fmt.Sprintf("Ignored a message sent at %s while the bridge was not running. Send it again if it still matters.",
				time.Unix(m.Date, 0).In(b.location()).Format("15:04")))
		case b.chat != nil:
			b.chat.HandleText(ctx, m.Text, m.MessageID)
		default:
			b.say(ctx, "This bot handles approvals only; chatting with the agent is not set up (agent in telegram.yaml).")
		}
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
// filed ended, so it can check its fix.
func outcome(r approval.Request) string {
	what := fmt.Sprintf("request %d (%s on %s %s)", r.ID, textsafe.Line(r.Action), textsafe.Line(r.Resource.Type), textsafe.Line(r.Resource.Name))
	var s string
	switch r.Status {
	case approval.StatusDenied:
		s = "Latch: the owner denied " + what + ". It did not run."
	case approval.StatusExpired:
		s = "Latch: " + what + " expired without a decision. It did not run."
	case approval.StatusDone, approval.StatusFailed:
		how := "The owner approved"
		if r.DecidedBy == nil {
			how = "Nobody decided in time, so Latch ran"
		}
		s = fmt.Sprintf("Latch: %s %s. It finished: %s.", how, what, r.Status)
	default:
		s = "Latch: " + what + " is " + string(r.Status) + "."
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
