package webhttp_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/cplieger/webhttp"
)

// TestCanonicalRequestPath pins the cleaned value and the verdict per spelling:
// the canonical forms a mux routes as-is, the rewrite classes it answers 307 for
// (repeated slashes, dot segments, a path that cleans INTO a namespace), the
// load-bearing trailing slash, the encoded dot segment that is canonical on the
// escaped value, and the two documented edge inputs.
func TestCanonicalRequestPath(t *testing.T) {
	for _, tc := range []struct {
		name          string
		in            string
		wantClean     string
		wantCanonical bool
	}{
		{"root", "/", "/", true},
		{"plain path", "/beat", "/beat", true},
		{"trailing slash kept", "/beat/", "/beat/", true},
		{"nested path", "/beat/api", "/beat/api", true},
		{"nested trailing slash kept", "/beat/api/", "/beat/api/", true},
		{"doubled trailing slash", "/beat//", "/beat/", false},
		{"interior doubled slash", "/beat//api", "/beat/api", false},
		{"leading doubled slash cleans into the namespace", "//beat/api", "/beat/api", false},
		{"single dot segment", "/beat/./api", "/beat/api", false},
		{"double dot segment moves to a sibling", "/beat/api/../ghost", "/beat/ghost", false},
		{"double dot segment leaves the namespace", "/beat/..", "/", false},
		{"double dot segment with trailing slash", "/beat/../", "/", false},
		{"dot segments above root are dropped", "/../beat", "/beat", false},
		{"dot-only path", "/.", "/", false},
		{"dot-only path with trailing slash", "/./", "/", false},
		{"all slashes", "///", "/", false},
		{"encoded dot segment is canonical", "/beat/%2e%2e/ghost", "/beat/%2e%2e/ghost", true},
		{"empty is rooted and never canonical", "", "/", false},
		{"missing leading slash is rooted", "beat/api", "/beat/api", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clean, canonical := webhttp.CanonicalRequestPath(tc.in)
			if clean != tc.wantClean {
				t.Errorf("CanonicalRequestPath(%q) clean = %q, want %q", tc.in, clean, tc.wantClean)
			}
			if canonical != tc.wantCanonical {
				t.Errorf("CanonicalRequestPath(%q) canonical = %v, want %v", tc.in, canonical, tc.wantCanonical)
			}
		})
	}
}

// TestCanonicalRequestPathAgreesWithServeMuxRedirect is the oracle for the whole
// surface: it drives a REAL http.ServeMux and asserts the function's verdict
// matches what the mux actually does with the same path. A non-canonical
// spelling must draw the 307 (with the cleaned path as its Location) that no
// registered pattern can intercept, and a canonical one must reach the handler.
//
// The mux carries a single "/" catch-all, which is what makes this an oracle for
// the CLEANING step alone: with only that pattern registered the trailing-slash
// redirect can never fire (it needs an exact match on path+"/"), so a 307 here
// can only be the canonicalization. Inputs are unreserved ASCII, so
// r.URL.EscapedPath() — the value the mux actually cleans — is the input
// verbatim.
func TestCanonicalRequestPathAgreesWithServeMuxRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	for _, in := range []string{
		"/", "/beat", "/beat/", "/beat/api", "/beat/api/",
		"/beat//", "/beat//api", "//beat/api", "/beat/./api",
		"/beat/api/../ghost", "/beat/..", "/beat/../", "/../beat",
		"/.", "/./", "///",
	} {
		t.Run(in, func(t *testing.T) {
			clean, canonical := webhttp.CanonicalRequestPath(in)

			// Built by hand rather than with httptest.NewRequest: that helper
			// parses the target as a URL, and a "//host/path" target parses as
			// scheme-relative (authority "beat"), which would test a different
			// path than the one under test.
			r := &http.Request{
				Method:     http.MethodGet,
				URL:        &url.URL{Path: in},
				Host:       "example.com",
				Proto:      "HTTP/1.1",
				ProtoMajor: 1,
				ProtoMinor: 1,
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, r)

			if canonical {
				if rec.Code != http.StatusOK {
					t.Fatalf("%q reported canonical but the mux answered %d (Location %q): the verdict claims the mux routes it",
						in, rec.Code, rec.Header().Get("Location"))
				}
				return
			}
			if rec.Code != http.StatusTemporaryRedirect {
				t.Fatalf("%q reported non-canonical but the mux answered %d, want 307", in, rec.Code)
			}
			if got := rec.Header().Get("Location"); got != clean {
				t.Errorf("%q: mux redirects to %q but clean = %q; the returned value is not the path the mux will route",
					in, got, clean)
			}
		})
	}
}
