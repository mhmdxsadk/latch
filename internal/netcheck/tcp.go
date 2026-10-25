package netcheck

import (
	"context"
	"math"
	"time"
)

// TCP bounds.
const (
	maxTCPAddrs    = 4
	tcpAttemptTime = 3 * time.Second
	tcpTotalTime   = 10 * time.Second
)

// TCP connection states.
const (
	StateOpen        = "open"
	StateRefused     = "refused"
	StateTimeout     = "timeout"
	StateUnreachable = "unreachable"
	StateError       = "error"
)

// TCPAttempt is one connection attempt.
type TCPAttempt struct {
	Address   string  `json:"address"`
	State     string  `json:"state"`
	LatencyMS float64 `json:"latency_ms"`
}

// TCPResult is the outcome of net_tcp.
type TCPResult struct {
	Port     int          `json:"port"`
	Open     bool         `json:"open"`
	Attempts []TCPAttempt `json:"attempts"`
	Failure  Failure      `json:"failure,omitempty"`
}

// TCP tries to connect to host:port, one address at a time until one
// accepts. Nothing is sent or read; the connection is closed at once.
func (p *Prober) TCP(ctx context.Context, host string, port int) (TCPResult, error) {
	if port < 1 || port > 65535 {
		return TCPResult{}, ErrInvalid
	}
	g, err := p.guard(ctx)
	if err != nil {
		return TCPResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, tcpTotalTime)
	defer cancel()
	res := TCPResult{Port: port, Attempts: []TCPAttempt{}}
	addrs, failure, err := g.resolve(ctx, host, uint16(port))
	if err != nil || failure != FailureNone {
		res.Failure = failure
		return res, err
	}
	if len(addrs) > maxTCPAddrs {
		addrs = addrs[:maxTCPAddrs]
	}
	for _, a := range addrs {
		start := time.Now()
		c, err := g.dial(ctx, a, uint16(port), tcpAttemptTime)
		at := TCPAttempt{Address: a.String(), LatencyMS: math.Round(float64(time.Since(start).Microseconds())) / 1000}
		if err == nil {
			c.Close()
			at.State = StateOpen
			res.Attempts = append(res.Attempts, at)
			res.Open = true
			return res, nil
		}
		if isNotAllowed(err) {
			return TCPResult{}, ErrNotAllowed
		}
		switch classifyConn(err) {
		case FailureRefused:
			at.State = StateRefused
		case FailureTimeout:
			at.State = StateTimeout
		case FailureUnreachable:
			at.State = StateUnreachable
		default:
			at.State = StateError
		}
		res.Attempts = append(res.Attempts, at)
		if ctx.Err() != nil {
			break
		}
	}
	return res, nil
}
