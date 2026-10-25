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
	Send(ctx context.Context, chat int64, text string, kb tgapi.Keyboard) (tgapi.Message, error)
	Edit(ctx context.Context, chat, message int64, text string, kb tgapi.Keyboard) error
	Answer(ctx context.Context, callbackID, text string) error
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
	Location  *time.Location
	Logger    *slog.Logger
	// OnFinished is called once when a request the owner was asked about
	// ends, however it was decided. The chat bridge uses it to tell the
	// agent the outcome. It must not block.
	OnFinished func(approval.Request)
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
			text, kb := a.render(r)
			m, err := a.o.Bot.Send(ctx, a.o.Chat, text, kb)
			if err != nil {
				a.o.Logger.Warn("announcing a request failed", "id", r.ID, "err", err)
				return // tried again next round
			}
			a.state.Requests[r.ID] = &shown{Message: m.MessageID, Text: hash(text)}
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
	text, kb := a.render(r)
	if hash(text) != s.Text {
		if err := a.o.Bot.Edit(ctx, a.o.Chat, s.Message, text, kb); err != nil {
			a.o.Logger.Warn("updating a request message failed", "id", r.ID, "err", err)
			return
		}
		s.Text = hash(text)
	}
	if final {
		s.Final = true
		if a.o.OnFinished != nil && r.NeedsApproval {
			a.o.OnFinished(r)
		}
	}
}

// goneLocked marks a shown request Latch no longer knows: Latch keeps
// pending requests in memory only, so a restart forgets them.
func (a *Approver) goneLocked(ctx context.Context, id int64, s *shown) {
	text := fmt.Sprintf("Latch request %d is gone: Latch no longer knows it (it restarted, or the request was forgotten). Nothing will run.", id)
	if err := a.o.Bot.Edit(ctx, a.o.Chat, s.Message, text, nil); err != nil {
		a.o.Logger.Warn("updating a request message failed", "id", id, "err", err)
		return
	}
	s.Text, s.Final = hash(text), true
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

// render is the message for r and its buttons (nil when it is decided).
func (a *Approver) render(r approval.Request) (string, tgapi.Keyboard) {
	if r.Status != approval.StatusPending {
		return a.outcome(r), nil
	}
	head, body := a.pendingText(r)
	fp := fingerprint(r)
	deny := tgapi.Button{Text: "Deny", Data: data(false, r.ID, fp)}
	full := head + "\n\n" + strings.Join(body, "\n")
	if tgapi.TextLen(full) <= tgapi.MaxText {
		return full, tgapi.Keyboard{{{Text: "Approve", Data: data(true, r.ID, fp)}, deny}}
	}
	return a.shortened(r, head, body), tgapi.Keyboard{{deny}}
}

func (a *Approver) tooLong(r approval.Request) bool {
	head, body := a.pendingText(r)
	return tgapi.TextLen(head+"\n\n"+strings.Join(body, "\n")) > tgapi.MaxText
}

// shortened shows as much of a long change as fits and says where to read
// and approve the rest.
func (a *Approver) shortened(r approval.Request, head string, body []string) string {
	tail := fmt.Sprintf("\n... %%d more lines not shown.\nThis change is too long to show here in full, so it has no Approve button. Read and approve it on the server: latch pending, then latch approve %d", r.ID)
	budget := tgapi.MaxText - tgapi.TextLen(head) - tgapi.TextLen(tail) - 16
	var b strings.Builder
	n := 0
	for _, l := range body {
		if tgapi.TextLen(b.String())+tgapi.TextLen(l)+1 > budget {
			break
		}
		b.WriteString("\n" + l)
		n++
	}
	return head + "\n" + b.String() + fmt.Sprintf(tail, len(body)-n)
}

func (a *Approver) pendingText(r approval.Request) (string, []string) {
	var head string
	if r.RunsOnTimeout {
		head = fmt.Sprintf("Latch undo request %d\n%s on %s %s\nFiled by Latch because a change did not come up. RUNS BY ITSELF at %s unless you deny it.",
			r.ID, clean(r.Action), clean(r.Resource.Type), clean(r.Resource.Name), a.clock(r.ExpiresAt))
	} else {
		head = fmt.Sprintf("Latch request %d needs your approval\n%s on %s %s\nAsked by uid %d, expires at %s.",
			r.ID, clean(r.Action), clean(r.Resource.Type), clean(r.Resource.Name), r.RequestedBy.UID, a.clock(r.ExpiresAt))
	}
	var body []string
	for _, d := range r.Details {
		body = append(body, clean(d))
	}
	for _, p := range r.Params {
		body = append(body, clean(p.Name)+": "+clean(p.Value))
	}
	return head, body
}

// outcome is the message for a request that is no longer pending.
func (a *Approver) outcome(r approval.Request) string {
	how := ""
	switch {
	case r.DecidedBy == nil && r.RunsOnTimeout && r.Status != approval.StatusExpired:
		how = " (nobody decided in time, so it ran by itself)"
	case r.DecidedVia == approval.ViaTelegram:
		how = " (decided in Telegram)"
	case r.DecidedBy != nil:
		how = " (decided on the server)"
	}
	status := string(r.Status)
	switch r.Status {
	case approval.StatusApproved, approval.StatusRunning:
		status = "approved, running"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Latch request %d: %s%s\n%s on %s %s", r.ID, status, how, clean(r.Action), clean(r.Resource.Type), clean(r.Resource.Name))
	writeResult(&b, r)
	return fit(b.String())
}

// notice is the one-off message for an automatic change.
func (a *Approver) notice(r approval.Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Latch ran request %d automatically: %s on %s %s, %s", r.ID, clean(r.Action), clean(r.Resource.Type), clean(r.Resource.Name), r.Status)
	writeResult(&b, r)
	return fit(b.String())
}

func writeResult(b *strings.Builder, r approval.Request) {
	if r.Result == nil {
		return
	}
	fmt.Fprintf(b, "\nresult: %s", clean(r.Result.Summary))
	for _, d := range r.Result.Details {
		fmt.Fprintf(b, "\n  %s", clean(d))
	}
}

// fit cuts text that is too long for one message.
func fit(s string) string {
	if tgapi.TextLen(s) <= tgapi.MaxText {
		return s
	}
	return tgapi.Split(s, tgapi.MaxText-40)[0] + "\n... (cut; see latch requests)"
}

func (a *Approver) clock(t time.Time) string {
	return t.In(a.o.Location).Format("15:04")
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

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
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
