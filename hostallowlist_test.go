package webhttp_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/cplieger/webhttp"
)

func TestCanonicalHost(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"bare host", "example.com", "example.com"},
		{"host with port", "example.com:9848", "example.com"},
		{"uppercase + trailing dot", "Webterm.Example.COM.", "webterm.example.com"},
		{"trailing dot with port", "example.com.:443", "example.com"},
		{"underscore label (compose service name)", "my_service:9848", "my_service"},
		{"punycode a-label", "xn--wbterm-bva.example", "xn--wbterm-bva.example"},
		{"ipv4", "192.168.1.5", "192.168.1.5"},
		{"ipv4 with port", "192.168.1.5:443", "192.168.1.5"},
		{"ipv6 bracketed with port", "[::1]:9848", "::1"},
		{"ipv6 bracketed without port", "[::1]", "::1"},
		{"ipv6 expanded spelling", "0:0:0:0:0:0:0:1", "::1"},
		{"v4-mapped v6 collapses to ipv4", "[::ffff:192.168.1.5]:443", "192.168.1.5"},
		{"trailing-dot fqdn", "localhost.", "localhost"},

		// Malformed authorities are rejected, never repaired. Each of the
		// bracket/colon shapes below used to collapse onto a plausible
		// allowlist key, silently widening an exact-match gate (the
		// adversarial-review finding this pins).
		{"lone port is empty", ":9848", ""},
		{"empty is empty", "", ""},
		{"empty brackets", "[]", ""},
		{"stray bracket + colon", "[:", ""},
		{"bracketed non-ip", "[a:b]", ""},
		{"bracketed hostname", "[allowed.example]", ""},
		{"bracketed ipv4 (brackets are v6-only)", "[127.0.0.1]", ""},
		{"interior bracket garbage", "allowed[.]example", ""},
		{"multi-colon port garbage", "allowed.example:garbage:443", ""},
		{"double port", "example.com:1:2", ""},
		{"empty port", "example.com:", ""},
		{"port out of range", "example.com:99999", ""},
		{"double trailing dot", "example.com..", ""},
		{"empty label", ".example.com", ""},
		{"leading-zero ipv4 is not repaired", "127.0.0.001", ""},
		{"octal-looking ipv4", "0177.0.0.1", ""},
		{"all-numeric dotted non-ip", "1.2.3.4.5", ""},
		{"non-ascii name (use punycode)", "wébterm.example", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := webhttp.CanonicalHost(tc.in); got != tc.want {
				t.Errorf("CanonicalHost(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// Idempotence: a canonical value canonicalizes to itself.
			if got := webhttp.CanonicalHost(tc.want); got != tc.want {
				t.Errorf("CanonicalHost not idempotent: CanonicalHost(%q) = %q, want %q", tc.want, got, tc.want)
			}
		})
	}
}

