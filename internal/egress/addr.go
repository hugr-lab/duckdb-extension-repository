// Package egress is the outbound HTTP client for requests kista makes on a tenant's behalf (issuer
// discovery, JWKS; upstreams later): only global unicast addresses, checked on the address actually
// dialed, no proxy from the environment, no redirects, bounded size and time (spec 0006).
package egress

import (
	"net/netip"
)

// special are address ranges that are never "the internet": the IANA special-purpose registries
// and more. An address in one of them is refused unless allowlisted.
var special = mustPrefixes(
	// IPv4
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
	"192.0.0.0/24", "192.0.2.0/24", "192.31.196.0/24", "192.52.193.0/24", "192.88.99.0/24", "192.168.0.0/16",
	"192.175.48.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"255.255.255.255/32",
	// IPv6
	// (NAT64 64:ff9b::/96 is not listed: its embedded IPv4 address decides)
	"::/128", "::1/128", "::ffff:0:0/96", "::ffff:0:0:0/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32",
	"2002::/16", "3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
)

// never are refused even when allowlisted: link-local (an IdP never lives there; cloud metadata
// does) and other cloud metadata and platform endpoints.
var never = mustPrefixes("169.254.0.0/16", "fe80::/10", "168.63.129.16/32", "100.100.100.200/32", "fd00:ec2::/112")

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// embedded returns the IPv4 addresses an IPv6 address carries (mapped, compatible, NAT64, 6to4,
// Teredo), which must pass the checks too.
func embedded(a netip.Addr) []netip.Addr {
	if !a.Is6() {
		return nil
	}
	b := a.As16()
	v4 := func(off int) netip.Addr { return netip.AddrFrom4([4]byte{b[off], b[off+1], b[off+2], b[off+3]}) }
	var out []netip.Addr
	switch {
	case a.Is4In6():
		out = append(out, a.Unmap())
	case b[0] == 0 && b[1] == 0 && b[2] == 0 && b[3] == 0 && b[4] == 0 && b[5] == 0 && b[6] == 0 && b[7] == 0 &&
		b[8] == 0 && b[9] == 0 && b[10] == 0 && b[11] == 0: // IPv4-compatible ::a.b.c.d
		out = append(out, v4(12))
	case netip.MustParsePrefix("64:ff9b::/96").Contains(a), netip.MustParsePrefix("64:ff9b:1::/48").Contains(a),
		netip.MustParsePrefix("::ffff:0:0:0/96").Contains(a): // NAT64 (well-known, local-use), SIIT
		out = append(out, v4(12))
	case netip.MustParsePrefix("2002::/16").Contains(a): // 6to4
		out = append(out, v4(2))
	case netip.MustParsePrefix("2001::/32").Contains(a): // Teredo: server, and the obfuscated client
		c := v4(12).As4()
		out = append(out, v4(4), netip.AddrFrom4([4]byte{^c[0], ^c[1], ^c[2], ^c[3]}))
	}
	return out
}

// Allow is one allowlist entry: a prefix, and the ports it may be reached on (empty: 443).
type Allow struct {
	Prefix netip.Prefix
	Ports  []uint16
}

// check decides whether addr:port may be dialed.
func (c *Client) check(addr netip.Addr, port uint16) bool {
	// a zoned address (fe80::1%eth0, fd00::1%1) never matches a prefix: refuse it outright
	if !addr.IsValid() || addr.Zone() != "" {
		return false
	}
	addr = addr.Unmap()
	all := append([]netip.Addr{addr}, embedded(addr)...)
	for _, a := range all {
		for _, p := range never {
			if p.Contains(a) {
				return false
			}
		}
	}
	if c.allowed(addr, port) {
		return true
	}
	if port != 443 {
		return false
	}
	for _, a := range all {
		if !a.IsGlobalUnicast() {
			return false
		}
		for _, p := range special {
			if p.Contains(a) {
				return false
			}
		}
	}
	return true
}

func (c *Client) allowed(addr netip.Addr, port uint16) bool {
	for _, al := range c.allow {
		if !al.Prefix.Contains(addr) {
			continue
		}
		if len(al.Ports) == 0 && port == 443 {
			return true
		}
		for _, p := range al.Ports {
			if p == port {
				return true
			}
		}
	}
	return false
}
