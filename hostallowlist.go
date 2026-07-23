package webhttp

import (
	"net"
	"net/http"
	"strings"
)

// CanonicalHost parses an HTTP Host header value (or a configured allowlist
// entry) into its canonical host for exact comparison, returning "" when the
// value is not a well-formed authority. Accepted shapes, per the RFC 3986
// authority grammar:
//
//   - a DNS name of ASCII labels (letters, digits, hyphens, and underscores —
//     Docker Compose service names carry underscores — 63 bytes max per
//     label), optionally ending in one FQDN dot, optionally followed by one
//     ":port" (digits, at most 65535)
//   - an IPv4 literal, with the same optional trailing dot and :port
//   - a bare IPv6 literal (no port: an unbracketed colon-bearing value can
//     only be an address, never host:port)
//   - a bracketed IPv6 literal "[...]", optionally followed by ":port"
//
// The canonical form is lowercase with no port, no brackets, and no trailing
// dot; IP literals are rendered through net.ParseIP so textually different
// spellings of one address (::1 vs 0:0:0:0:0:0:0:1, [::ffff:192.0.2.1] vs
// 192.0.2.1) compare equal. The function is idempotent:
// CanonicalHost(CanonicalHost(x)) == CanonicalHost(x).
//
// Everything else returns "": stray or unmatched brackets, a bracketed
// non-IPv6 ("[allowed.example]" — brackets are IPv6-only syntax), a
// non-numeric, out-of-range, or empty port, a second port, more than one
// trailing dot, an empty label, a non-ASCII name (configure the Punycode
// A-label, "xn--...", instead — matching is byte-exact and no IDN mapping is
// performed), and an all-numeric dotted name that net.ParseIP rejects
// ("127.0.0.001", "0177.0.0.1" — HTTP clients disagree on leading-zero and
// octal IPv4 forms, so no single textual key can match them safely).
//
// The sibling ssrf library carries its own numeric-IPv4 heuristic
// (looksLikeNumericIPv4) with a deliberately DIFFERENT reject set: ssrf feeds
// a resolver, so it must also reject dotted-hex forms like "0x7f.0.0.1" that
// this exact-match key safely accepts (the "x" makes the label a plain DNS
// label, matchable only if explicitly allowlisted). The two must NOT be
// unified — see that function's doc for the outbound rationale.
//
// Malformed input is REJECTED, never repaired. Deleting the offending syntax
// instead (stripping stray brackets, truncating at a bad port) would let
// distinct wire values collapse onto an allowlisted key — "[allowed.example]",
// "allowed[.]example", and "allowed.example:garbage:443" would all admit as
// "allowed.example" — silently widening an exact-match allowlist.
//
// No name resolution is performed. Resolving a hostname to compare against the
// request would reopen the very DNS-rebinding race a host allowlist exists to
// close (the attacker controls what the name resolves to), so matching is
// purely textual on the canonicalized Host.
func CanonicalHost(hostport string) string {
	if ip := net.ParseIP(hostport); ip != nil {
		return ip.String() // bare IPv4/IPv6 literal
	}
	if strings.HasPrefix(hostport, "[") {
		return canonicalBracketed(hostport)
	}
	host := hostport
	if i := strings.IndexByte(host, ':'); i >= 0 {
		if !validPort(host[i+1:]) {
			return "" // never repaired by truncating at the colon
		}
		host = host[:i]
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".") // at most one FQDN dot
	if ip := net.ParseIP(host); ip != nil {
		return ip.String() // IPv4 that carried a port or a trailing dot
	}
	if !validHostname(host) {
		return ""
	}
	return host
}

// canonicalBracketed parses a "[IPv6]" or "[IPv6]:port" authority. Brackets
// are IPv6-only syntax in RFC 3986, so a bracketed hostname or IPv4 literal is
// rejected, as is anything after "]" that is not exactly one valid ":port".
func canonicalBracketed(s string) string {
	end := strings.IndexByte(s, ']')
	if end < 0 {
		return ""
	}
	if rest := s[end+1:]; rest != "" {
		port, ok := strings.CutPrefix(rest, ":")
		if !ok || !validPort(port) {
			return ""
		}
	}
	inner := s[1:end]
	ip := net.ParseIP(inner)
	if ip == nil || !strings.Contains(inner, ":") {
		return ""
	}
	return ip.String()
}

