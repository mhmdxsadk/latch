// Package tgapprove shows Latch's change requests in the owner's Telegram chat
// and lets the owner approve or deny them with a button.
//
// Every message text comes from Latch's own description of a request, never
// from the agent's words. A button carries only the request id and a
// fingerprint of what the message showed; before deciding, the request is
// fetched again and must still match, so an old button can never decide a
// different request. A change too long to show in one message gets no
// Approve button: the owner approves it on the server, where it can be read
// in full.
package tgapprove

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mhmdxsadk/latch/internal/admin"
	"github.com/mhmdxsadk/latch/internal/approval"
	"github.com/mhmdxsadk/latch/internal/textsafe"
	"github.com/mhmdxsadk/latch/internal/tgapi"
)

// Admin is the part of the admin API client the approver uses.
type Admin interface {
	Pending(ctx context.Context) ([]approval.Request, error)
	Recent(ctx context.Context) ([]approval.Request, error)
	Get(ctx context.Context, id int64) (approval.Request, error)
	Approve(ctx context.Context, id int64) (approval.Request, error)
	Deny(ctx context.Context, id int64) (approval.Request, error)
}

// Bot is the part of the Telegram client the approver uses.
type Bot interface {
	Send(ctx context.Context, chat int64, t tgapi.Text, kb tgapi.Keyboard) (tgapi.Message, error)
	Edit(ctx context.Context, chat, message int64, t tgapi.Text, kb tgapi.Keyboard) error
	Answer(ctx context.Context, callbackID, text string) error
	DeleteMessages(ctx context.Context, chat int64, ids []int64) error
}

// Defaults for Options.
const (
	DefaultInterval = 3 * time.Second
	// noticeWindow is how old a finished automatic restart may be and still
	// get a notice; older ones (seen at first start) stay quiet.
	noticeWindow = 10 * time.Minute
	// keepFinished is how many finished requests the state file remembers.
	keepFinished = 500
)

// Options configure an Approver.
type Options struct {
	Admin Admin
	Bot   Bot
	// Chat is the owner's private chat id (the same as their user id).
	Chat int64
	// StatePath is a root-only file remembering which requests were shown.
	StatePath string
	Interval  time.Duration
	Now       func() time.Time
	// Location is the owner's time zone for the times shown.
	Location *time.Location
	// AgentName and AgentUID name requests from the agent's account ("From
	// keeper" rather than "From uid 1000"). Optional.
	AgentName string
	AgentUID  *uint32
	Logger    *slog.Logger
	// OnFinished is called once when a request the owner was asked about
	// ends, however it was decided. The chat bridge uses it to tell the
	// agent the outcome. message is the request's chat message. It must
	// not block.
	OnFinished func(r approval.Request, message int64)
}

// Approver announces requests and handles button presses.
type Approver struct {
	o Options

	mu    sync.Mutex
	state state
}

type state struct {
	Requests map[int64]*shown `json:"requests"`
}

// shown is what the chat shows for one request.
type shown struct {
	Message int64  `json:"message,omitempty"` // 0: never shown
	Text    string `json:"text,omitempty"`    // hash of the shown text
	Final   bool   `json:"final"`
	// Lead is the agent's answer that filed the request, shown above it while
	// it waits; Tail is the agent's answer to the outcome, shown under it.
	// Both are agent text: formatting is computed here, never links, and bold
	// is Latch's alone, so the request's own line cannot be imitated.
	Lead *tgapi.Text `json:"lead,omitempty"`
	Tail *tgapi.Text `json:"tail,omitempty"`
}

// New loads the state file (a missing file is an empty state).
func New(o Options) (*Approver, error) {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
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
	a := &Approver{o: o, state: state{Requests: map[int64]*shown{}}}
	b, err := os.ReadFile(o.StatePath)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &a.state); err != nil {
			return nil, fmt.Errorf("%s: %w", o.StatePath, err)
		}
		if a.state.Requests == nil {
			a.state.Requests = map[int64]*shown{}
		}
	}
	return a, nil
}

