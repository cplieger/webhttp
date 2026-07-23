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
		{"ipv4", "192.168.1.5", "192.168.1.5"},
		{"ipv4 with port", "192.168.1.5:443", "192.168.1.5"},
		{"ipv6 bracketed with port", "[::1]:9848", "::1"},
		{"ipv6 expanded spelling", "0:0:0:0:0:0:0:1", "::1"},
		{"lone port is empty", ":9848", ""},
		{"empty is empty", "", ""},
		{"empty brackets", "[]", ""},
		{"trailing-dot fqdn", "localhost.", "localhost"},
		{"stray bracket + colon collapses (idempotence guard)", "[:", ""},
		{"bracketed colon garbage collapses", "[a:b]", "a"},
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
		{"slash entry reported, gate active", []string{"http://example.com"}, true, 0, []string{"http://example.com"}},
		{"lone port reported, gate active", []string{":9848"}, true, 0, []string{":9848"}},
		{"mixed valid and invalid", []string{"good.example", "bad/entry", ":80", "  "}, true, 1, []string{"bad/entry", ":80"}},
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
// that X-Forwarded-Host cannot smuggle an allowed name, the inactive
// pass-through, and the loopback carve-out with each attack shape it must still
// reject.
func TestHostPolicyMiddleware(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	do := func(h http.Handler, host, xfh, remoteAddr string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/x", http.NoBody)
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