// validPort reports whether a port string is all digits and at most 65535.
func validPort(port string) bool {
	if port == "" || len(port) > 5 {
		return false
	}
	n := 0
	for i := range len(port) {
		c := port[i]
		if c < '0' || c > '9' {
			return false
		}
		n = n*10 + int(c-'0')
	}
	return n <= 65535
}

// validHostname reports whether a lowercased, port- and dot-stripped candidate
// is an acceptable DNS name: non-empty ASCII labels of letters, digits,
// hyphens, and underscores, each at most 63 bytes. An all-numeric dotted
// candidate is rejected — it reaches here only because net.ParseIP refused it,
// and an IPv4-looking string that Go will not parse (leading zeros, five
// octets) is a configuration error, not a hostname.
func validHostname(host string) bool {
	nonNumeric := false
	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := range len(label) {
			switch c := label[i]; {
			case c >= '0' && c <= '9':
			case c >= 'a' && c <= 'z', c == '-', c == '_':
				nonNumeric = true
			default:
				return false
			}
		}
	}
	return nonNumeric
}

// HostAllowlistOption configures a HostPolicy built by ParseHostList.
type HostAllowlistOption func(*hostPolicyConfig)

// hostPolicyConfig holds resolved HostPolicy configuration.
type hostPolicyConfig struct {
	code           string
	msg            string
	loopbackExempt bool
}

// WithLoopbackExempt admits a request whenever BOTH its socket peer and its
// Host are loopback, regardless of the allowlist — the container-internal
// carve-out. It keeps a service's own loopback clients working under any
// allowlist: a baked Docker healthcheck (curl http://127.0.0.1:PORT), an
// in-container agent hitting localhost, a sidecar probe. Without it, an
// allowlist of browser-facing hostnames rejects those callers and can brick a
// health signal.
//
// The carve-out is unreachable by the attacks a host allowlist defends against.
// A DNS-rebinding request carries the ATTACKER'S hostname in Host, so it fails
// the loopback-Host test even when it originates from a browser on this same
// machine (a loopback peer); and a remote client forging Host: 127.0.0.1 is not
// a loopback socket peer, so it fails the peer test. Both conditions must hold,
// and only genuinely local traffic can satisfy both.
//
// Off by default: a bare allowlist rejects everything not listed, loopback
// included. Opt in when the service runs behind a loopback health check or
// serves in-container clients.
func WithLoopbackExempt() HostAllowlistOption {
	return func(c *hostPolicyConfig) { c.loopbackExempt = true }
}

// WithHostAllowlistError sets the error code and message written in the 403
// JSON envelope (via WriteError) when a request's Host is not allowed. Defaults
// to "host_not_allowed" / "host not allowed". Override it to name the app's own
// configuration knob, e.g. "host not allowed; add it to KWEB_ALLOWED_HOSTS".
func WithHostAllowlistError(code, msg string) HostAllowlistOption {
	return func(c *hostPolicyConfig) {
		c.code, c.msg = code, msg
	}
}

// HostPolicy is an immutable, canonicalized exact-match Host allowlist. Build
// one with ParseHostList and apply it with Middleware; the same policy answers
// the per-request decision through Allows. The zero value and a nil pointer are
// both inactive (permissive), so a policy parsed from unset configuration is a
// safe pass-through.
type HostPolicy struct {
	allowed        map[string]struct{}
	code           string
	msg            string
	loopbackExempt bool
	active         bool
}

