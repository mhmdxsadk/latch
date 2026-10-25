package netcheck

import (
	"context"
	"math"
	"regexp"
	"strconv"
	"time"
)

// Ping bounds.
const (
	DefaultPingCount = 3
	MaxPingCount     = 5
	pingWaitSeconds  = 2
)

// PingResult is the outcome of net_ping.
type PingResult struct {
	Address     string  `json:"address"`
	Sent        int     `json:"sent"`
	Received    int     `json:"received"`
	LossPercent float64 `json:"loss_percent"`
	MinMS       float64 `json:"min_ms"`
	AvgMS       float64 `json:"avg_ms"`
	MaxMS       float64 `json:"max_ms"`
	Failure     Failure `json:"failure,omitempty"`
}

var (
	pingStats = regexp.MustCompile(`(?m)^(\d+) packets transmitted, (\d+) (?:packets )?received`)
	pingRTT   = regexp.MustCompile(`(?m)^(?:rtt|round-trip) min/avg/max/(?:mdev|stddev) = ([\d.]+)/([\d.]+)/([\d.]+)/`)
)

// Ping sends count ICMP echo requests to the first address of host by
// running the system ping with the checked IP literal.
func (p *Prober) Ping(ctx context.Context, host string, count int) (PingResult, error) {
	if count < 1 || count > MaxPingCount {
		return PingResult{}, ErrInvalid
	}
	g, err := p.guard(ctx)
	if err != nil {
		return PingResult{}, err
	}
	path := findProgram(p.pingPaths)
	if path == "" {
		return PingResult{}, ErrUnsupported
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	addrs, failure, err := g.resolve(rctx, host, 0)
	cancel()
	if err != nil || failure != FailureNone {
		return PingResult{Failure: failure}, err
	}
	a := addrs[0]
	res := PingResult{Address: a.String()}

	deadline := count + pingWaitSeconds
	ctx, cancel = context.WithTimeout(ctx, time.Duration(deadline+3)*time.Second)
	defer cancel()
	args := []string{familyFlag(a.Is4()), "-n", "-q",
		"-c", strconv.Itoa(count), "-W", strconv.Itoa(pingWaitSeconds), "-w", strconv.Itoa(deadline),
		"--", a.String()}
	stdout, stderr, runErr := p.run(ctx, path, args)
	if !parsePing(stdout, &res) {
		switch {
		case ctx.Err() != nil:
			res.Failure = FailureTimeout
		case permissionDenied(stderr):
			return PingResult{}, ErrUnsupported
		case unreachable(stderr):
			res.Failure = FailureUnreachable
		case runErr != nil:
			return PingResult{}, runErr
		default:
			return PingResult{}, errBadOutput
		}
	}
	return res, nil
}

// parsePing reads iputils ping's summary into res.
func parsePing(out []byte, res *PingResult) bool {
	m := pingStats.FindSubmatch(out)
	if m == nil {
		return false
	}
	sent, err1 := strconv.Atoi(string(m[1]))
	recv, err2 := strconv.Atoi(string(m[2]))
	if err1 != nil || err2 != nil || sent < 0 || recv < 0 || sent > MaxPingCount*2 || recv > sent {
		return false
	}
	res.Sent, res.Received = sent, recv
	if sent > 0 {
		res.LossPercent = math.Round(float64(sent-recv)*1000/float64(sent)) / 10
	}
	if r := pingRTT.FindSubmatch(out); r != nil {
		res.MinMS, res.AvgMS, res.MaxMS = ms(r[1]), ms(r[2]), ms(r[3])
	}
	return true
}

// ms parses a millisecond value, rounded to 0.001.
func ms(b []byte) float64 {
	f, err := strconv.ParseFloat(string(b), 64)
	if err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0
	}
	return math.Round(f*1000) / 1000
}