// TestLoopbackRequest pins the composite predicate's contract: both legs
// required, forwarded headers ignored in BOTH directions, and fail-closed on
// anything unparseable in either leg. The motivating case — a REMOTE peer
// sending Host: localhost — is asserted explicitly, since it is the request a
// peer-only gate (and, per the subtest below, a Host allowlist) admits.
func TestLoopbackRequest(t *testing.T) {
	// The request URL is a fixed placeholder; the predicate reads req.Host,
	// assigned raw so malformed wire values reach it unrepaired (building a URL
	// from them would panic in NewRequest before the predicate ran).
	build := func(host, remoteAddr string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "http://placeholder/x", http.NoBody)
		req.Host = host
		req.RemoteAddr = remoteAddr
		return req
	}

	cases := []struct {
		name, host, remoteAddr string
		want                   bool
	}{
		// Both legs loopback: the in-container caller this gate exists for.
		{"ipv4 peer + ipv4 Host", "127.0.0.1:9848", "127.0.0.1:5000", true},
		{"ipv4 peer + localhost name (the documented curl shape)", "localhost:9848", "127.0.0.1:5000", true},
		{"portless name Host", "localhost", "127.0.0.1:5000", true},
		{"portless ipv4 Host", "127.0.0.1", "127.0.0.1:5000", true},
		{"case-folded name Host", "LocalHost:9848", "127.0.0.1:5000", true},
		{"trailing-dot fqdn Host", "localhost.:9848", "127.0.0.1:5000", true},
		{"127/8 is loopback, not just 127.0.0.1", "127.9.9.9:9848", "127.0.0.5:5000", true},
		{"bracketed ipv6 Host + ipv6 peer", "[::1]:9848", "[::1]:5000", true},
		{"bare ipv6 Host (no port possible)", "::1", "[::1]:5000", true},
		{"expanded ipv6 spelling", "0:0:0:0:0:0:0:1", "[::1]:5000", true},
		{"v4-mapped bracketed ipv6 Host", "[::ffff:127.0.0.1]:9848", "127.0.0.1:5000", true},

		// The Host leg alone refusing. A DNS-rebound page reaches a same-host
		// server with a LOOPBACK socket peer and the attacker's own name in
		// Host, so dropping this leg reopens CWE-346.
		{"loopback peer + attacker Host", "attacker.evil:9848", "127.0.0.1:5000", false},
		{"loopback peer + LAN name Host", "kiro.lan", "127.0.0.1:5000", false},
		{"loopback peer + decorated name Host", "localhost.evil.example", "127.0.0.1:5000", false},
		{"loopback peer + decorated literal Host", "127.0.0.1.evil.example", "127.0.0.1:5000", false},

		// The peer leg alone refusing — THE motivating case. A remote client
		// can put anything in Host, so a Host-only decision admits it.
		{"remote peer + Host localhost", "localhost:9848", "203.0.113.9:5000", false},
		{"remote peer + Host 127.0.0.1", "127.0.0.1:9848", "203.0.113.9:5000", false},
		{"remote peer + Host [::1]", "[::1]:9848", "[2001:db8::1]:5000", false},
		{"remote peer + remote Host", "webterm.example.com:9848", "203.0.113.9:5000", false},

		// Unparseable RemoteAddr fails closed: a portless or malformed value
		// would otherwise let a non-stdlib caller widen the gate.
		{"portless peer", "localhost:9848", "127.0.0.1", false},
		{"empty peer", "localhost:9848", "", false},
		{"non-address peer", "localhost:9848", "not-an-addr", false},
		{"non-ip peer host", "localhost:9848", "not-an-ip:1234", false},
		{"unbracketed ipv6 peer (too many colons)", "localhost:9848", "::1:5000", false},

		// Unparseable or empty Host fails closed: CanonicalHost returns "",
		// which names nothing, and is never repaired into a match.
		{"empty Host", "", "127.0.0.1:5000", false},
		{"Host with garbage port", "127.0.0.1:garbage", "127.0.0.1:5000", false},
		{"bracketed name Host (brackets are ipv6-only)", "[localhost]", "127.0.0.1:5000", false},
		{"multi-colon port Host", "localhost:garbage:443", "127.0.0.1:5000", false},
		{"double trailing dot Host", "localhost..", "127.0.0.1:5000", false},
		{"pasted url Host", "http://localhost:9848/x", "127.0.0.1:5000", false},
		{"leading-zero literal Host is not repaired", "127.0.0.001:9848", "127.0.0.1:5000", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := webhttp.LoopbackRequest(build(tc.host, tc.remoteAddr)); got != tc.want {
				t.Errorf("LoopbackRequest(Host %q, peer %q) = %v, want %v", tc.host, tc.remoteAddr, got, tc.want)
			}
		})
	}

	// Forwarded headers are ignored in BOTH directions. Their presence must not
	// ADMIT (the header is client-controlled, so honoring it would hand a remote
	// caller the gate) and must not REFUSE either (an app that wants a
	// provenance deny composes it around this predicate; folding it in here
	// would make that policy the library's).
	t.Run("forwarded headers change nothing", func(t *testing.T) {
		forwarded := map[string]string{
			"X-Forwarded-For":  "127.0.0.1",
			"X-Forwarded-Host": "localhost",
			"Forwarded":        "for=127.0.0.1;host=localhost",
		}
		cases := []struct {
			name, host, remoteAddr string
			want                   bool
		}{
			{"cannot refuse an admitted request", "localhost:9848", "127.0.0.1:5000", true},
			{"cannot admit a remote peer", "localhost:9848", "203.0.113.9:5000", false},
			{"cannot admit a rebound Host", "attacker.evil:9848", "127.0.0.1:5000", false},
			{"cannot admit a malformed peer", "localhost:9848", "127.0.0.1", false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				for name, value := range forwarded {
					req := build(tc.host, tc.remoteAddr)
					req.Header.Set(name, value)
					if got := webhttp.LoopbackRequest(req); got != tc.want {
						t.Errorf("%s: %s = %q changed the verdict to %v, want %v", tc.name, name, value, got, tc.want)
					}
				}
				// All three at once, in case a future implementation reads them
				// only in combination.
				req := build(tc.host, tc.remoteAddr)
				for name, value := range forwarded {
					req.Header.Set(name, value)
				}
				if got := webhttp.LoopbackRequest(req); got != tc.want {
					t.Errorf("%s: all forwarded headers set changed the verdict to %v, want %v", tc.name, got, tc.want)
				}
			})
		}
	})

	// Why this predicate exists rather than a HostPolicy call, pinned rather
	// than only asserted in the godoc: HostPolicy checks its Host-only
	// allowlist BEFORE the two-legged loopback exemption, so an operator who
	// allows "localhost" (so their own browser reaches the service) thereby
	// admits a REMOTE caller that sends Host: localhost — the exact request
	// LoopbackRequest must refuse. The two answer different questions, and this
	// case fails if that ordering ever changes underneath the doc comment.
	t.Run("HostPolicy admits the caller this predicate refuses", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://placeholder/x", http.NoBody)
		req.Host = "localhost:9848"
		req.RemoteAddr = "203.0.113.9:5000" // a REMOTE peer

		for _, exempt := range []bool{false, true} {
			var opts []webhttp.HostAllowlistOption
			if exempt {
				opts = append(opts, webhttp.WithLoopbackExempt())
			}
			p, invalid := webhttp.ParseHostList([]string{"localhost", "webterm.example.com"}, opts...)
			if len(invalid) != 0 {
				t.Fatalf("unexpected invalid entries: %v", invalid)
			}
			if !p.Allows(req) {
				t.Errorf("WithLoopbackExempt=%v: HostPolicy.Allows refused a remote caller sending an allowlisted Host; the doc comment's rationale for LoopbackRequest is stale", exempt)
			}
		}
		if webhttp.LoopbackRequest(req) {
			t.Error("LoopbackRequest admitted a remote peer sending Host: localhost")
		}
	})
}

