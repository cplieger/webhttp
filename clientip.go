package webhttp

import (
	"net"
	"net/http"
	"slices"
	"strings"
)

// ClientIP returns the best-effort client IP for r.
//
// The spoofing model is the point of this helper. X-Forwarded-For is set by
// clients and intermediaries and is trivially forgeable, so it is consulted
// ONLY when the immediate TCP peer (the host part of r.RemoteAddr) is a proxy
// you control, i.e. it falls inside one of the caller-supplied trusted ranges:
//
//   - With NO trusted ranges, or when the direct peer is not inside one, the
//     forwarded header is ignored entirely and the peer address (which cannot
//     be spoofed at this layer) is returned. This is the safe default: no
//     header a client sends can move the result off the real socket peer.
//   - When the peer IS a trusted proxy, X-Forwarded-For is walked from the
//     RIGHT. Each entry that is itself a trusted proxy is skipped (those are
//     your own hops, which appended the address they saw); the first untrusted
//     entry from the right is returned as the client. A malformed entry at that
//     boundary stops the walk, and the peer is returned.
//
// The right-to-left, skip-trusted walk is the only correct reading when the
// proxy APPENDS the peer it observed to X-Forwarded-For (as Caddy and most
// reverse proxies do): the LEFTMOST entry is then whatever the client SENT and
// is attacker-controlled, while the rightmost entries are the trustworthy hops.
// Consequently the trusted set must contain EVERY proxy hop between the client
// and this server; if a hop is missing from the set the walk stops there and
// that hop's address is returned as the client.
//
// X-Real-IP is deliberately NOT consulted: it is client-settable and a proxy
// such as Caddy does not overwrite it, so honoring it would reintroduce a spoof
// vector. It can return as an explicit opt-in if a proxy that overwrites it is
// ever adopted.
//
// The caller supplies the trusted CIDRs (typically the reverse proxy's address
// range); the library hardcodes none. A malformed r.RemoteAddr with no port is
// used verbatim as the host and, being unparseable, is never trusted.
func ClientIP(r *http.Request, trusted ...*net.IPNet) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr had no host:port form; use it as-is.
		host = r.RemoteAddr
	}

	peer := net.ParseIP(host)
	// Consult X-Forwarded-For only when the immediate peer is a trusted proxy.
	// With no trusted ranges (or an unparseable peer) this is always false, so
	// the socket peer is returned: the spoof-proof default.
	if peer == nil || !ipInTrusted(peer, trusted) {
		return host
	}

	if xff := strings.Join(r.Header.Values("X-Forwarded-For"), ","); xff != "" {
		// Values (not Get) so multiple X-Forwarded-For header LINES are treated
		// as the single comma-joined value RFC 7230 defines, closing a spoof gap
		// where a proxy that adds a separate line instead of comma-appending
		// would otherwise leave Get reading only the client's spoofed first line.
		// Walk right-to-left, skipping our own trusted hops; the first untrusted
		// entry from the right is the client.
		for _, part := range slices.Backward(strings.Split(xff, ",")) {
			ip := net.ParseIP(strings.TrimSpace(part))
			if ip == nil {
				break // malformed boundary: stop, trust nothing further left
			}
			if ipInTrusted(ip, trusted) {
				continue // our own proxy hop, keep walking left
			}
			return ip.String()
		}
	}
	// Trusted peer but no usable forwarded entry: fall back to the peer.
	return host
}

// ipInTrusted reports whether ip is contained in any of the given trusted
// networks.
func ipInTrusted(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

// ParseCIDRs parses trusted reverse-proxy entries into the *net.IPNet set that
// ClientIP and WithClientIP consult. Each entry is either CIDR notation
// ("10.0.0.0/8") or a bare IP address ("192.168.1.5", "::1"), a bare address
// being treated as a single host (/32 for IPv4, /128 for IPv6). Surrounding
// whitespace is trimmed and blank entries are skipped.
//
// It returns the successfully parsed networks and, separately, the list of
// entries that were neither a valid CIDR nor a valid IP. Returning both (rather
// than failing on the first bad entry) lets a strict caller reject the input —
// e.g. config-file validation surfacing the offending entry — while a lenient
// caller logs the bad entries and proceeds with the valid subset, e.g. reading
// a TRUSTED_PROXIES environment variable where one typo should not disable proxy
// awareness entirely. Both usages share this one parser instead of each app
// reimplementing the CIDR/bare-IP handling. The returned slice is passed
// straight to ClientIP(r, nets...) or WithClientIP(nets...); an empty result
// means "trust nothing", the spoof-proof default that logs the socket peer.
func ParseCIDRs(entries []string) (nets []*net.IPNet, invalid []string) {
	for _, e := range entries {
		s := strings.TrimSpace(e)
		if s == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(s); err == nil {
			nets = append(nets, n)
			continue
		}
		if ip := net.ParseIP(s); ip != nil {
			bits := 8 * net.IPv4len
			if ip.To4() == nil {
				bits = 8 * net.IPv6len
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		invalid = append(invalid, s)
	}
	return nets, invalid
}
