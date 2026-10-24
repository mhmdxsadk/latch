package netcheck

import (
	"bufio"
	"bytes"
	"context"
	"net/netip"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Traceroute bounds.
const (
	DefaultMaxHops   = 20
	MaxHops          = 30
	traceTimeout     = 30 * time.Second
	traceWaitSeconds = 2
)

// Hop is one line of a traceroute. Address is empty when the hop did not
// answer. Hop addresses are reported, never checked.
type Hop struct {
	Hop     int     `json:"hop"`
	Address string  `json:"address"`
	RTTMS   float64 `json:"rtt_ms,omitempty"`
}

// TraceResult is the outcome of net_traceroute.
type TraceResult struct {
	Address string `json:"address"`
	Program string `json:"program"`
	Hops    []Hop  `json:"hops"`
	// TimedOut means the overall time limit ended the trace; Hops holds
	// what was seen until then.
	TimedOut bool    `json:"timed_out"`
	Failure  Failure `json:"failure,omitempty"`
}

var (
	// " 3  192.0.2.1  5.123 ms" or " 4  *" (traceroute -n -q 1).
	tracerouteLine = regexp.MustCompile(`^\s*(\d+)\s+(\S+)(?:\s+([\d.]+)\s*ms)?`)
	// " 3:  192.0.2.1   5.123ms" or " 4:  no reply" (tracepath -n).
	tracepathLine = regexp.MustCompile(`^\s*(\d+)\??:\s+(\S+(?: reply)?)(?:\s+([\d.]+)ms)?`)
)

// Traceroute runs the first available of traceroute and tracepath against
// the first address of host.
func (p *Prober) Traceroute(ctx context.Context, host string, maxHops int) (TraceResult, error) {
	if maxHops < 1 || maxHops > MaxHops {
		return TraceResult{}, ErrInvalid
	}
	g, err := p.guard(ctx)
	if err != nil {
		return TraceResult{}, err
	}
	path := findProgram(p.tracePaths)
	if path == "" {
		return TraceResult{}, ErrUnsupported
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	addrs, failure, err := g.resolve(rctx, host, 0)
	cancel()
	if err != nil || failure != FailureNone {
		return TraceResult{Hops: []Hop{}, Failure: failure}, err
	}
	a := addrs[0]
	prog := filepath.Base(path)
	res := TraceResult{Address: a.String(), Program: prog, Hops: []Hop{}}

	var args []string
	line := tracerouteLine
	if prog == "tracepath" {
		line = tracepathLine
		args = []string{familyFlag(a.Is4()), "-n", "-m", strconv.Itoa(maxHops), a.String()}
	} else {
		// Butskoy traceroute: UDP probes, one per hop, fixed wait. The
		// address is a canonical IP literal, so it cannot look like a flag.
		args = []string{familyFlag(a.Is4()), "-n", "-q", "1", "-w", strconv.Itoa(traceWaitSeconds),
			"-m", strconv.Itoa(maxHops), a.String()}
	}

	ctx, cancel = context.WithTimeout(ctx, traceTimeout)
	defer cancel()
	stdout, stderr, runErr := p.run(ctx, path, args)
	res.Hops = parseHops(stdout, line, maxHops)
	res.TimedOut = ctx.Err() != nil
	if len(res.Hops) == 0 && !res.TimedOut {
		switch {
		case permissionDenied(stderr):
			return TraceResult{}, ErrUnsupported
		case unreachable(stderr):
			res.Failure = FailureUnreachable
		case runErr != nil:
			return TraceResult{}, runErr
		}
	}
	return res, nil
}

// parseHops reads hop lines, keeping the first answer per hop. Addresses
// are re-formatted from a parsed IP, so no program text reaches the agent.
func parseHops(out []byte, line *regexp.Regexp, maxHops int) []Hop {
	hops := []Hop{}
	seen := map[int]int{} // hop number -> index in hops
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		m := line.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || n < 1 || n > maxHops {
			continue
		}
		h := Hop{Hop: n}
		if a, err := netip.ParseAddr(m[2]); err == nil {
			h.Address = a.Unmap().WithZone("").String()
			if m[3] != "" {
				h.RTTMS = ms([]byte(m[3]))
			}
		} else if m[2] != "*" && !strings.HasPrefix(m[2], "no") {
			continue // [LOCALHOST] and other non-hop lines
		}
		if i, ok := seen[n]; ok {
			if hops[i].Address == "" && h.Address != "" {
				hops[i] = h
			}
			continue
		}
		seen[n] = len(hops)
		hops = append(hops, h)
	}
	return hops
}
