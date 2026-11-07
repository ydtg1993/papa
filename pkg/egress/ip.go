package egress

import "net/netip"

var deniedNetworks = func() []netip.Prefix {
	var out []netip.Prefix
	for _, raw := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "::/96", "::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10", "ff00::/8"} {
		out = append(out, netip.MustParsePrefix(raw))
	}
	return out
}()

// PublicIP reports whether an address is globally routable under the default policy.
func PublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range deniedNetworks {
		if p.Contains(ip) {
			return false
		}
	}
	// Currently assigned globally routed IPv6 space; reserved future ranges
	// fail closed until a host explicitly authorizes that origin.
	return !ip.Is6() || netip.MustParsePrefix("2000::/3").Contains(ip)
}
