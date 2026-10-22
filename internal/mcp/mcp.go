// Package mcp exposes Latch's read-only operations as MCP tools.
//
// Every tool follows the same pipeline: the typed input is schema-validated by
// the SDK (unknown fields are rejected), names are resolved through policy,
// the integration runs, and the outcome is audited. Errors returned to the
// agent are limited to a fixed vocabulary; integration errors, which may carry
// secrets or host details, are logged as a category only.
package mcp

import (
	"context"
	"errors"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mhmdxsadk/latch/internal/audit"
	"github.com/mhmdxsadk/latch/internal/host"
	"github.com/mhmdxsadk/latch/internal/policy"
)

// Deps are the collaborators the tools need. Integrations are interfaces so
// tests can inject fakes.
type Deps struct {
	Policy  *policy.Policy
	Host    host.Provider
	Audit   *audit.Logger
	Version string
}

// userError is the only error type whose text reaches the agent.
type userError struct{ msg string }

func (e userError) Error() string { return e.msg }

// Safe messages. Deliberately do not echo agent input.
var (
	errInvalidName = userError{"invalid resource name"}
	errNotAllowed  = userError{"resource is not configured"}
	errUnavailable = userError{"operation failed; see the Latch audit log"}
	errUnsupported = userError{"not supported on this host"}
)

// classify maps any error to a safe user-facing error and an audit outcome.
func classify(err error) (userError, audit.Outcome, string) {
	var ue userError
	switch {
	case errors.As(err, &ue):
		return ue, audit.OutcomeDenied, "bad_request"
	case errors.Is(err, policy.ErrInvalidName):
		return errInvalidName, audit.OutcomeDenied, "invalid_name"
	case errors.Is(err, policy.ErrNotAllowed):
		return errNotAllowed, audit.OutcomeDenied, "not_allowed"
	case errors.Is(err, host.ErrUnsupported):
		return errUnsupported, audit.OutcomeError, "unsupported"
	default:
		return errUnavailable, audit.OutcomeError, "integration_error"
	}
}

// wrap adapts fn into an SDK handler with auditing and error sanitisation.
// resource extracts the audit label from the (already schema-validated) input;
// it must return "" unless the value passed policy.
func wrap[In, Out any](d *Deps, tool string, fn func(context.Context, In) (Out, string, error)) sdk.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
		start := time.Now()
		out, resource, err := fn(ctx, in)
		if err != nil {
			ue, outcome, reason := classify(err)
			d.Audit.Call(ctx, tool, resource, outcome, reason, time.Since(start))
			var zero Out
			return nil, zero, ue
		}
		d.Audit.Call(ctx, tool, resource, audit.OutcomeOK, "", time.Since(start))
		return nil, out, nil
	}
}

var readOnly = &sdk.ToolAnnotations{
	ReadOnlyHint:  true,
	OpenWorldHint: ptr(false),
}

func ptr[T any](v T) *T { return &v }

// NewServer builds the MCP server with all v0.1 tools registered.
func NewServer(d *Deps) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "latch", Version: d.Version}, &sdk.ServerOptions{
		Instructions: "Latch gives read-only visibility into one server. " +
			"Call list_resources first to see which named resources you may query. " +
			"Only configured names are accepted.",
	})
	registerResources(s, d)
	registerHost(s, d)
	return s
}
