package webhttp

import (
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

// ClientResolution is ResolveClient's answer: the client address and how it
// was found.
type ClientResolution struct {
	// IP is the client: the X-Forwarded-For entry the walk chose when
	// Forwarded, otherwise Peer.
	IP netip.Addr
	// Peer is the host of r.RemoteAddr, unmapped; the zero Addr when it is
	// not an IP address.
	Peer netip.Addr
	// Forwarded reports that IP came from X-Forwarded-For, which is read only
	// when Peer is trusted.
	Forwarded bool
	// UntrustedForwarding reports that a peer outside the trusted set sent a
	// Forwarded, X-Forwarded-For or X-Real-IP field holding a non-empty list
	// element, which was ignored.
	UntrustedForwarding bool
	// IgnoredPublic reports that such an ignored header named a public
	// address: global unicast and outside RFC 1918 and RFC 4193.
	IgnoredPublic bool
}

// ResolveClient resolves r's client against the trusted proxy prefixes, which
// must hold every hop, or the missing one is the client. Only a trusted peer
// has X-Forwarded-For read, from the right past trusted hops: a proxy appends
// the peer it saw, so the first untrusted entry is the client; a non-address
// entry or trusted hops only answer the peer. A field of over 64 entries, or a
// Forwarded element of over 16 pairs, is invalid whole and names no address.
// X-Real-IP and Forwarded never set IP: Caddy passes a client's copy through (https://caddyserver.com/docs/caddyfile/directives/reverse_proxy#defaults).
// Addresses compare unmapped, so a dual-stack IPv4 peer matches; a zone never does.
func ResolveClient(r *http.Request, trusted ...netip.Prefix) ClientResolution {
	peer, err := netip.ParseAddr(peerHost(r.RemoteAddr))
	if err == nil {
		peer = peer.Unmap()
	}
	res := ClientResolution{IP: peer, Peer: peer}
	fwd := parseForwarding(r.Header)
	if !inTrusted(peer, trusted) {
		res.UntrustedForwarding, res.IgnoredPublic = fwd.present(), fwd.public()
		return res
	}
	if ip, ok := fwd.client(trusted); ok {
		res.IP, res.Forwarded = ip, true
	}
	return res
}

// ClientIP returns ResolveClient's IP as text, for a log line or a rate-limit
// key; a RemoteAddr that is not an IP address gives "invalid IP", the zero
// Addr's text.
func ClientIP(r *http.Request, trusted ...netip.Prefix) string {
	return ResolveClient(r, trusted...).IP.String()
}

func peerHost(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// inTrusted reports whether ip, unmapped, is inside a prefix. The second
// Contains matches an IPv4-mapped prefix such as ::ffff:10.0.0.0/104.
func inTrusted(ip netip.Addr, trusted []netip.Prefix) bool {
	if !ip.IsValid() {
		return false
	}
	mapped := netip.AddrFrom16(ip.As16())
	for _, p := range trusted {
		if p.Contains(ip) || (ip.Is4() && p.Contains(mapped)) {
			return true
		}
	}
	return false
}

// maxForwardingHops bounds the list elements read from one forwarding field,
// far past any real proxy chain. A field holding more is refused at the next
// element, so the elements kept stay bounded; the byte scan up to that point
// stays linear in the header's length.
const maxForwardingHops = 64

// maxForwardedPairs bounds one Forwarded element's pairs. RFC 7239 registers
// four parameters (https://www.iana.org/assignments/http-parameters) and none
// may repeat in an element; the bound keeps the repeated-name check linear.
const maxForwardedPairs = 16

// forwarding is a request's forwarding fields, each parsed once by
// parseForwarding; nothing else parses X-Forwarded-For, X-Real-IP or
// Forwarded (ProxiedRequest only tests their presence). A field over
// maxForwardingHops holds one zero element, so it is present, names nothing
// and stops the walk; it is never read in part.
type forwarding struct {
	xff       []listEntry
	realIP    []listEntry
	forwarded []forwardedElement
}

// listEntry is one non-empty X-Forwarded-For or X-Real-IP list element.
type listEntry struct {
	// addr is the address the element names, unmapped, also when quoted,
	// bracketed or with a port, since neither header has a grammar; the zero
	// Addr when it names none or carries a zone.
	addr netip.Addr
	// bare reports that the element is an address alone, the only form the
	// trust walk reads.
	bare bool
}

// forwardedElement is one non-empty Forwarded list element.
type forwardedElement struct {
	// forNode is the for= node, unmapped; the zero Addr when the element is
	// invalid or its for= is absent, "unknown" or obfuscated.
	forNode netip.Addr
	valid   bool
}

func parseForwarding(h http.Header) forwarding {
	return forwarding{
		xff:       parseBounded(h, "X-Forwarded-For", tokenList, parseListEntry),
		realIP:    parseBounded(h, "X-Real-Ip", tokenList, parseListEntry),
		forwarded: parseBounded(h, "Forwarded", quotedStringList, parseForwardedElement),
	}
}

// client walks X-Forwarded-For from the right for the first untrusted entry;
// false when an entry before it is not an address alone, or there is none.
func (f forwarding) client(trusted []netip.Prefix) (netip.Addr, bool) {
	for _, e := range slices.Backward(f.xff) {
		if !e.bare {
			return netip.Addr{}, false
		}
		if !inTrusted(e.addr, trusted) {
			return e.addr, true
		}
	}
	return netip.Addr{}, false
}

func (f forwarding) present() bool {
	return len(f.xff) > 0 || len(f.realIP) > 0 || len(f.forwarded) > 0
}

func (f forwarding) public() bool {
	entryPublic := func(e listEntry) bool { return isPublic(e.addr) }
	return slices.ContainsFunc(f.xff, entryPublic) ||
		slices.ContainsFunc(f.realIP, entryPublic) ||
		slices.ContainsFunc(f.forwarded, func(e forwardedElement) bool {
			return e.valid && isPublic(e.forNode)
		})
}

func isPublic(ip netip.Addr) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate()
}

// parseBounded parses each element of h's name list with parse, or returns
// one zero element once the list holds more than maxForwardingHops.
func parseBounded[T any](h http.Header, name string, syntax listSyntax, parse func(string) T) []T {
	var elems []T
	for elem := range fieldList(h, name, syntax) {
		if len(elems) == maxForwardingHops {
			var zero T
			return []T{zero}
		}
		elems = append(elems, parse(elem))
	}
	return elems
}

func parseListEntry(elem string) listEntry {
	ip, err := netip.ParseAddr(elem)
	bare := err == nil
	if !bare {
		elem = strings.Trim(elem, `"`)
		if ap, portErr := netip.ParseAddrPort(elem); portErr == nil {
			ip = ap.Addr()
		} else {
			ip, _ = netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(elem, "["), "]"))
		}
	}
	if ip.Zone() != "" {
		return listEntry{}
	}
	return listEntry{addr: ip.Unmap(), bare: bare}
}