func TestParseHostList(t *testing.T) {
	cases := []struct {
		name        string
		entries     []string
		wantActive  bool
		wantSize    int
		wantInvalid []string
	}{
		{"nil is inactive", nil, false, 0, nil},
		{"all blank is inactive", []string{"  ", "", " \t "}, false, 0, nil},
		{"valid entries", []string{"localhost", "192.168.1.5", "Webterm.Example.COM."}, true, 3, nil},
		{"duplicate canonicalizes to one", []string{"example.com", "EXAMPLE.com:80", "example.com."}, true, 1, nil},
		{"pasted url reported, gate active", []string{"http://example.com"}, true, 0, []string{"http://example.com"}},
		{"lone port reported, gate active", []string{":9848"}, true, 0, []string{":9848"}},
		{"mixed valid and invalid", []string{"good.example", "bad/entry", ":80", "  "}, true, 1, []string{"bad/entry", ":80"}},
		{
			"collision shapes reported, never repaired",
			[]string{"[allowed.example]", "allowed[.]example", "allowed.example:garbage:443", "example.com.."},
			true, 0,
			[]string{"[allowed.example]", "allowed[.]example", "allowed.example:garbage:443", "example.com.."},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, invalid := webhttp.ParseHostList(tc.entries)
			if p.Active() != tc.wantActive {
				t.Errorf("Active() = %v, want %v", p.Active(), tc.wantActive)
			}
			if p.Size() != tc.wantSize {
				t.Errorf("Size() = %d, want %d", p.Size(), tc.wantSize)
			}
			if !slices.Equal(invalid, tc.wantInvalid) {
				t.Errorf("invalid = %v, want %v", invalid, tc.wantInvalid)
			}
		})
	}
}