// Run polls Latch until ctx ends.
func (a *Approver) Run(ctx context.Context) {
	t := time.NewTicker(a.o.Interval)
	defer t.Stop()
	for {
		if err := a.Poll(ctx); err != nil && ctx.Err() == nil {
			a.o.Logger.Warn("checking requests failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Poll does one round: announce new requests, update changed ones.
func (a *Approver) Poll(ctx context.Context) error {
	pending, err := a.o.Admin.Pending(ctx)
	if err != nil {
		return err
	}
	recent, err := a.o.Admin.Recent(ctx)
	if err != nil {
		return err
	}
	byID := map[int64]approval.Request{}
	for _, r := range slices.Concat(recent, pending) {
		byID[r.ID] = r
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := make([]int64, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		a.updateLocked(ctx, byID[id])
	}
	// Shown requests that dropped out of the lists: ask for each one.
	for id, s := range a.state.Requests {
		if _, ok := byID[id]; ok || s.Final || s.Message == 0 {
			continue
		}
		r, err := a.o.Admin.Get(ctx, id)
		if admin.IsNotFound(err) {
			a.goneLocked(ctx, id, s)
			continue
		}
		if err != nil {
			return err
		}
		a.updateLocked(ctx, r)
	}
	return a.saveLocked()
}

// updateLocked brings the chat in line with r.
func (a *Approver) updateLocked(ctx context.Context, r approval.Request) {
	s, known := a.state.Requests[r.ID]
	final := isFinal(r.Status)
	if !known {
		switch {
		case r.Status == approval.StatusPending:
			text, kb := a.render(r, nil)
			m, err := a.o.Bot.Send(ctx, a.o.Chat, text, kb)
			if err != nil {
				a.o.Logger.Warn("announcing a request failed", "id", r.ID, "err", err)
				return // tried again next round
			}
			a.state.Requests[r.ID] = &shown{Message: m.MessageID, Text: hashText(text)}
		case !r.NeedsApproval && !final:
			// An automatic restart still running: wait for its outcome.
		case !r.NeedsApproval && a.o.Now().Sub(r.FinishedAt) < noticeWindow:
			if _, err := a.o.Bot.Send(ctx, a.o.Chat, a.notice(r), nil); err != nil {
				a.o.Logger.Warn("sending a notice failed", "id", r.ID, "err", err)
				return
			}
			a.state.Requests[r.ID] = &shown{Final: true}
		default:
			// Decided on the server before it could be shown, or too old.
			a.state.Requests[r.ID] = &shown{Final: final}
		}
		return
	}
	if s.Final || s.Message == 0 {
		if !s.Final && final {
			s.Final = true
		}
		return
	}
	text, kb := a.render(r, s)
	if hashText(text) != s.Text {
		if err := a.o.Bot.Edit(ctx, a.o.Chat, s.Message, text, kb); err != nil {
			a.o.Logger.Warn("updating a request message failed", "id", r.ID, "err", err)
			return
		}
		s.Text = hashText(text)
	}
	if final {
		s.Final = true
		if a.o.OnFinished != nil && r.NeedsApproval {
			a.o.OnFinished(r, s.Message)
		}
	}
}

// goneLocked marks a shown request Latch no longer knows: Latch keeps
// pending requests in memory only, so a restart forgets them.
func (a *Approver) goneLocked(ctx context.Context, id int64, s *shown) {
	var b tgapi.Builder
	b.Bold(fmt.Sprintf("Request %d is gone", id))
	b.Plain("\nLatch no longer knows it (it restarted, or forgot the request). Nothing will run.")
	text := b.Done()
	if err := a.o.Bot.Edit(ctx, a.o.Chat, s.Message, text, nil); err != nil {
		a.o.Logger.Warn("updating a request message failed", "id", id, "err", err)
		return
	}
	s.Text, s.Final = hashText(text), true
}

// FinishedMessages returns the chat messages of requests that are decided,
// for the chat to delete when it is cleared, and forgets them. A request
// still waiting is never included: its buttons must stay.
func (a *Approver) FinishedMessages() []int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	var ids []int64
	for _, s := range a.state.Requests {
		if s.Final && s.Message != 0 {
			ids = append(ids, s.Message)
			s.Message = 0
		}
	}
	if len(ids) > 0 {
		if err := a.saveLocked(); err != nil {
			a.o.Logger.Warn("saving the request state failed", "err", err)
		}
	}
	return ids
}

// HandlePress handles a button press. It reports false when the data is not
// an approval button. The caller has already checked that the owner pressed.
func (a *Approver) HandlePress(ctx context.Context, cb tgapi.CallbackQuery) bool {
	approve, id, fp, ok := parseData(cb.Data)
	if !ok {
		return false
	}
	answer := a.decide(ctx, approve, id, fp)
	if err := a.o.Bot.Answer(ctx, cb.ID, answer); err != nil {
		a.o.Logger.Warn("answering a button failed", "err", err)
	}
	return true
}

func (a *Approver) decide(ctx context.Context, approve bool, id int64, fp string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, err := a.o.Admin.Get(ctx, id)
	switch {
	case admin.IsNotFound(err):
		if s, ok := a.state.Requests[id]; ok && s.Message != 0 && !s.Final {
			a.goneLocked(ctx, id, s)
			_ = a.saveLocked()
		}
		return fmt.Sprintf("Request %d is gone or expired. Nothing done.", id)
	case err != nil:
		return "Could not reach Latch. Nothing done."
	}
	if fingerprint(r) != fp {
		return fmt.Sprintf("This message no longer matches request %d. Nothing done.", id)
	}
	if r.Status != approval.StatusPending {
		a.updateLocked(ctx, r)
		_ = a.saveLocked()
		return fmt.Sprintf("Request %d is already %s.", id, r.Status)
	}
	if approve && a.tooLong(r) {
		return fmt.Sprintf("Too long to approve here. On the server: latch approve %d", id)
	}
	verb, do := "Denied", a.o.Admin.Deny
	if approve {
		verb, do = "Approved", a.o.Admin.Approve
	}
	got, err := do(ctx, id)
	var ae *admin.APIError
	switch {
	case errors.As(err, &ae) && ae.Status == http.StatusConflict && ae.Message == approval.ErrBusy.Error():
		return fmt.Sprintf("Another change to %s is running. Try again in a moment.", textsafe.Line(r.Resource.Name))
	case errors.As(err, &ae):
		return fmt.Sprintf("Request %d: %s. Nothing done.", id, textsafe.Line(ae.Message))
	case err != nil:
		return "Could not reach Latch. Nothing done."
	}
	a.updateLocked(ctx, got)
	_ = a.saveLocked()
	return fmt.Sprintf("%s request %d.", verb, id)
}

// render is the message for r and its buttons (nil when it is decided). s is
// what the chat already shows for r, if anything: the agent's words go with
// the request in one message.
//
// Messages read like a colleague asking for a sign-off: one line saying
// who wants to do what, the deadline, and the exact details folded away
// until tapped.
func (a *Approver) render(r approval.Request, s *shown) (tgapi.Text, tgapi.Keyboard) {
	if r.Status != approval.StatusPending {
		out := a.outcome(r)
		if s != nil && s.Tail != nil {
			if t, ok := stack(out, *s.Tail); ok {
				out = t
			}
		}
		return out, nil
	}
	head, body := a.pendingText(r)
	fp := fingerprint(r)
	deny := tgapi.Button{Text: "Deny", Data: data(false, r.ID, fp), Style: tgapi.StyleDanger}
	if full := join(head, body); full.Len() <= tgapi.MaxText {
		if s != nil && s.Lead != nil {
			if t, ok := stack(*s.Lead, full); ok {
				full = t
			}
		}
		return full, tgapi.Keyboard{{{Text: "Approve", Data: data(true, r.ID, fp), Style: tgapi.StyleSuccess}, deny}}
	}
	// Too long to read here: offer the command to approve it on the server.
	copyCmd := tgapi.Button{Text: "Copy approve command", Copy: &tgapi.CopyText{Text: fmt.Sprintf("latch approve %d", r.ID)}}
	return a.shortened(r, head, body), tgapi.Keyboard{{copyCmd, deny}}
}

func (a *Approver) tooLong(r approval.Request) bool {
	head, body := a.pendingText(r)
	return join(head, body).Len() > tgapi.MaxText
}

// stack puts two texts in one message, a blank line apart. It reports false
// when they do not fit together: agent text is never cut to make room.
func stack(first, second tgapi.Text) (tgapi.Text, bool) {
	if first.Len()+2+second.Len() > tgapi.MaxText {
		return tgapi.Text{}, false
	}
	var b tgapi.Builder
	return b.Text(first).Plain("\n\n").Text(second).Done(), true
}

// Attach gives the agent's answer to a request its turn filed: the answer
// and the request become one message with the buttons under it. since is
// when the turn started. It reports false when the turn filed nothing the
// answer can join (nothing was changed; the caller sends the answer itself).
// An empty answer still reports true when the turn filed a request, so the
// request alone is the whole reply.
func (a *Approver) Attach(ctx context.Context, since time.Time, lead tgapi.Text) bool {
	pending, err := a.o.Admin.Pending(ctx)
	if err != nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var r *approval.Request
	for i, c := range pending {
		if c.Status != approval.StatusPending || !c.NeedsApproval || c.RunsOnTimeout || c.CreatedAt.Before(since) ||
			(a.o.AgentUID != nil && *a.o.AgentUID != c.RequestedBy.UID) {
			continue
		}
		if r == nil || c.ID < r.ID {
			r = &pending[i]
		}
	}
	if r == nil || a.tooLong(*r) {
		return false
	}
	s := a.state.Requests[r.ID]
	if s != nil && (s.Final || s.Message == 0 || s.Lead != nil) {
		return false
	}
	if strings.TrimSpace(lead.S) == "" {
		return true
	}
	if _, ok := stack(lead, join(a.pendingText(*r))); !ok {
		return false
	}
	if s == nil {
		s = &shown{Lead: &lead}
		text, kb := a.render(*r, s)
		m, err := a.o.Bot.Send(ctx, a.o.Chat, text, kb)
		if err != nil {
			a.o.Logger.Warn("announcing a request failed", "id", r.ID, "err", err)
			return false
		}
		s.Message, s.Text = m.MessageID, hashText(text)
		a.state.Requests[r.ID] = s
		_ = a.saveLocked()
		return true
	}
	s.Lead = &lead
	text, kb := a.render(*r, s)
	if err := a.replaceLocked(ctx, s, text, kb); err != nil {
		a.o.Logger.Warn("updating a request message failed", "id", r.ID, "err", err)
		s.Lead = nil
		return false
	}
	_ = a.saveLocked()
	return true
}

// replaceLocked shows text as a new message and deletes the old one. The
// turn's live preview only goes away when a message arrives; an edit would
// leave it on screen for a while next to the message that now holds the
// answer.
func (a *Approver) replaceLocked(ctx context.Context, s *shown, text tgapi.Text, kb tgapi.Keyboard) error {
	m, err := a.o.Bot.Send(ctx, a.o.Chat, text, kb)
	if err != nil {
		return err
	}
	old := s.Message
	s.Message, s.Text = m.MessageID, hashText(text)
	if err := a.o.Bot.DeleteMessages(ctx, a.o.Chat, []int64{old}); err != nil {
		a.o.Logger.Warn("deleting the old request message failed", "message", old, "err", err)
	}
	return nil
}

// Follow adds the agent's answer to a decision under the outcome in the
// request's own message instead of a message of its own. It reports false
// when it did not fit or the message is gone; the caller sends the answer.
func (a *Approver) Follow(ctx context.Context, message int64, tail tgapi.Text) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, s := range a.state.Requests {
		if s.Message != message || !s.Final || s.Tail != nil {
			continue
		}
		r, err := a.o.Admin.Get(ctx, id)
		if err != nil || r.Status == approval.StatusPending {
			return false
		}
		if _, ok := stack(a.outcome(r), tail); !ok {
			return false
		}
		s.Tail = &tail
		text, _ := a.render(r, s)
		if err := a.replaceLocked(ctx, s, text, nil); err != nil {
			a.o.Logger.Warn("updating a request message failed", "id", id, "err", err)
			s.Tail = nil
			return false
		}
		_ = a.saveLocked()
		return true
	}
	return false
}

// join puts a head and its details together, the details in one quote
// that opens when tapped.
func join(head tgapi.Text, body []tgapi.Text) tgapi.Text {
	var b, d tgapi.Builder
	for i, t := range body {
		if i > 0 {
			d.Plain("\n")
		}
		d.Text(t)
	}
	b.Text(head)
	if d.Len() > 0 {
		b.Plain("\n").Quote(d.Done(), true)
	}
	return b.Done()
}

// shortened shows as many details of a long change as fit and says where to
// read and approve the rest.
func (a *Approver) shortened(r approval.Request, head tgapi.Text, body []tgapi.Text) tgapi.Text {
	var tail tgapi.Builder
	tail.Italic("Too long to show in full here, so there is no Approve button. Read and approve it on the server.")
	budget := tgapi.MaxText - head.Len() - tail.Len() - 40
	var kept []tgapi.Text
	used := 0
	for _, t := range body {
		if used+t.Len()+1 > budget {
			// A block that does not fit is cut, never shown in part as if
			// it were whole: say so.
			kept = append(kept, tgapi.Plain(fmt.Sprintf("... %d more block(s) not shown.", len(body)-len(kept))))
			break
		}
		kept = append(kept, t)
		used += t.Len() + 1
	}
	var b tgapi.Builder
	b.Text(join(head, kept)).Plain("\n").Text(tail.Done())
	return b.Done()
}

// pendingText is the head (who wants to do what, and until when) and the
// detail blocks of a waiting request.
func (a *Approver) pendingText(r approval.Request) (tgapi.Text, []tgapi.Text) {
	var b tgapi.Builder
	if r.RunsOnTimeout {
		b.Bold("A change did not come up. Undo it?")
		b.Plain("\nLatch will " + a.phrase(r, false) + " by itself at ")
		b.Text(a.timeText(r.ExpiresAt, false))
		b.Plain(" unless you deny it.")
	} else {
		b.Bold(a.who(r.RequestedBy.UID) + " wants to " + a.phrase(r, false))
		b.Plain("\nExpires ")
		b.Text(a.timeText(r.ExpiresAt, false))
		b.Plain(fmt.Sprintf(" · request %d", r.ID))
	}
	return b.Done(), a.details(r)
}

// phrase says what r does in plain words, "restart container jellyfin",
// or in the past tense, "restarted container jellyfin".
func (a *Approver) phrase(r approval.Request, past bool) string {
	name := clean(r.Resource.Name)
	thing := clean(r.Resource.Type) + " " + name
	if r.Resource.Type == "unit" {
		thing = name
	}
	verb := func(now, done string) string {
		if past {
			return done
		}
		return now
	}
	switch r.Action {
	case "docker_restart", "systemd_restart":
		return verb("restart ", "restarted ") + thing
	case "docker_stop", "systemd_stop":
		return verb("stop ", "stopped ") + thing
	case "docker_start", "systemd_start":
		return verb("start ", "started ") + thing
	case "docker_compose_up":
		return verb("recreate ", "recreated ") + thing + " with docker compose"
	case "file_edit":
		return verb("edit ", "edited ") + name
	case "file_create":
		return verb("create ", "created ") + name
	case "file_restore":
		if r.RunsOnTimeout {
			return verb("undo the change to ", "undid the change to ") + name
		}
		return verb("restore ", "restored ") + name + " from a backup"
	case "http_request":
		method := "an HTTP"
		for _, p := range r.Params {
			if p.Name == "method" {
				method = "a " + clean(p.Value)
			}
		}
		return verb("send ", "sent ") + method + " request to " + name
	}
	return verb("run ", "ran ") + clean(r.Action) + " on " + thing
}

func (a *Approver) who(uid uint32) string {
	if a.o.AgentName != "" && a.o.AgentUID != nil && *a.o.AgentUID == uid {
		return a.o.AgentName
	}
	return fmt.Sprintf("uid %d", uid)
}

// details turns Latch's description lines into blocks: a unified diff and
// file content become code blocks, everything else stays plain. Params that
// the description already states (the path, the mode) are left out.
func (a *Approver) details(r approval.Request) []tgapi.Text {
	lines := make([]string, len(r.Details))
	for i, d := range r.Details {
		lines[i] = clean(d)
	}
	var out []tgapi.Text
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		switch {
		case strings.HasPrefix(l, "--- ") && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "+++ "):
			var diff []string
			for i += 2; i < len(lines) && isDiffLine(lines[i]); i++ {
				diff = append(diff, lines[i])
			}
			i--
			var b tgapi.Builder
			out = append(out, b.Pre(strings.Join(diff, "\n"), "diff").Done())
		case l == "content:":
			var body []string
			for i++; i < len(lines) && (strings.HasPrefix(lines[i], "  ") || lines[i] == ""); i++ {
				body = append(body, strings.TrimPrefix(lines[i], "  "))
			}
			i--
			var b tgapi.Builder
			b.Plain("Content:\n").Pre(strings.Join(body, "\n"), "")
			out = append(out, b.Done())
		default:
			out = append(out, tgapi.Plain(l))
		}
	}
	all := strings.Join(lines, "\n")
	for _, p := range r.Params {
		if v := clean(p.Value); v != "" && strings.Contains(all, v) {
			continue
		}
		out = append(out, tgapi.Plain(clean(p.Name)+": "+clean(p.Value)))
	}
	return out
}

func isDiffLine(l string) bool {
	return l == "" || strings.HasPrefix(l, "@@") || l[0] == ' ' || l[0] == '-' || l[0] == '+' || strings.HasPrefix(l, "... ")
}

// outcome is the message for a request that is no longer pending: what
// happened in one line, the result folded away.
func (a *Approver) outcome(r approval.Request) tgapi.Text {
	var b tgapi.Builder
	what := a.phrase(r, false)
	switch {
	case r.Status == approval.StatusDone && r.DecidedBy == nil && r.RunsOnTimeout:
		b.Bold("Done: " + a.phrase(r, true)).Plain(" (nobody decided in time)")
	case r.Status == approval.StatusDone:
		b.Bold("Done: " + a.phrase(r, true))
	case r.Status == approval.StatusFailed:
		b.Bold("Failed: " + what)
	case r.Status == approval.StatusDenied:
		b.Bold("Denied: " + what)
	case r.Status == approval.StatusExpired:
		b.Bold("Expired: " + what).Plain(" (nobody decided)")
	default:
		b.Bold("Approved, working on it: " + what)
	}
	return fit(a.withResult(b.Done(), r))
}

// notice is the one-off message for an automatic change.
func (a *Approver) notice(r approval.Request) tgapi.Text {
	var b tgapi.Builder
	who := a.who(r.RequestedBy.UID)
	if r.Status == approval.StatusDone {
		b.Bold(who + " " + a.phrase(r, true))
	} else {
		b.Bold(who + " tried to " + a.phrase(r, false) + ", but it " + string(r.Status))
	}
	return fit(a.withResult(b.Done(), r))
}

// withResult adds the result (summary and detail lines) to head, folded.
func (a *Approver) withResult(head tgapi.Text, r approval.Request) tgapi.Text {
	if r.Result == nil {
		return head
	}
	var body []tgapi.Text
	body = append(body, tgapi.Plain(clean(r.Result.Summary)))
	if len(r.Result.Details) > 0 {
		lines := make([]string, len(r.Result.Details))
		for i, d := range r.Result.Details {
			lines[i] = clean(d)
		}
		var b tgapi.Builder
		body = append(body, b.Pre(strings.Join(lines, "\n"), "").Done())
	}
	return join(head, body)
}

// fit cuts a message that is too long; the full result is in latch requests.
func fit(t tgapi.Text) tgapi.Text {
	if t.Len() <= tgapi.MaxText {
		return t
	}
	var b tgapi.Builder
	b.Text(tgapi.SplitText(t, tgapi.MaxText-60)[0])
	b.Italic("\n... cut; see latch requests on the server")
	return b.Done()
}

func (a *Approver) clock(t time.Time) string {
	return t.In(a.o.Location).Format("15:04")
}

// timeText is t as a time the reader's app shows in their own time zone;
// the text under it (for apps that cannot) uses Location. bold keeps it
// bold inside a bold title.
func (a *Approver) timeText(t time.Time, bold bool) tgapi.Text {
	var b tgapi.Builder
	b.Time(a.clock(t), t, "t")
	if !bold {
		return b.Done()
	}
	out := b.Done()
	out.Entities = append([]tgapi.Entity{{Type: "bold", Offset: 0, Length: out.Len()}}, out.Entities...)
	return out
}

func clean(s string) string { return textsafe.Line(s) }

func (a *Approver) saveLocked() error {
	// Forget the oldest finished requests beyond keepFinished.
	var finished []int64
	for id, s := range a.state.Requests {
		if s.Final {
			finished = append(finished, id)
		}
	}
	if len(finished) > keepFinished {
		slices.Sort(finished)
		for _, id := range finished[:len(finished)-keepFinished] {
			delete(a.state.Requests, id)
		}
	}
	b, err := json.Marshal(a.state)
	if err != nil {
		return err
	}
	return WriteFile(a.o.StatePath, b)
}

// WriteFile replaces path with b atomically, readable by its owner only.
func WriteFile(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func isFinal(st approval.Status) bool {
	switch st {
	case approval.StatusDone, approval.StatusFailed, approval.StatusDenied, approval.StatusExpired:
		return true
	}
	return false
}

// hashText identifies what a message shows, formatting included.
func hashText(t tgapi.Text) string {
	b, _ := json.Marshal(t)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// fingerprint identifies what a request asks for. Pressing a button checks
// that the request still has the fingerprint the message was made from.
func fingerprint(r approval.Request) string {
	b, _ := json.Marshal(struct {
		ID       int64
		Action   string
		Resource approval.Resource
		Params   []approval.Param
		Details  []string
		Created  time.Time
		Undo     bool
	}{r.ID, r.Action, r.Resource, r.Params, r.Details, r.CreatedAt.UTC(), r.RunsOnTimeout})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:6])
}

// data is a button's callback data: "a:ID:FP" (approve) or "d:ID:FP" (deny).
func data(approve bool, id int64, fp string) string {
	v := "d:"
	if approve {
		v = "a:"
	}
	return v + strconv.FormatInt(id, 10) + ":" + fp
}

func parseData(s string) (approve bool, id int64, fp string, ok bool) {
	verb, rest, ok := strings.Cut(s, ":")
	if !ok || (verb != "a" && verb != "d") {
		return false, 0, "", false
	}
	ids, fp, ok := strings.Cut(rest, ":")
	if !ok || len(fp) != 12 {
		return false, 0, "", false
	}
	id, err := strconv.ParseInt(ids, 10, 64)
	if err != nil || id < 1 {
		return false, 0, "", false
	}
	return verb == "a", id, fp, true
}