// parseForwardedElement parses an RFC 7239 section 4 forwarded-element:
// pairs of token "=" (token / quoted-string), separated by ";", with no
// whitespace outside a quoted-string. Names fold ASCII case and none may
// repeat, and an element holds at most maxForwardedPairs pairs. The for and
// by values must be section 6 nodes; an invalid element is the zero value.
func parseForwardedElement(elem string) forwardedElement {
	var forNode netip.Addr
	seen := make([]string, 0, maxForwardedPairs)
	for rest := elem; rest != ""; {
		if after, ok := strings.CutPrefix(rest, ";"); ok {
			rest = after
			continue
		}
		if len(seen) == maxForwardedPairs {
			return forwardedElement{}
		}
		name, value, after, ok := cutForwardedPair(rest)
		if !ok || slices.ContainsFunc(seen, func(s string) bool { return equalASCIIFold(s, name) }) {
			return forwardedElement{}
		}
		seen, rest = append(seen, name), after
		switch {
		case equalASCIIFold(name, "for"):
			forNode, ok = parseNode(value)
		case equalASCIIFold(name, "by"):
			_, ok = parseNode(value)
		}
		if !ok {
			return forwardedElement{}
		}
	}
	return forwardedElement{forNode: forNode, valid: true}
}

// cutForwardedPair reads one forwarded-pair from the front of s, its value
// decoded; false unless the pair is well formed and ends s or a ";" follows.
func cutForwardedPair(s string) (name, value, rest string, ok bool) {
	n := tokenLen(s)
	after, eq := strings.CutPrefix(s[n:], "=")
	if n == 0 || !eq {
		return "", "", "", false
	}
	if value, rest, ok = cutForwardedValue(after); !ok || (rest != "" && rest[0] != ';') {
		return "", "", "", false
	}
	return s[:n], value, rest, true
}

