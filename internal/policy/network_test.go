package policy

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/mhmdxsadk/latch/internal/config"
)

func netPolicy(t *testing.T) *Policy {
	t.Helper()
	c, err := config.Parse([]byte(`
server: {socket: /run/l.sock, socket_group: latch-agents}
network:
  exclude: [vault.internal, 'Admin.Example.', 10.0.0.5, 'fd00::5']
`))
	if err != nil {
		t.Fatal(err)
	}
	return New(c)
}

func TestNetworkHost(t *testing.T) {
	p := netPolicy(t)
	ok := map[string]string{
		"example.com": "example.com", "Example.COM.": "example.com", "localhost": "localhost",
		"127.0.0.1": "127.0.0.1", "::1": "::1", "::ffff:192.0.2.1": "192.0.2.1",
		"10.0.0.6": "10.0.0.6", "vault.internal.example": "vault.internal.example",
	}
	for in, want := range ok {
		if got, err := p.NetworkHost(in); err != nil || got != want {
			t.Errorf("NetworkHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"vault.internal", "VAULT.internal", "vault.internal.", "Vault.Internal.",
		"admin.example", "ADMIN.EXAMPLE.", "10.0.0.5", "::ffff:10.0.0.5", "fd00::5", "FD00:0::5",
	} {
		if _, err := p.NetworkHost(in); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("NetworkHost(%q) = %v, want ErrNotAllowed", in, err)
		}
	}
	for _, in := range []string{
		"", " ", "*.example.com", "a b", "http://x", "x:80", "[::1]", "fe80::1%eth0",
		"-x.com", "a..b", "_dmarc.example.com", "a/b", "x;id", strings.Repeat("a.", 127) + "aa",
		"exa\x00mple.com", "exаmple.com", "-n", "--help",
	} {
		if _, err := p.NetworkHost(in); !errors.Is(err, ErrInvalidName) {
			t.Errorf("NetworkHost(%q) = %v, want ErrInvalidName", in, err)
		}
	}
}

func TestDNSQueryName(t *testing.T) {
	p := netPolicy(t)
	for _, in := range []string{"_dmarc.example.com", "_imap._tcp.example.com", "sel._domainkey.example.com"} {
		if _, err := p.DNSQueryName(in); err != nil {
			t.Errorf("DNSQueryName(%q) = %v", in, err)
		}
	}
	if _, err := p.DNSQueryName("Vault.Internal."); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("excluded name: %v", err)
	}
	if _, err := p.DNSQueryName("*.example.com"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("wildcard: %v", err)
	}
}

func TestNetworkAddrExcluded(t *testing.T) {
	p := netPolicy(t)
	for s, want := range map[string]bool{
		"10.0.0.5": true, "::ffff:10.0.0.5": true, "fd00::5": true, "fd00::5%eth0": true,
		"10.0.0.6": false, "127.0.0.1": false,
	} {
		if got := p.NetworkAddrExcluded(netip.MustParseAddr(s)); got != want {
			t.Errorf("NetworkAddrExcluded(%s) = %v", s, got)
		}
	}
}
