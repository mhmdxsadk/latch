// Package health runs DNS checks against operator-configured hostnames.
//
// The agent never supplies a hostname: it names a target, and policy returns
// the configured value. Failures are results, not errors, and use a fixed
// vocabulary so no resolver error text (which can carry internal detail)
// reaches the agent.
package health

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/mhmdxsadk/latch/internal/config"
)

// Failure is a fixed, agent-safe description of why a check did not pass.
type Failure string

const (
	FailureNone     Failure = ""
	FailureTimeout  Failure = "timeout"
	FailureNotFound Failure = "not_found"
	FailureDNS      Failure = "dns_error"
)

// DNSResult is the outcome of a DNS check.
type DNSResult struct {
	OK         bool     `json:"ok"`
	Addresses  []string `json:"addresses"`
	Failure    Failure  `json:"failure,omitempty"`
	DurationMS int64    `json:"duration_ms"`
}

// Checker is the interface the MCP layer uses; tests substitute a fake.
// Targets have already been resolved by policy.
type Checker interface {
	DNS(ctx context.Context, t config.DNSTarget) (DNSResult, error)
}

// Resolver looks up the addresses of a host.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Prober is the real Checker. The zero value uses the system resolver; the
// field exists for tests.
type Prober struct {
	Resolver Resolver
}

var _ Checker = Prober{}

func (p Prober) resolver() Resolver {
	if p.Resolver != nil {
		return p.Resolver
	}
	return net.DefaultResolver
}

// DNS resolves the target host and reports its addresses.
func (p Prober) DNS(ctx context.Context, t config.DNSTarget) (DNSResult, error) {
	ctx, cancel := context.WithTimeout(ctx, config.DefaultCheckTimeout)
	defer cancel()
	start := time.Now()
	addrs, err := p.resolver().LookupNetIP(ctx, "ip", t.Host)
	res := DNSResult{Addresses: []string{}, DurationMS: time.Since(start).Milliseconds()}
	if err == nil && len(addrs) == 0 {
		err = &net.DNSError{Err: "no addresses", Name: t.Host, IsNotFound: true}
	}
	if err != nil {
		res.Failure = classifyDNS(err)
		return res, nil
	}
	for _, a := range addrs {
		res.Addresses = append(res.Addresses, a.Unmap().String())
	}
	slices.Sort(res.Addresses)
	res.Addresses = slices.Compact(res.Addresses)
	res.OK = true
	return res, nil
}

func classifyDNS(err error) Failure {
	var de *net.DNSError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return FailureTimeout
	case errors.As(err, &de) && de.IsTimeout:
		return FailureTimeout
	case errors.As(err, &de) && de.IsNotFound:
		return FailureNotFound
	default:
		return FailureDNS
	}
}