// cutForwardedValue reads a token or an RFC 9110 section 5.6.4 quoted-string
// from the front of s, returning it decoded and the rest of s.
func cutForwardedValue(s string) (value, rest string, ok bool) {
	if !strings.HasPrefix(s, `"`) {
		n := tokenLen(s)
		return s[:n], s[n:], n > 0
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			return b.String(), s[i+1:], true
		case c == '\\' && i+1 < len(s) && quotedPairByte(s[i+1]):
			i++
			b.WriteByte(s[i])
		case qdtextByte(c):
			b.WriteByte(c)
		default:
			return "", "", false
		}
	}
	return "", "", false
}

// parseNode parses an RFC 7239 section 6 node: an IPv4 address, a bracketed
// IPv6 one, "unknown" or an obfuscated name, then an optional decimal or
// obfuscated port. It returns the address, unmapped; the zero Addr for
// "unknown" and obfuscated names.
func parseNode(node string) (netip.Addr, bool) {
	name, port, hasPort := strings.Cut(node, ":")
	inner, bracketed := strings.CutPrefix(node, "[")
	if bracketed {
		var after string
		var closed bool
		if name, after, closed = strings.Cut(inner, "]"); !closed {
			return netip.Addr{}, false
		}
		if port, hasPort = strings.CutPrefix(after, ":"); !hasPort && after != "" {
			return netip.Addr{}, false
		}
	}
	if hasPort && !validNodePort(port) {
		return netip.Addr{}, false
	}
	if !bracketed && (equalASCIIFold(name, "unknown") || obfuscated(name)) {
		return netip.Addr{}, true
	}
	ip, err := netip.ParseAddr(name)
	if err != nil || ip.Is6() != bracketed || ip.Zone() != "" {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

func validNodePort(port string) bool {
	return obfuscated(port) || port != "" && len(port) <= 5 && strings.Trim(port, "0123456789") == ""
}

// obfuscated reports an RFC 7239 section 6.3 obfnode or obfport: "_" then one
// or more of ALPHA, DIGIT, ".", "_" and "-".
func obfuscated(s string) bool {
	rest, ok := strings.CutPrefix(s, "_")
	return ok && rest != "" && strings.Trim(rest, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._-") == ""
}

// ParseCIDRs parses trusted reverse-proxy entries into the prefix set
// ResolveClient, ClientIP and WithClientIP take. An entry is a CIDR, masked to
// its network, or a bare IP, a single-host prefix; an IPv4-mapped entry of 96
// bits or more becomes its IPv4 prefix, so both spellings of a network are one
// prefix. Entries are trimmed and blank ones skipped; one that parses as
// neither, a zoned address included, comes back in invalid, in input order, so
// a strict caller can reject the input and a lenient one can use the rest. An
// empty set trusts nothing.
func ParseCIDRs(entries []string) (prefixes []netip.Prefix, invalid []string) {
	for _, e := range entries {
		s := strings.TrimSpace(e)
		if s == "" {
			continue
		}
		if p, ok := parseTrustedPrefix(s); ok {
			prefixes = append(prefixes, p)
		} else {
			invalid = append(invalid, s)
		}
	}
	return prefixes, invalid
}

func parseTrustedPrefix(s string) (netip.Prefix, bool) {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		ip, addrErr := netip.ParseAddr(s)
		if addrErr != nil || ip.Zone() != "" {
			return netip.Prefix{}, false
		}
		p = netip.PrefixFrom(ip, ip.BitLen())
	}
	if ip := p.Addr(); ip.Is4In6() && p.Bits() >= 96 {
		p = netip.PrefixFrom(ip.Unmap(), p.Bits()-96)
	}
	return p.Masked(), true
}
