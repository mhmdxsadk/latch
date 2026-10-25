package netcheck

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/mhmdxsadk/latch/internal/redact"
	"github.com/mhmdxsadk/latch/internal/textsafe"
)

// DNS bounds.
const (
	MaxDNSAnswers  = 64
	MaxAnswerBytes = 512
	dnsTimeout     = 5 * time.Second
)

// RecordTypes are the DNS record types net_dns accepts. PTR takes an IP.
var RecordTypes = []string{"A", "AAAA", "CNAME", "MX", "TXT", "NS", "SRV", "PTR"}

// DNSResult is the outcome of net_dns.
type DNSResult struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Answers    []string `json:"answers"`
	Truncated  bool     `json:"truncated"`
	Failure    Failure  `json:"failure,omitempty"`
	DurationMS int64    `json:"duration_ms"`
}

// DNS looks up name with the system resolver. name must have passed
// Policy.DNSQueryName (or, for PTR, Policy.NetworkHost and be an IP).
func (p *Prober) DNS(ctx context.Context, name, recordType string) (DNSResult, error) {
	if !slices.Contains(RecordTypes, recordType) {
		return DNSResult{}, ErrInvalid
	}
	g, err := p.guard(ctx)
	if err != nil {
		return DNSResult{}, err
	}
	var ptrAddr netip.Addr
	if recordType == "PTR" {
		a, err := netip.ParseAddr(name)
		if err != nil {
			return DNSResult{}, ErrInvalid
		}
		ptrAddr = a.Unmap().WithZone("")
		if !g.allowed(ptrAddr, 0) {
			return DNSResult{}, ErrNotAllowed
		}
	}

	ctx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()
	start := time.Now()
	answers, err := p.lookup(ctx, name, recordType, ptrAddr)
	res := DNSResult{Name: name, Type: recordType, Answers: []string{}, DurationMS: time.Since(start).Milliseconds()}
	if err != nil {
		res.Failure = classifyDNS(err)
		return res, nil
	}
	if len(answers) > MaxDNSAnswers {
		answers, res.Truncated = answers[:MaxDNSAnswers], true
	}
	for _, a := range answers {
		res.Answers = append(res.Answers, safeText(a, MaxAnswerBytes))
	}
	return res, nil
}

func (p *Prober) lookup(ctx context.Context, name, recordType string, ptrAddr netip.Addr) ([]string, error) {
	r := p.resolver
	var out []string
	switch recordType {
	case "A", "AAAA":
		network := "ip4"
		if recordType == "AAAA" {
			network = "ip6"
		}
		addrs, err := r.LookupNetIP(ctx, network, name)
		if err != nil {
			return nil, err
		}
		for _, a := range addrs {
			out = append(out, a.Unmap().WithZone("").String())
		}
	case "CNAME":
		c, err := r.LookupCNAME(ctx, name)
		if err != nil {
			return nil, err
		}
		out = []string{c}
	case "MX":
		mx, err := r.LookupMX(ctx, name)
		if err != nil {
			return nil, err
		}
		for _, m := range mx {
			out = append(out, fmt.Sprintf("%d %s", m.Pref, m.Host))
		}
	case "TXT":
		txt, err := r.LookupTXT(ctx, name)
		if err != nil {
			return nil, err
		}
		out = txt
	case "NS":
		ns, err := r.LookupNS(ctx, name)
		if err != nil {
			return nil, err
		}
		for _, n := range ns {
			out = append(out, n.Host)
		}
	case "SRV":
		_, srv, err := r.LookupSRV(ctx, "", "", name)
		if err != nil {
			return nil, err
		}
		for _, s := range srv {
			out = append(out, fmt.Sprintf("%d %d %d %s", s.Priority, s.Weight, s.Port, s.Target))
		}
	case "PTR":
		names, err := r.LookupAddr(ctx, ptrAddr.String())
		if err != nil {
			return nil, err
		}
		out = names
	}
	return out, nil
}

// safeText makes network text safe for the agent: one line, no escapes or
// invisible characters, secret-looking values hidden, at most n bytes.
// Redaction runs before the cut so a secret cannot escape it by straddling
// the limit.
func safeText(s string, n int) string {
	return textsafe.Head(redact.Text(textsafe.Line(s)), n)
}
