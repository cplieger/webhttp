package webhttp_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cplieger/webhttp/v2"
)

// FuzzCanonicalHost asserts CanonicalHost never panics and holds four
// invariants:
//
//  1. Idempotence — a value it produces canonicalizes to itself. This is the
//     property the allowlist relies on: an entry canonicalized at parse time
//     must compare equal to a request Host canonicalized the same way.
//  2. Lowercase — canonical output never carries uppercase, so matching is
//     case-insensitive end to end.
//  3. IP oracle — an input net.ParseIP accepts wholesale must canonicalize to
//     exactly that address's canonical spelling.
//  4. Spelling collapse (metamorphic) — every equivalent wire spelling of a
//     canonical value (a :port, uppercase, one trailing FQDN dot for names and
//     IPv4, brackets+port for IPv6) canonicalizes back to the same value.
func FuzzCanonicalHost(f *testing.F) {
	for _, s := range []string{
		"", "example.com", "example.com:9848", "Webterm.Example.COM.",
		"::1", "[::1]:9848", "0:0:0:0:0:0:0:1", "127.0.0.001", ":9848", "[]",
		"a:b:c", "http://x/y", "\x00", "％", "xn--",
		// Repair-collision shapes from the adversarial review: each must
		// reject, never collapse onto a plausible allowlist key.
		"[allowed.example]", "allowed[.]example", "allowed.example:garbage:443",
		"example.com..", "example.com:1:2", "example.com:", "example.com:99999",
		"0177.0.0.1", "1.2.3.4.5", "my_service", "[::ffff:127.0.0.1]:80",
		"[127.0.0.1]", "wébterm.example",
		// The two case-fold laundering runes: strings.ToLower maps U+0130 to
		// "i" and U+212A to "k", so under a Unicode fold each of these
		// canonicalized to an ASCII key an allowlist could hold. They must
		// reject. Measured identical on Unicode 15 and 17, so this is a
		// permanent property of the mapping, not a release-specific one.
		"\u212Aibana.example", "k\u0130bana.example", "\u212A\u0130.example",
		"\u212Aibana.example:443", "\u212Aibana.example.",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got := webhttp.CanonicalHost(in)
		if again := webhttp.CanonicalHost(got); again != got {
			t.Errorf("CanonicalHost not idempotent: CanonicalHost(%q)=%q, CanonicalHost(%q)=%q", in, got, got, again)
		}
		if lower := strings.ToLower(got); lower != got {
			t.Errorf("CanonicalHost(%q)=%q is not lowercase", in, got)
		}
		// Canonical output is pure ASCII, which is what makes the allowlist's
		// byte-exact matching claim true. A non-ASCII byte reaching the output
		// would mean some rune survived the ASCII fold and validHostname, and
		// the reverse direction is the live hazard this pins: strings.ToLower
		// maps U+0130 to "i" and U+212A to "k", so a Unicode fold here would
		// let a non-ASCII authority produce an ASCII canonical key.
		for i := range len(got) {
			if got[i] >= utf8.RuneSelf {
				t.Errorf("CanonicalHost(%q)=%q carries a non-ASCII byte %#x at %d", in, got, got[i], i)
				break
			}
		}
		if ip := net.ParseIP(in); ip != nil && got != ip.String() {
			t.Errorf("CanonicalHost(%q)=%q, want IP canonical form %q", in, got, ip.String())
		}
		if got == "" {
			return
		}
		// Metamorphic: equivalent spellings of a canonical value collapse
		// back to it. IPv6 output (the only canonical form containing a
		// colon) takes a port via brackets; names and IPv4 take :port and
		// one trailing FQDN dot.
		if strings.Contains(got, ":") {
			for _, variant := range []string{"[" + got + "]", "[" + got + "]:8080", strings.ToUpper(got)} {
				if v := webhttp.CanonicalHost(variant); v != got {
					t.Errorf("CanonicalHost(%q)=%q, want %q (spelling variant of canonical %q)", variant, v, got, got)
				}
			}
			return
		}
		for _, variant := range []string{got + ":8080", got + ".", strings.ToUpper(got)} {
			if v := webhttp.CanonicalHost(variant); v != got {
				t.Errorf("CanonicalHost(%q)=%q, want %q (spelling variant of canonical %q)", variant, v, got, got)
			}
		}
	})
}

