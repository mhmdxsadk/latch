package health

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mhmdxsadk/latch/internal/config"
)

// fakeResolver returns fixed answers and counts lookups.
type fakeResolver struct {
	addrs []netip.Addr
	err   error
	calls atomic.Int32
}

func (f *fakeResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	f.calls.Add(1)
	return f.addrs, f.err
}

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}

// Failure results must not carry raw error text, which can include internal
// hostnames or addresses.
func TestResultsCarryNoErrorText(t *testing.T) {
	p := Prober{Resolver: &fakeResolver{err: errors.New("lookup SECRET-internal.corp on 10.9.8.7:53: boom")}}
	d, err := p.DNS(context.Background(), config.DNSTarget{Name: "x", Host: "x.test"})
	b, _ := json.Marshal(d)
	if err != nil || d.Failure != FailureDNS || strings.Contains(string(b), "SECRET") || strings.Contains(string(b), "10.9.8.7") {
		t.Errorf("leaked: %s %v", b, err)
	}
}

func TestDNS(t *testing.T) {
	p := Prober{Resolver: &fakeResolver{addrs: addrs("2001:db8::1", "192.0.2.1", "::ffff:192.0.2.1", "192.0.2.1")}}
	res, err := p.DNS(context.Background(), config.DNSTarget{Name: "x", Host: "x.test"})
	if err != nil || !res.OK || strings.Join(res.Addresses, ",") != "192.0.2.1,2001:db8::1" {
		t.Errorf("got %+v, %v", res, err)
	}

	nx := Prober{Resolver: &fakeResolver{err: &net.DNSError{IsNotFound: true}}}
	res, _ = nx.DNS(context.Background(), config.DNSTarget{Host: "x.test"})
	if res.OK || res.Failure != FailureNotFound || res.Addresses == nil {
		t.Errorf("nxdomain: %+v", res)
	}
	to := Prober{Resolver: &fakeResolver{err: &net.DNSError{IsTimeout: true}}}
	if res, _ = to.DNS(context.Background(), config.DNSTarget{Host: "x.test"}); res.Failure != FailureTimeout {
		t.Errorf("timeout: %+v", res)
	}
}
