package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/mhmdxsadk/latch/internal/textsafe"
	"github.com/mhmdxsadk/latch/internal/tgapi"
)

// ownerWatch is how often the bridge rereads the config while waiting for
// pair to set owner_id.
const ownerWatch = 2 * time.Second

// pairBot is what the bridge needs from Telegram while nobody is paired;
// tests substitute a fake.
type pairBot interface {
	Updates(ctx context.Context, offset int64) ([]tgapi.Update, error)
	Confirm(ctx context.Context, offset int64) error
	Send(ctx context.Context, chat int64, t tgapi.Text, kb tgapi.Keyboard) (tgapi.Message, error)
}

// safePath is a config path that can be pasted into a shell unquoted.
var safePath = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

// pairCommand is the command the owner runs on the server for code.
func pairCommand(configPath, code string) string {
	cmd := "sudo latch-telegram pair"
	if configPath != defaultConfig && safePath.MatchString(configPath) {
		cmd += " --config " + configPath
	}
	return cmd + " " + code
}

// desk answers people who message the bot while no owner is set. Each gets
// a code that only they see; pair turns one into owner_id.
type desk struct {
	bot        pairBot
	dir        string // the state directory
	configPath string
	now        func() time.Time
	log        *slog.Logger

	waiting []waiting
	codes   map[int64]string // the code each waiting account was sent
}

// handle replies to a private message with the account's pairing code.
func (d *desk) handle(ctx context.Context, m tgapi.Message) {
	if m.From == nil || m.From.IsBot || m.Chat.Type != "private" || m.Chat.ID != m.From.ID {
		return
	}
	now := d.now()
	d.waiting = liveWaiting(d.waiting, now)
	code := d.codes[m.From.ID]
	if code == "" || !d.holds(m.From.ID) {
		// No live code for this account yet. (A repeat message gets the
		// same code again: the first one may have scrolled away.)
		if len(d.waiting) >= maxWaiting {
			d.log.Warn("too many waiting pairing codes; ignored a message", "from", m.From.ID)
			return
		}
		var err error
		if code, err = newCode(); err != nil {
			d.log.Error("making a pairing code failed", "err", err)
			return
		}
		d.waiting = append(d.without(m.From.ID), waiting{
			UserID:  m.From.ID,
			Name:    displayName(*m.From),
			Hash:    hashCode(code),
			Expires: now.Add(codeTTL),
		})
		if d.codes == nil {
			d.codes = map[int64]string{}
		}
		d.codes[m.From.ID] = code
		if err := writeWaiting(d.dir, d.waiting); err != nil {
			d.log.Error("saving the pairing code failed", "err", err)
			return
		}
	}
	cmd := pairCommand(d.configPath, code)
	var b tgapi.Builder
	b.Bold("Pair this bot with your server").Plain("\nRun this on the server:\n\n").Code(cmd)
	b.Plain(fmt.Sprintf("\n\nThe code works for %d minutes. If you did not ask for this, ignore it.", int(codeTTL.Minutes())))
	kb := tgapi.Keyboard{{{Text: "Copy command", Copy: &tgapi.CopyText{Text: cmd}}}}
	if _, err := d.bot.Send(ctx, m.Chat.ID, b.Done(), kb); err != nil {
		d.log.Warn("sending the pairing code failed", "err", err)
	}
}

// holds reports whether user has a code that has not expired.
func (d *desk) holds(user int64) bool {
	for _, w := range d.waiting {
		if w.UserID == user {
			return true
		}
	}
	return false
}

func (d *desk) without(user int64) []waiting {
	var out []waiting
	for _, w := range d.waiting {
		if w.UserID != user {
			out = append(out, w)
		}
	}
	return out
}

func liveWaiting(ws []waiting, now time.Time) []waiting {
	var out []waiting
	for _, w := range ws {
		if now.Before(w.Expires) {
			out = append(out, w)
		}
	}
	return out
}

// displayName is how pair names an account: first name and @username, made
// safe for a terminal.
func displayName(u tgapi.User) string {
	parts := []string{textsafe.Line(u.FirstName)}
	if u.Username != "" {
		parts = append(parts, "@"+textsafe.Line(u.Username))
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// awaitOwner answers messages with pairing codes until owner_id appears in
// the config at path, then returns the loaded config. Nobody gets anything
// else: no approvals, no chat. It returns ctx's error if the service stops
// first.
func awaitOwner(ctx context.Context, bot pairBot, path, stateDir string, watch time.Duration, log *slog.Logger) (*Config, error) {
	pctx, stop := context.WithCancel(ctx)
	defer stop()
	found := make(chan *Config, 1)
	go func() {
		tick := time.NewTicker(watch)
		defer tick.Stop()
		for {
			select {
			case <-pctx.Done():
				return
			case <-tick.C:
				if c, err := loadConfig(path); err == nil && c.OwnerID != 0 {
					found <- c
					stop()
					return
				}
			}
		}
	}()
	d := &desk{bot: bot, dir: stateDir, configPath: path, now: time.Now, log: log}
	// Codes from an earlier run are no longer sendable; start clean.
	if err := writeWaiting(stateDir, nil); err != nil {
		return nil, err
	}
	var offset int64
	for pctx.Err() == nil {
		ups, err := bot.Updates(pctx, offset)
		if err != nil {
			if pctx.Err() != nil {
				break
			}
			wait := 5 * time.Second
			var te *tgapi.Error
			if errors.As(err, &te) && te.RetryAfter > 0 {
				wait = te.RetryAfter
			}
			log.Warn("reading Telegram updates failed", "err", err, "retry_in", wait)
			select {
			case <-pctx.Done():
			case <-time.After(wait):
			}
			continue
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			if u.Message != nil {
				d.handle(pctx, *u.Message)
			}
		}
	}
	if offset > 0 {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = bot.Confirm(cctx, offset)
		cancel()
	}
	select {
	case c := <-found:
		_ = writeWaiting(stateDir, nil)
		return c, nil
	default:
		return nil, ctx.Err()
	}
}
