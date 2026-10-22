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
