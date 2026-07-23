package webhttp_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cplieger/webhttp"
)

// FuzzCanonicalHost asserts CanonicalHost never panics and is idempotent — a
// value it produces canonicalizes to itself. Idempotence is the property the
// allowlist relies on: an entry canonicalized at parse time must compare equal
// to a request Host canonicalized the same way.
func FuzzCanonicalHost(f *testing.F) {
	for _, s := range []string{
		"", "example.com", "example.com:9848", "Webterm.Example.COM.",
		"::1", "[::1]:9848", "0:0:0:0:0:0:0:1", "127.0.0.001", ":9848", "[]",
		"a:b:c", "http://x/y", "\x00", "％", "xn--",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got := webhttp.CanonicalHost(in)
		if again := webhttp.CanonicalHost(got); again != got {
			t.Errorf("CanonicalHost not idempotent: CanonicalHost(%q)=%q, CanonicalHost(%q)=%q", in, got, got, again)
		}
	})
}

// FuzzHostPolicyAllows pins the security invariant of the gate against
// arbitrary Host and RemoteAddr input: an ACTIVE policy admits a request ONLY
// when the canonical Host is in the allowlist, or the loopback carve-out is
// enabled AND both the Host and the socket peer are loopback. The justification
// is re-derived with the standard library directly, an oracle independent of
// the package's own unexported loopback helpers.
func FuzzHostPolicyAllows(f *testing.F) {
	f.Add("example.com", "203.0.113.9:5000", true)
	f.Add("127.0.0.1", "127.0.0.1:5000", true)
	f.Add("attacker.evil", "127.0.0.1:5000", false)
	f.Add("localhost:80", "[::1]:9", true)
	f.Add("", "", false)

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
		// re-derived independently with net.* below.
		canon := webhttp.CanonicalHost(host)
		if canon == allowed {
			return
		}
		if exempt && loopbackName(canon) && loopbackAddr(remoteAddr) {
			return
		}
		t.Errorf("active gate admitted an unjustified request: host=%q (canon %q) remoteAddr=%q exempt=%v", host, canon, remoteAddr, exempt)
	})
}

// loopbackName reports whether a canonical host names the local host, using the
// standard library directly (independent of the package's isLoopbackHost).
func loopbackName(canon string) bool {
	if canon == "localhost" {
		return true
	}
	ip := net.ParseIP(canon)
	return ip != nil && ip.IsLoopback()
}

// loopbackAddr reports whether a RemoteAddr is a loopback peer, using the
// standard library directly (independent of the package's isLoopbackPeer).
func loopbackAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
