package webhttp_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cplieger/webhttp"
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

	// A fixed, active allowlist of one browser-facing host.
	const allowed = "webterm.example.com"
	f.Fuzz(func(t *testing.T, host, remoteAddr string, exempt bool) {
		var opts []webhttp.HostAllowlistOption
		if exempt {
			opts = append(opts, webhttp.WithLoopbackExempt())
		}
		p, _ := webhttp.ParseHostList([]string{allowed}, opts...)

		req := httptest.NewRequest(http.MethodGet, "http://placeholder/x", http.NoBody)
		req.Host = host
		req.RemoteAddr = remoteAddr

		if !p.Allows(req) {
			return // a rejection can never be a security failure for an active gate
		}
		// Admitted: it must be justified by exactly one of the two rules,
		// re-derived independently below.
		if name, ok := splitAuthority(host); ok && strings.TrimSuffix(strings.ToLower(name), ".") == allowed {
			return
		}
		if exempt && oracleLoopbackHost(host) && loopbackAddr(remoteAddr) {
			return
		}
		t.Errorf("active gate admitted an unjustified request: host=%q remoteAddr=%q exempt=%v", host, remoteAddr, exempt)
	})
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
	name = strings.TrimSuffix(strings.ToLower(name), ".")
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
