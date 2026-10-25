// Package audit records every tool invocation as one structured log line.
package audit

import (
	"context"
	"log/slog"
	"time"
)

// Outcome classifies how a call ended.
type Outcome string

const (
	OutcomeOK       Outcome = "ok"
	OutcomeDenied   Outcome = "denied"   // rejected by policy or validation
	OutcomeError    Outcome = "error"    // allowed, but the integration failed
	OutcomeRejected Outcome = "rejected" // peer failed credential check
)

// Logger writes audit records. It deliberately accepts only fixed, already
// validated fields: callers must never pass raw agent input or error text from
// integrations, since those can contain secrets.
type Logger struct{ l *slog.Logger }

// New wraps an slog.Logger (normally a JSON handler).
func New(l *slog.Logger) *Logger { return &Logger{l: l} }

// Discard returns a Logger that drops every record.
func Discard() *Logger { return New(slog.New(slog.DiscardHandler)) }

// Peer is the kernel-verified identity of the caller.
type Peer struct {
	UID uint32
	GID uint32
	PID int32
}

// Call records one tool invocation. resource is the validated resource name,
// or "" when the call did not target one (or the name was invalid).
func (a *Logger) Call(ctx context.Context, tool, resource string, outcome Outcome, reason string, d time.Duration) {
	a.l.LogAttrs(ctx, slog.LevelInfo, "tool_call",
		slog.String("tool", tool),
		slog.String("resource", resource),
		slog.String("outcome", string(outcome)),
		slog.String("reason", reason),
		slog.Duration("duration", d),
	)
}

// Connection records an accepted or rejected socket peer.
func (a *Logger) Connection(ctx context.Context, p Peer, outcome Outcome, reason string) {
	a.l.LogAttrs(ctx, slog.LevelInfo, "connection",
		slog.Uint64("uid", uint64(p.UID)),
		slog.Uint64("gid", uint64(p.GID)),
		slog.Int("pid", int(p.PID)),
		slog.String("outcome", string(outcome)),
		slog.String("reason", reason),
	)
}

// ActionRequested records a new change request. action and resource are
// validated identifiers; parameters are never logged, since later action
// kinds (file edits) carry content that may hold secrets.
func (a *Logger) ActionRequested(ctx context.Context, id int64, action, resource string, needsApproval bool, p Peer) {
	a.l.LogAttrs(ctx, slog.LevelInfo, "action_requested",
		slog.Int64("id", id),
		slog.String("action", action),
		slog.String("resource", resource),
		slog.Bool("needs_approval", needsApproval),
		slog.Uint64("uid", uint64(p.UID)),
		slog.Int("pid", int(p.PID)),
	)
}

// Decision events for ActionDecided.
const (
	ActionApproved = "action_approved"
	ActionDenied   = "action_denied"
	ActionExpired  = "action_expired"
	// ActionTimedOut is a request that runs because nobody decided in time.
	ActionTimedOut = "action_timed_out"
)

// ActionDecided records an approval, denial or expiry. uid is the approver
// and via the channel it used (a fixed word); both are empty for expiry.
func (a *Logger) ActionDecided(ctx context.Context, event string, id int64, uid uint32, via string) {
	attrs := []slog.Attr{slog.Int64("id", id), slog.Uint64("by_uid", uint64(uid))}
	if via != "" {
		attrs = append(attrs, slog.String("via", via))
	}
	a.l.LogAttrs(ctx, slog.LevelInfo, event, attrs...)
}

// ActionExecuted records the end of a change. outcome is a fixed word (done,
// failed) and reason a fixed category; never integration error text.
func (a *Logger) ActionExecuted(ctx context.Context, id int64, outcome, reason string, d time.Duration) {
	a.l.LogAttrs(ctx, slog.LevelInfo, "action_executed",
		slog.Int64("id", id),
		slog.String("outcome", outcome),
		slog.String("reason", reason),
		slog.Duration("duration", d),
	)
}

type peerKey struct{}

// WithPeer returns ctx carrying the kernel-verified peer of a connection.
func WithPeer(ctx context.Context, p Peer) context.Context {
	return context.WithValue(ctx, peerKey{}, p)
}

// PeerFromContext returns the peer stored by WithPeer, if any.
func PeerFromContext(ctx context.Context) (Peer, bool) {
	p, ok := ctx.Value(peerKey{}).(Peer)
	return p, ok
}
