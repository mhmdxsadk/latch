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
	"github.com/mhmdxsadk/latch/internal/docker"
	"github.com/mhmdxsadk/latch/internal/health"
	"github.com/mhmdxsadk/latch/internal/host"
	"github.com/mhmdxsadk/latch/internal/netcheck"
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
	Net     netcheck.Checker
	Audit   *audit.Logger
	Version string

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
	// errTargetNotAllowed is the one message for every blocked network
	// target, whatever the reason.
	errTargetNotAllowed = userError{"target is not allowed"}
	errInvalidTarget    = userError{"invalid host, name or URL"}
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
	case errors.Is(err, netcheck.ErrNotAllowed):
		return errTargetNotAllowed, audit.OutcomeDenied, "not_allowed"
	case errors.Is(err, netcheck.ErrInvalid):
		return errInvalidTarget, audit.OutcomeDenied, "invalid_name"
	case errors.Is(err, netcheck.ErrUnknown):
		// Excluded containers are unknown, so nothing is reached.
		return errUnavailable, audit.OutcomeError, "excluded_unknown"
	case errors.Is(err, netcheck.ErrUnsupported):
		return errUnsupported, audit.OutcomeError, "unsupported"
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

// NewServer builds the MCP server with all v0.1 tools registered. Every
// integration in d must be non-nil.
func NewServer(d *Deps) *sdk.Server {
	d.sem = make(chan struct{}, MaxConcurrentCalls)
	s := sdk.NewServer(&sdk.Implementation{Name: "latch", Version: d.Version}, &sdk.ServerOptions{
		Instructions: "Latch gives read-only visibility into one server. " +
			"Use docker_list and systemd_list to see containers and services, and list_resources for named disks and DNS checks. " +
			"The net_* tools run network diagnostics from this server. " +
			"Some resources may be excluded by the operator; they behave as if they do not exist.",
	})
	registerResources(s, d)
	registerHost(s, d)
	registerDocker(s, d)
	registerSystemd(s, d)
	registerHealth(s, d)
	registerNetwork(s, d)
	return s
}