// ParseHostList parses operator-supplied allowlist entries (a config array or a
// comma-split environment variable) into a HostPolicy, returning the entries it
// could not use separately — a shape deliberately identical to ParseCIDRs so a
// strict caller can reject bad input while a lenient caller logs it and
// proceeds.
//
// Each entry is trimmed; a blank entry is skipped and does not engage the gate.
// A usable entry is canonicalized via CanonicalHost and added to the allowlist.
// An entry that is not a well-formed host[:port] under CanonicalHost's strict
// grammar is reported as invalid (returned, never added): a pasted URL
// ("http://example.com"), a lone port (":9848") that belongs in the bind
// address, bracket or colon garbage, a non-ASCII name (configure the Punycode
// A-label instead), or an IP-like numeric string that is not a valid address
// ("127.0.0.001").
//
// Activation, and the fail-closed contract: the policy is INACTIVE only when no
// non-blank entry was supplied at all (unset or all-whitespace configuration) —
// then Middleware is a pass-through and Allows returns true, the
// backward-compatible "accept every Host" default. As soon as ANY non-blank
// entry is supplied the gate is ACTIVE, even if every entry was invalid: a
// misconfigured allowlist (all entries malformed) then rejects every request
// rather than silently disabling the protection. Because invalid entries are
// returned rather than retained as unmatchable keys, a caller that wants to
// fail loud can inspect the invalid slice at startup and refuse to boot; a
// caller that wants to proceed logs it and runs with the valid subset (or, when
// the subset is empty, a fail-closed deny-all).
//
// Pair it with WithLoopbackExempt for a containerized service whose own
// loopback health check or in-container clients must keep working, and with
// WithHostAllowlistError to name the app's configuration knob in the 403.
func ParseHostList(entries []string, opts ...HostAllowlistOption) (policy *HostPolicy, invalid []string) {
	c := &hostPolicyConfig{code: "host_not_allowed", msg: "host not allowed"}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	p := &HostPolicy{
		allowed:        make(map[string]struct{}),
		code:           c.code,
		msg:            c.msg,
		loopbackExempt: c.loopbackExempt,
	}
	for _, e := range entries {
		s := strings.TrimSpace(e)
		if s == "" {
			continue // a blank entry never engages the gate
		}
		p.active = true // any non-blank entry engages the gate (fail closed)
		key := CanonicalHost(s)
		if key == "" {
			invalid = append(invalid, s)
			continue
		}
		p.allowed[key] = struct{}{}
	}
	return p, invalid
}

// Active reports whether the gate is engaged. A policy parsed from unset or
// all-blank configuration is inactive: Middleware is a pass-through and Allows
// returns true for every request. A nil policy is inactive.
func (p *HostPolicy) Active() bool {
	return p != nil && p.active
}

// Size reports the number of valid, canonicalized hosts in the allowlist
// (excluding entries ParseHostList reported as invalid). A nil policy has size
// zero.
func (p *HostPolicy) Size() int {
	if p == nil {
		return 0
	}
	return len(p.allowed)
}

// Allows reports whether the request is admitted. An inactive (or nil) policy
// admits every request. An active policy admits a request only when its
// canonicalized Host is in the allowlist, or — when WithLoopbackExempt was set
// — when both the socket peer and the Host are loopback. r.Host is the only
// host value consulted; X-Forwarded-Host is deliberately ignored (it is
// client-controlled and this check must hold on the direct-exposure path). A
// malformed Host canonicalizes to "" and can never match.
func (p *HostPolicy) Allows(r *http.Request) bool {
	if p == nil || !p.active {
		return true
	}
	host := CanonicalHost(r.Host)
	if _, ok := p.allowed[host]; ok {
		return true
	}
	if p.loopbackExempt && isLoopbackHost(host) && isLoopbackPeer(r.RemoteAddr) {
		return true
	}
	return false
}

// Middleware returns the allowlist as webhttp middleware. An inactive (or nil)
// policy returns the next handler unwrapped — the "off" contract shared with
// RateLimiter and RouteTimeout, so a policy from unset configuration adds no
// overhead. An active policy rejects a request whose Host is not allowed with a
// 403 via WriteError (code and message set by WithHostAllowlistError), before
// the next handler runs.
//
// Placement: put it OUTERMOST among request-authorization middleware, before a
// cross-origin or CSRF check, so a disallowed host is rejected before any route
// runs — a DNS-rebinding request makes Origin and Host agree, so a same-origin
// check alone would admit it; the exact-Host allowlist is what breaks that
// chain (CWE-346).
func (p *HostPolicy) Middleware() Middleware {
	if p == nil || !p.active {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if p.Allows(r) {
				next.ServeHTTP(w, r)
				return
			}
			WriteError(w, r, http.StatusForbidden, p.code, p.msg)
		})
	}
}

// isLoopbackHost reports whether a CanonicalHost-normalized value names the
// local host: the literal "localhost" or any loopback IP (127.0.0.0/8, ::1).
func isLoopbackHost(canon string) bool {
	if canon == "localhost" {
		return true
	}
	ip := net.ParseIP(canon)
	return ip != nil && ip.IsLoopback()
}

// isLoopbackPeer reports whether an http.Request.RemoteAddr belongs to a
// loopback socket peer. RemoteAddr is set by the server from the accepted
// connection (net.Conn.RemoteAddr().String(), always "host:port"), so it
// cannot be spoofed at this layer; forwarded headers play no part. Anything
// that does not split cleanly as host:port fails CLOSED — a lenient portless
// fallback would let a non-stdlib caller with a hand-built RemoteAddr widen
// the loopback carve-out.
func isLoopbackPeer(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