// FuzzHostPolicyAllows pins the security invariant of the gate against
// arbitrary Host and RemoteAddr input: an ACTIVE policy admits a request ONLY
// when the Host is a well-formed spelling of an allowlisted name, or the
// loopback carve-out is enabled AND both the Host and the socket peer are
// loopback. The justification is re-derived with an independent authority
// parser written from the RFC 3986 grammar below — deliberately NOT
// CanonicalHost, so a parser bug cannot vouch for itself. The oracle may be
// marginally LOOSER than production on shapes production rejects (a rejection
// is never a security failure); it must never be looser on admission.
func FuzzHostPolicyAllows(f *testing.F) {
	f.Add("example.com", "203.0.113.9:5000", true)
	f.Add("127.0.0.1", "127.0.0.1:5000", true)
	f.Add("attacker.evil", "127.0.0.1:5000", false)
	f.Add("localhost:80", "[::1]:9", true)
	f.Add("", "", false)
	// Repair-collision Hosts (must never admit) and the portless-peer shape
	// (must never satisfy the carve-out).
	f.Add("[webterm.example.com]", "203.0.113.9:5000", false)
	f.Add("webterm.example[.]com", "203.0.113.9:5000", false)
	f.Add("webterm.example.com:garbage:443", "203.0.113.9:5000", false)
	f.Add("webterm.example.com..", "203.0.113.9:5000", false)
	f.Add("127.0.0.1", "127.0.0.1", true)
	f.Add("[::ffff:127.0.0.1]:80", "[::1]:5", true)
	// Case-fold laundering against the second allowlist entry. It exists
	// BECAUSE the first one cannot express this class: "webterm.example.com"
	// carries neither 'i' nor 'k', and those are the only two ASCII letters
	// strings.ToLower can produce from a non-ASCII rune (U+0130 and U+212A),
	// so no laundering input could ever have reached the old single-entry
	// oracle. That is why the fuzzer never found the widening it was written
	// to catch.
	f.Add("\u212Aibana.example", "203.0.113.9:5000", false)
	f.Add("k\u0130bana.example", "203.0.113.9:5000", false)
	f.Add("\u212A\u0130bana.example:443", "203.0.113.9:5000", true)

	// A fixed, active allowlist: one browser-facing host, plus one whose name
	// carries the two letters a Unicode case fold can synthesize.
	allowed := []string{"webterm.example.com", "kibana.example"}
	f.Fuzz(func(t *testing.T, host, remoteAddr string, exempt bool) {
		var opts []webhttp.HostAllowlistOption
		if exempt {
			opts = append(opts, webhttp.WithLoopbackExempt(true))
		}
		p, _ := webhttp.ParseHostList(allowed, opts...)

		req := httptest.NewRequest(http.MethodGet, "http://placeholder/x", http.NoBody)
		req.Host = host
		req.RemoteAddr = remoteAddr

		if !p.Allows(req) {
			return // a rejection can never be a security failure for an active gate
		}
		// Admitted: it must be justified by exactly one of the two rules,
		// re-derived independently below.
		if name, ok := splitAuthority(host); ok && slices.Contains(allowed, strings.TrimSuffix(oracleLowerASCII(name), ".")) {
			return
		}
		if exempt && oracleLoopbackHost(host) && loopbackAddr(remoteAddr) {
			return
		}
		t.Errorf("active gate admitted an unjustified request: host=%q remoteAddr=%q exempt=%v", host, remoteAddr, exempt)
	})
}

// FuzzLoopbackRequest pins the exported composite against arbitrary Host and
// RemoteAddr input, with three invariants:
//
//  1. Justified admission — an admitted request satisfies BOTH legs under the
//     independent oracles below (loopbackAddr from the standard library,
//     oracleLoopbackHost from the RFC 3986 grammar), never the package's own
//     parser. One-directional for the same reason as FuzzHostPolicyAllows: the
//     Host oracle is marginally LOOSER on shapes production rejects (it accepts
//     a trailing dot on an IPv6 literal, which CanonicalHost refuses), and a
//     rejection can never be a security failure.
//  2. Forwarded headers are inert — X-Forwarded-For, X-Forwarded-Host and
//     Forwarded, set to the fuzzed values themselves, never change the verdict
//     in EITHER direction. Admitting on one would hand a remote caller the gate;
//     refusing on one would make an app's provenance policy the library's.
//  3. It IS HostPolicy's loopback carve-out — for an active exempt policy whose
//     allowlist holds one non-loopback name, Allows is exactly "the Host matches
//     that name, or LoopbackRequest". The carve-out shipped first, so it is the
//     reference implementation; a leg dropped from either side breaks equality.
func FuzzLoopbackRequest(f *testing.F) {
	f.Add("localhost:9848", "127.0.0.1:5000")     // the in-container curl shape
	f.Add("127.0.0.1", "127.0.0.1:5000")          // portless literal Host
	f.Add("[::1]:9848", "[::1]:5000")             // ipv6 both ends
	f.Add("localhost:9848", "203.0.113.9:5000")   // remote peer forging Host
	f.Add("attacker.evil:9848", "127.0.0.1:5000") // rebinding: loopback peer, attacker Host
	f.Add("localhost:9848", "127.0.0.1")          // portless peer must fail closed
	f.Add("", "")
	f.Add("[::ffff:127.0.0.1]:80", "[::1]:5")
	f.Add("0:0:0:0:0:0:0:1", "127.0.0.1:5000")
	f.Add("localhost.", "127.0.0.1:5000")
	f.Add("localhost..", "127.0.0.1:5000")
	f.Add("127.0.0.001:9848", "127.0.0.1:5000")
	f.Add("webterm.example.com", "203.0.113.9:5000")

	// A fixed, active allowlist of one browser-facing (non-loopback) host, so
	// invariant 3's two branches never overlap.
	const allowed = "webterm.example.com"
	f.Fuzz(func(t *testing.T, host, remoteAddr string) {
		build := func() *http.Request {
			req := httptest.NewRequest(http.MethodGet, "http://placeholder/x", http.NoBody)
			req.Host = host
			req.RemoteAddr = remoteAddr
			return req
		}

		got := webhttp.LoopbackRequest(build())

		if got && (!loopbackAddr(remoteAddr) || !oracleLoopbackHost(host)) {
			t.Errorf("admitted an unjustified request: host=%q remoteAddr=%q (oracle: peer=%v host=%v)",
				host, remoteAddr, loopbackAddr(remoteAddr), oracleLoopbackHost(host))
		}

		forwarded := build()
		forwarded.Header.Set("X-Forwarded-For", remoteAddr)
		forwarded.Header.Set("X-Forwarded-Host", host)
		forwarded.Header.Set("Forwarded", "for="+remoteAddr+";host="+host)
		if again := webhttp.LoopbackRequest(forwarded); again != got {
			t.Errorf("forwarded headers changed the verdict %v -> %v: host=%q remoteAddr=%q", got, again, host, remoteAddr)
		}

		p, _ := webhttp.ParseHostList([]string{allowed}, webhttp.WithLoopbackExempt(true))
		wantAllows := webhttp.CanonicalHost(host) == allowed || got
		if allows := p.Allows(build()); allows != wantAllows {
			t.Errorf("exempt HostPolicy.Allows = %v, want %v (allowlist match=%v, LoopbackRequest=%v): host=%q remoteAddr=%q",
				allows, wantAllows, webhttp.CanonicalHost(host) == allowed, got, host, remoteAddr)
		}
	})
}

