package health

import "net/netip"

var (
	// alwaysBlocked can never be probed, whatever the target configuration.
	alwaysBlocked = prefixes(
		"0.0.0.0/8",               // "this network"; 0.0.0.0 reaches localhost on Linux
		"169.254.169.254/32",      // cloud metadata (AWS, GCP, Azure, ...)
		"169.254.170.2/32",        // AWS ECS task metadata
		"100.100.100.200/32",      // Alibaba Cloud metadata
		"168.63.129.16/32",        // Azure WireServer
		"240.0.0.0/4",             // reserved, includes 255.255.255.255
		"fd00:ec2::254/128",       // AWS metadata over IPv6
		"::/128", "::ffff:0:0/96", // unspecified; IPv4-mapped are unmapped first
		"::ffff:0:0:0/96", // IPv4-translated
		"100::/64",        // discard-only
	)
	// privateRanges need allow_private. netip's IsLoopback, IsPrivate and
	// IsLinkLocalUnicast cover the common ranges; these are the rest.
	privateRanges = prefixes(
		"100.64.0.0/10",   // carrier-grade NAT
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"2001:db8::/32",   // documentation
		"fec0::/10",       // deprecated site-local
		// Tunnelling prefixes can embed an internal IPv4 address.
		"64:ff9b::/96", "64:ff9b:1::/48", // NAT64
		"2002::/16", // 6to4
		"2001::/32", // Teredo
	)
)

func prefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

func inAny(a netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

var (
	ipv4Compat = netip.MustParsePrefix("::/96") // deprecated IPv4-compatible
	nat64      = prefixes("64:ff9b::/96", "64:ff9b:1::/48")
	sixToFour  = netip.MustParsePrefix("2002::/16")
	teredo     = netip.MustParsePrefix("2001::/32")
)

// embeddedIPv4 returns the IPv4 addresses that tunnelling and translation
// prefixes carry inside an IPv6 address, so they get the same checks.
func embeddedIPv4(a netip.Addr) []netip.Addr {
	if !a.Is6() {
		return nil
	}
	b := a.As16()
	v4 := func(p []byte) netip.Addr { return netip.AddrFrom4([4]byte(p)) }
	switch {
	case inAny(a, nat64), ipv4Compat.Contains(a) && !a.IsLoopback():
		return []netip.Addr{v4(b[12:16])}
	case sixToFour.Contains(a):
		return []netip.Addr{v4(b[2:6])}
	case teredo.Contains(a):
		client := [4]byte{b[12] ^ 0xff, b[13] ^ 0xff, b[14] ^ 0xff, b[15] ^ 0xff}
		return []netip.Addr{v4(b[4:8]), netip.AddrFrom4(client)}
	}
	return nil
}

// Allowed reports whether an HTTP check may connect to a. Private-class
// addresses are permitted only when allowPrivate is set.
func Allowed(a netip.Addr, allowPrivate bool) bool {
	a = a.Unmap().WithZone("")
	if !a.IsValid() || a.IsUnspecified() || a.IsMulticast() || inAny(a, alwaysBlocked) {
		return false
	}
	// IPv4-compatible addresses other than ::1 have no legitimate use.
	if ipv4Compat.Contains(a) && !a.IsLoopback() {
		return false
	}
	for _, v4 := range embeddedIPv4(a) {
		if !v4.IsValid() || v4.IsUnspecified() || v4.IsMulticast() || inAny(v4, alwaysBlocked) {
			return false
		}
	}
	private := a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || inAny(a, privateRanges)
	return allowPrivate || !private
}
