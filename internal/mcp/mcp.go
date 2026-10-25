// Package mcp exposes Latch's operations as MCP tools.
//
// Every tool follows the same pipeline: the typed input is schema-validated by
// the SDK (unknown fields are rejected), names are resolved through policy,
// the integration runs, and the outcome is audited. Change tools (actions.go)
// do not change anything themselves: they build an approval.Action and hand
// it to the approval store, which runs it or waits for the owner. There is no
// tool that approves or denies; that only exists on the admin socket. Errors returned to the
// agent are limited to a fixed vocabulary; integration errors, which may carry
// secrets or host details, are logged as a category only.
package mcp

import (
	"context"
	"errors"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mhmdxsadk/latch/internal/approval"
	"github.com/mhmdxsadk/latch/internal/audit"
	"github.com/mhmdxsadk/latch/internal/docker"
	"github.com/mhmdxsadk/latch/internal/health"
	"github.com/mhmdxsadk/latch/internal/host"
	"github.com/mhmdxsadk/latch/internal/policy"
	"github.com/mhmdxsadk/latch/internal/systemd"
)

// Deps are the collaborators the tools need. Integrations are interfaces so
// tests can inject fakes.
type Deps struct {
	Policy  *policy.Policy
	Host    host.Provider
	Docker  docker.Client
	Systemd systemd.Client
	Health  health.Checker
	Audit   *audit.Logger
	Version string

	// Changes. Actions is the approval store; NewServer creates one if nil.
	Actions        *approval.Store
	DockerActions  docker.Mutator
	SystemdActions systemd.Mutator

	// sem bounds concurrent tool calls across all connections, so parallel
	// requests cannot multiply memory use, outbound probes or bus connections.
	sem chan struct{}
}

// MaxConcurrentCalls is the number of tool calls served at once; further
// calls are refused until one finishes.
const MaxConcurrentCalls = 16

// userError is the only error type whose text reaches the agent.
type userError struct{ msg string }

func (e userError) Error() string { return e.msg }

// Safe messages. Deliberately do not echo agent input.
var (
	errInvalidName = userError{"invalid resource name"}
	// errNotAllowed and errMissing read the same on purpose: an excluded
	// resource must not be distinguishable from one that does not exist. Only
	// the audit reason differs.
	errNotAllowed  = userError{"resource does not exist or is not allowed"}
	errUnavailable = userError{"operation failed; see the Latch audit log"}
	errUnsupported = userError{"not supported on this host"}
	errMissing     = userError{"resource does not exist or is not allowed"}
	errBusy        = userError{"too many concurrent requests; retry shortly"}
)

// errPartial lets a tool return its output while auditing that part of it
// failed. It never reaches the agent.
var errPartial = errors.New("partial result")

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
	case errors.Is(err, docker.ErrNotFound), errors.Is(err, systemd.ErrNotFound):
		return errMissing, audit.OutcomeError, "not_found"
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
		select {
		case d.sem <- struct{}{}:
			defer func() { <-d.sem }()
		default:
			d.Audit.Call(ctx, tool, "", audit.OutcomeDenied, "busy", time.Since(start))
			var zero Out
			return nil, zero, errBusy
		}
		out, resource, err := fn(ctx, in)
		if errors.Is(err, errPartial) {
			d.Audit.Call(ctx, tool, resource, audit.OutcomeError, "partial", time.Since(start))
			return nil, out, nil
		}
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

// NewServer builds the MCP server with all tools registered. Every
// integration in d must be non-nil.
func NewServer(d *Deps) *sdk.Server {
	d.sem = make(chan struct{}, MaxConcurrentCalls)
	if d.Actions == nil {
		d.Actions = approval.New(approval.Options{Audit: d.Audit, Explain: ExplainActionError})
	}
	s := sdk.NewServer(&sdk.Implementation{Name: "latch", Version: d.Version}, &sdk.ServerOptions{
		Instructions: "Latch gives visibility into one server and can make a few named changes. " +
			"Use docker_list and systemd_list to see containers and services, and list_resources for named disks and DNS checks. " +
			"Some resources may be excluded by the operator; they behave as if they do not exist. " +
			"Reading is automatic. Restarting a container or unit runs automatically within a limit; stopping, starting and " +
			"docker_compose_up wait for the server owner's approval. A change tool returns a request id: when its status is " +
			"\"pending\", tell the user the id and check action_status later. Never claim a change happened until " +
			"action_status says \"done\". There are no arbitrary commands.",
	})
	registerResources(s, d)
	registerHost(s, d)
	registerDocker(s, d)
	registerSystemd(s, d)
	registerHealth(s, d)
	registerActions(s, d)
	return s
}