// oracleLowerASCII lowercases the ASCII uppercase letters of s and leaves every
// other byte alone. It is deliberately NOT strings.ToLower: that applies
// Unicode simple case mapping, which lowercases U+0130 to "i" and U+212A to
// "k", so an oracle written with it would launder exactly the way the
// implementation must not — and an oracle sharing the defect it is meant to
// catch proves nothing. This is the one dimension on which the oracles below
// must diverge from the stdlib helper rather than merely from the package.
func oracleLowerASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// splitAuthority splits an unbracketed authority into its name, stripping at
// most one syntactically valid ":port" suffix; a second colon or a bad port
// fails. Written from the RFC 3986 authority grammar, independent of
// CanonicalHost.
func splitAuthority(hostport string) (string, bool) {
	name, port, found := strings.Cut(hostport, ":")
	if !found {
		return hostport, true
	}
	if !digitPort(port) { // a second colon is a non-digit, so it fails here too
		return "", false
	}
	return name, true
}

// digitPort reports whether a candidate port is all digits and at most 65535,
// independent of the package's validPort.
func digitPort(port string) bool {
	if port == "" || len(port) > 5 {
		return false
	}
	n := 0
	for i := range len(port) {
		if port[i] < '0' || port[i] > '9' {
			return false
		}
		n = n*10 + int(port[i]-'0')
	}
	return n <= 65535
}

// oracleLoopbackHost reports whether a wire Host names the local host under
// some well-formed spelling: a bare loopback IP literal (optional trailing
// dot), a bracketed loopback IPv6 with optional valid port, or the localhost
// name (case-insensitive, optional trailing dot, optional valid port).
// Independent of the package's parser and helpers.
func oracleLoopbackHost(host string) bool {
	if ip := net.ParseIP(strings.TrimSuffix(host, ".")); ip != nil {
		return ip.IsLoopback()
	}
	if strings.HasPrefix(host, "[") {
		return bracketedLoopback(host)
	}
	name, ok := splitAuthority(host)
	if !ok {
		return false
	}
	name = strings.TrimSuffix(oracleLowerASCII(name), ".")
	if name == "localhost" {
		return true
	}
	ip := net.ParseIP(name)
	return ip != nil && ip.IsLoopback()
}

// bracketedLoopback reports whether a "[IPv6]" or "[IPv6]:port" authority is a
// loopback address; brackets are IPv6-only syntax, so any other content fails.
func bracketedLoopback(host string) bool {
	end := strings.IndexByte(host, ']')
	if end < 0 {
		return false
	}
	if rest := host[end+1:]; rest != "" {
		port, found := strings.CutPrefix(rest, ":")
		if !found || !digitPort(port) {
			return false
		}
	}
	inner := host[1:end]
	ip := net.ParseIP(inner)
	return ip != nil && strings.Contains(inner, ":") && ip.IsLoopback()
}

// loopbackAddr reports whether a RemoteAddr is a loopback peer, using the
// standard library directly (independent of the package's isLoopbackPeer).
// Strict host:port only — the stdlib server always supplies both, and a
// portless or malformed value must fail closed.
func loopbackAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