// TestHostPolicyMiddleware pins the gate through a real handler: the anti-DNS-
// rebinding contract (a rebound Host with a matching Origin is still rejected
// because the allowlist is checked on Host, not Origin), canonicalization,
// that a malformed Host spelling of an allowed name is rejected rather than
// repaired into a match, that X-Forwarded-Host cannot smuggle an allowed name,
// the inactive pass-through, and the loopback carve-out with each attack shape
// it must still reject.
func TestHostPolicyMiddleware(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	do := func(h http.Handler, host, xfh, remoteAddr string) (int, string) {
		// The request URL is a fixed placeholder; the gate reads req.Host,
		// assigned raw so malformed wire values reach it unrepaired (building
		// a URL from them would panic in NewRequest before the gate ran).
		req := httptest.NewRequest(http.MethodGet, "http://placeholder/x", http.NoBody)
		req.Host = host
		if xfh != "" {
			req.Header.Set("X-Forwarded-Host", xfh)
		}
		if remoteAddr != "" {
			req.RemoteAddr = remoteAddr
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	t.Run("active gate: exact-match, canonicalization, XFH cannot smuggle", func(t *testing.T) {
		p, invalid := webhttp.ParseHostList([]string{"localhost", "192.168.1.5", "::1", "Webterm.Example.COM."})
		if len(invalid) != 0 {
			t.Fatalf("unexpected invalid entries: %v", invalid)
		}
		h := p.Middleware()(ok)
		cases := []struct {
			name, host, xfh, remoteAddr string
			want                        int
		}{
			{"rebound host rejected even with matching Origin semantics", "attacker.evil:9848", "", "", http.StatusForbidden},
			{"X-Forwarded-Host cannot smuggle an allowed name", "attacker.evil:9848", "localhost", "", http.StatusForbidden},
			{"allowed host passes", "localhost:9848", "", "", http.StatusOK},
			{"allowed IP passes", "192.168.1.5:9848", "", "", http.StatusOK},
			{"case + trailing dot + port canonicalize", "WEBTERM.example.com:1234", "", "", http.StatusOK},
			{"ipv6 spelling canonicalizes", "[0:0:0:0:0:0:0:1]:9848", "", "", http.StatusOK},
			{"loopback rejected without the exempt option", "127.0.0.1:9848", "", "127.0.0.1:5000", http.StatusForbidden},
			// Malformed spellings of an ALLOWED name must be rejected, not
			// repaired into a match (the adversarial-review collision shapes).
			{"bracket-wrapped allowed name rejected", "[webterm.example.com]", "", "", http.StatusForbidden},
			{"interior-bracket spelling rejected", "webterm.example[.]com", "", "", http.StatusForbidden},
			{"multi-colon port spelling rejected", "webterm.example.com:garbage:443", "", "", http.StatusForbidden},
			{"double-trailing-dot spelling rejected", "webterm.example.com..", "", "", http.StatusForbidden},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got, _ := do(h, tc.host, tc.xfh, tc.remoteAddr); got != tc.want {
					t.Errorf("Host %q = %d, want %d", tc.host, got, tc.want)
				}
			})
		}
	})

	t.Run("inactive policy is a pass-through", func(t *testing.T) {
		p, _ := webhttp.ParseHostList(nil)
		h := p.Middleware()(ok)
		if got, _ := do(h, "anything.example:9848", "", ""); got != http.StatusOK {
			t.Errorf("inactive gate rejected a request: got %d, want %d", got, http.StatusOK)
		}
	})

	t.Run("all-invalid nonblank list is active and denies all (fail closed)", func(t *testing.T) {
		p, invalid := webhttp.ParseHostList([]string{"http://x", ":80"})
		if !p.Active() || p.Size() != 0 {
			t.Fatalf("want active empty policy, got active=%v size=%d", p.Active(), p.Size())
		}
		if len(invalid) != 2 {
			t.Errorf("want 2 invalid entries, got %v", invalid)
		}
		h := p.Middleware()(ok)
		if got, _ := do(h, "anything.example:9848", "", ""); got != http.StatusForbidden {
			t.Errorf("misconfigured (all-invalid) gate did not fail closed: got %d, want %d", got, http.StatusForbidden)
		}
	})

	t.Run("loopback carve-out", func(t *testing.T) {
		// A browser-facing allowlist with NO loopback entry.
		p, _ := webhttp.ParseHostList([]string{"webterm.example.com"}, webhttp.WithLoopbackExempt())
		h := p.Middleware()(ok)
		cases := []struct {
			name, host, remoteAddr string
			want                   int
		}{
			{"healthcheck shape: loopback peer + 127.0.0.1 Host admitted", "127.0.0.1:9848", "127.0.0.1:5000", http.StatusOK},
			{"tools shape: loopback peer + localhost Host admitted", "localhost:9848", "127.0.0.1:5000", http.StatusOK},
			{"ipv6 loopback peer + ::1 Host admitted", "[::1]:9848", "[::1]:5000", http.StatusOK},
			{"rebinding via same-host browser: loopback peer + attacker Host rejected", "attacker.evil:9848", "127.0.0.1:5000", http.StatusForbidden},
			{"forged loopback Host from a remote peer rejected", "127.0.0.1:9848", "203.0.113.9:5000", http.StatusForbidden},
			{"malformed peer fails closed", "127.0.0.1:9848", "not-an-addr", http.StatusForbidden},
			{"portless loopback peer fails closed", "127.0.0.1:9848", "127.0.0.1", http.StatusForbidden},
			{"allowlisted host from a remote peer still passes", "webterm.example.com:9848", "203.0.113.9:5000", http.StatusOK},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got, _ := do(h, tc.host, "", tc.remoteAddr); got != tc.want {
					t.Errorf("Host %q peer %q = %d, want %d", tc.host, tc.remoteAddr, got, tc.want)
				}
			})
		}
	})

	t.Run("403 envelope is overridable", func(t *testing.T) {
		p, _ := webhttp.ParseHostList([]string{"good.example"},
			webhttp.WithHostAllowlistError("host_denied", "nope"))
		h := p.Middleware()(ok)
		code, body := do(h, "bad.example:9848", "", "")
		if code != http.StatusForbidden {
			t.Fatalf("got %d, want 403", code)
		}
		var env webhttp.ErrorResponse
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatalf("decode envelope: %v (body %q)", err, body)
		}
		if env.Code != "host_denied" || env.Error != "nope" {
			t.Errorf("envelope = {code:%q error:%q}, want {host_denied nope}", env.Code, env.Error)
		}
	})
}
