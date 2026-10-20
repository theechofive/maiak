package safehttp

import "net/netip"

// builtinBlocked are destinations the guard never connects to (ADR-0011, ADR-0016). Embedded
// IPv4 forms other than ::ffff:0:0/96 (which is unmapped before matching) are blocked as whole
// ranges rather than decoded: NAT64, 6to4 and Teredo can all reach an IPv4 address chosen by
// the tenant, and no legitimate monitor target needs them.
var builtinBlocked = mustPrefixes(
	// IPv4
	"0.0.0.0/8",       // "this network"
	"10.0.0.0/8",      // private
	"100.64.0.0/10",   // CGNAT
	"127.0.0.0/8",     // loopback
	"169.254.0.0/16",  // link-local, includes the cloud metadata endpoint
	"172.16.0.0/12",   // private
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation
	"192.88.99.0/24",  // 6to4 relay anycast
	"192.168.0.0/16",  // private
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // documentation
	"203.0.113.0/24",  // documentation
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved, includes broadcast 255.255.255.255
	// IPv6
	"::/128",         // unspecified
	"::1/128",        // loopback
	"::/96",          // IPv4-compatible (deprecated)
	"::ffff:0:0/96",  // IPv4-mapped (also unmapped before matching)
	"64:ff9b::/96",   // NAT64
	"64:ff9b:1::/48", // local-use NAT64
	"100::/64",       // discard
	"2001::/23",      // IETF protocol assignments, includes Teredo 2001::/32
	"2001:db8::/32",  // documentation
	"2002::/16",      // 6to4
	"3fff::/20",      // documentation
	"fc00::/7",       // unique local
	"fe80::/10",      // link-local
	"fec0::/10",      // site-local (deprecated)
	"ff00::/8",       // multicast
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// allowed reports whether the guard may connect to addr. Only global unicast addresses outside
// the built-in and extra ranges pass.
func allowed(addr netip.Addr, extra []netip.Prefix) bool {
	if !addr.IsValid() {
		return false
	}
	addr = addr.WithZone("")
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	if !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return false
	}
	for _, p := range builtinBlocked {
		if p.Contains(addr) {
			return false
		}
	}
	for _, p := range extra {
		if p.Contains(addr) {
			return false
		}
		// An IPv4 prefix given in mapped form (::ffff:10.0.0.0/104) must still match.
		if p.Addr().Is4In6() && p.Bits() >= 96 {
			if q, err := p.Addr().Unmap().Prefix(p.Bits() - 96); err == nil && q.Contains(addr) {
				return false
			}
		}
	}
	return true
}
