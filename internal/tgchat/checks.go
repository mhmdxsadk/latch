package tgchat

import (
	"context"
	"strings"
	"time"
)

// Scheduled checks: every CheckEvery the agent sweeps the server on its own,
// with CheckRunner, which must not be able to change anything (an agent
// config without the change tools, run in restricted safety mode). When it
// finds nothing new it answers allClear and the owner hears nothing. When it
// finds a problem, its report becomes the current conversation, so the owner's
// reply ("fix it") continues there. Fixes are only ever made when the owner
// asks.

// allClear is the whole answer of a check that found nothing new.
const allClear = "ALL_CLEAR"

// remindAfter is how long a reported problem is not reported again, unless
// it got worse.
const remindAfter = 24 * time.Hour

// report is one problem a check told the owner about.
type report struct {
	At       time.Time `json:"at"`
	Headline string    `json:"headline"`
}

// checkLoop queues a check every CheckEvery, counting from the last one so a
// bridge restart does not start the clock again.
func (c *Chat) checkLoop(ctx context.Context) {
	for {
		c.mu.Lock()
		next := c.index.LastCheck.Add(c.o.CheckEvery)
		c.mu.Unlock()
		if wait := time.Until(next); wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		c.mu.Lock()
		c.index.LastCheck = c.o.Now().UTC()
		err := c.saveLocked()
		c.mu.Unlock()
		if err != nil {
			c.o.Logger.Warn("saving the chat state failed", "err", err)
		}
		select {
		case c.queue <- job{check: true}:
		default:
			c.o.Logger.Warn("queue full; skipped a scheduled check")
		}
	}
}

// check runs one check; manual when the owner asked with /check, which
// also answers when there is nothing new.
func (c *Chat) check(ctx context.Context, manual bool) {
	ctx, cancel := context.WithTimeout(ctx, c.o.Timeout)
	defer cancel()
	now := c.o.Now().UTC()
	c.mu.Lock()
	var recent []report
	for _, r := range c.index.Reported {
		if now.Sub(r.At) < remindAfter {
			recent = append(recent, r)
		}
	}
	c.index.Reported = recent
	c.mu.Unlock()

	// A check runs in a conversation of its own with its own database, so
	// one that finds nothing can be deleted without a trace.
	id := newID()
	t := Turn{Session: id, Message: checkPrompt(recent), DB: c.dbFor(id)}
	if manual {
		_ = c.o.Bot.Typing(ctx, c.o.Chat)
	}
	out, err := c.o.CheckRunner.Run(ctx, t, nil)
	if err != nil {
		c.removeDB(ctx, t.DB)
		c.o.Logger.Warn("check failed", "err", err)
		if manual {
			c.say(ctx, "The check could not run; see the latch-telegram log.")
		}
		return
	}
	answer := strings.TrimSpace(out)
	if answer == "" || strings.EqualFold(strings.Trim(answer, "`*. \n"), allClear) {
		c.removeDB(ctx, t.DB)
		c.o.Logger.Info("check found nothing new")
		if manual {
			c.plain(ctx, "All clear.")
		}
		return
	}
	// A finding becomes the current conversation, so the owner's reply
	// ("fix it") continues there; the conversation it replaces is deleted.
	// What is on the screen stays and goes with /new.
	c.mu.Lock()
	old := c.index.Conversation
	c.index.Conversation = id
	c.index.Reported = append(c.index.Reported, report{At: now, Headline: textHead(firstLine(answer), 200)})
	err = c.saveLocked()
	c.mu.Unlock()
	if err != nil {
		c.o.Logger.Warn("saving the chat state failed", "err", err)
	}
	c.removeDB(ctx, c.dbFor(old))
	c.answer(ctx, answer, 0)
}

// checkPrompt asks for a read-only sweep and says what was already reported.
func checkPrompt(recent []report) string {
	var b strings.Builder
	b.WriteString("This is a scheduled check; nobody asked a question. Sweep the server for problems: " +
		"containers that are unhealthy, restarting, exited or failed; failed systemd units; disks over 90% full; " +
		"memory or load far above normal; errors in the recent logs of anything unhealthy.\n\n" +
		"You cannot change anything in this run. Do not file or suggest that you filed any change.\n\n")
	if len(recent) > 0 {
		b.WriteString("Already reported to the owner in the last day (do not report these again unless they got worse):\n")
		for _, r := range recent {
			b.WriteString("- " + r.Headline + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("If you find nothing new, reply with exactly " + allClear + " and nothing else. " +
		"Otherwise write to the owner as you would after noticing a problem on your own: start with one sentence " +
		"saying what is wrong, then what it affects and how you would fix it if they ask you to.")
	return b.String()
}

// firstLine is the first line of an answer as plain text, without its
// Markdown.
func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(Markdown(l).S)
}

func textHead(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
