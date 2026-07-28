package webhttp_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cplieger/webhttp"
)

// FuzzValidRequestID pins the request-id charset trust boundary: for ANY input,
// if ValidRequestID accepts it then its length is in 1..64 and every byte is in
// [A-Za-z0-9_-]. This is a strong security invariant, not a crash-only target —
// it guarantees no CR/LF, control byte, space, or other injection content can
// ever pass the validator and reach an echoed response header or a log line.
// The seed corpus is deliberately hardened with valid-charset-plus-one-control
// bytes (a\nb, a\rb, a\tb, "a b") so a narrow validator loosening is caught on
// every deterministic PR run, not only under coverage-guided fuzzing.
func FuzzValidRequestID(f *testing.F) {
	for _, s := range []string{
		"", "a", "abc-123_XYZ", "bad id", "a\r\nX-Evil: 1", "café",
		strings.Repeat("a", 64), strings.Repeat("a", 65),
		"a:b", "a/b", "\x00\x01", "a\nb", "a\rb", "a\tb", "a b",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !webhttp.ValidRequestID(s) {
			return
		}
		if len(s) < 1 || len(s) > 64 {
			t.Fatalf("accepted id %q with out-of-range length %d", s, len(s))
		}
		for i := range len(s) {
			c := s[i]
			ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
				(c >= '0' && c <= '9') || c == '_' || c == '-'
			if !ok {
				t.Fatalf("accepted id %q contains disallowed byte %q", s, c)
			}
		}
	})
}

// FuzzRequestLogger_echoedIDIsAlwaysValid drives the middleware end-to-end and
// asserts the security invariant that the echoed X-Request-ID is a valid id for
// ANY inbound header bytes: a header-splitting / log-forging value never
// round-trips onto the response. When the inbound id fails ValidRequestID,
// RequestLogger must mint a fresh valid id rather than echo the untrusted input.
func FuzzRequestLogger_echoedIDIsAlwaysValid(f *testing.F) {
	for _, s := range []string{
		"", "inbound-123", "bad id!!", "abc\r\nX-Evil: 1",
		strings.Repeat("a", 65), "\x00\x01", "café",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, inbound string) {
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
		h := webhttp.RequestLogger(next, webhttp.WithLogger(discardLogger()))
		hdr := http.Header{}
		hdr.Set(webhttp.HeaderRequestID, inbound)
		rr := serve(h, http.MethodGet, "/x", hdr)
		echoed := rr.Header().Get(webhttp.HeaderRequestID)
		if !webhttp.ValidRequestID(echoed) {
			t.Fatalf("inbound %q produced echoed id %q that is not a valid request id", inbound, echoed)
		}
		if strings.ContainsAny(echoed, "\r\n") {
			t.Fatalf("echoed id %q contains CR/LF for inbound %q", echoed, inbound)
		}
	})
}

// FuzzWithTemplatePathsUnder_neverLogsAPathUnderADeclaredPrefix fuzzes the one
// untrusted-input boundary in this option whose OUTPUT lands in an aggregated
// log stream: the request path is entirely client-controlled, and the prefixes it
// is checked against are declared precisely because paths beneath them embed a
// credential.
//
// The invariant is a strong enumeration rather than a reimplementation of the
// branch logic: for ANY path under the declared prefix, the recorded value may
// only be a template registered on the mux or the prefix's "(unmatched)" marker.
// Anything else means client bytes — and therefore a live capability token —
// reached the log (CWE-532). Paths outside the prefix must come through
// untouched, since that is the property that keeps a static 404 diagnosable.
//
// Ported from web-terminal-kiro, which fuzzed its own hand-rolled version of this
// policy before the policy moved here. The app deleted its copy; the invariant
// belongs with the code.
func FuzzWithTemplatePathsUnder_neverLogsAPathUnderADeclaredPrefix(f *testing.F) {
	const prefix = "/api/sessions/"
	for _, seed := range []string{
		"/", "/healthz", "/api/sessions", "/api/sessions/events",
		prefix, prefix + "abc", prefix + "abc/title", prefix + "abc/",
		prefix + "/title", prefix + "abc/extra/title", prefix + "abc\nx",
		prefix + "abc%2f..%2f", prefix + "abc?x=1", "/api/sessionsx/abc",
		prefix + "abc/title/extra", prefix + "..", prefix + "abc/{id}",
	} {
		f.Add(seed)
	}

	// Every pattern registered on the mux, plus the unmatched marker. This is
	// the complete set of values the policy can produce for a path under the
	// prefix, and none of them contains client bytes: a pattern is a string the
	// server registered, and the marker is a constant appended to the declared
	// prefix.
	//
	// The set includes patterns OUTSIDE the prefix on purpose. ServeMux cleans
	// request paths, so a path that arrives under the subtree can be redirected
	// onto a different route -- "/api/sessions//title" collapses onto the subtree
	// pattern itself, and "/api/sessions/.." onto the catch-all "/" -- and the
	// recorded value is whatever actually matched. Both were found by this target
	// rather than reasoned about up front.
	allowed := map[string]bool{
		"/":                    true,
		prefix:                 true,
		prefix + "{id}":        true,
		prefix + "{id}/title":  true,
		prefix + "events":      true,
		prefix + "(unmatched)": true,
	}

	f.Fuzz(func(t *testing.T, path string) {
		// Built as a struct literal, not httptest.NewRequest, which panics on
		// target strings a fuzzer generates. The policy reads only URL.Path and
		// Pattern, and routing populates the latter.
		if !strings.HasPrefix(path, "/") {
			t.Skip("not a request path")
		}
		logCap := &captureHandler{}
		h := webhttp.RequestLogger(nestedSessionMux(),
			webhttp.WithLogger(slog.New(logCap)),
			webhttp.WithTemplatePathsUnder(prefix))

		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://x", nil)
		req.URL.Path = path
		h.ServeHTTP(httptest.NewRecorder(), req)

		recs := logCap.snapshot()
		if len(recs) != 1 {
			t.Fatalf("got %d log records for %q, want exactly 1", len(recs), path)
		}
		got, _ := attrsOf(recs[0])["path"].(string)

		if !strings.HasPrefix(path, prefix) {
			// The cap applies to this branch (the option returns a path outside
			// every declared prefix unchanged, so the recorded value is client
			// bytes), so "unchanged" means unchanged-up-to-the-bound.
			if len(path) <= 512 {
				if got != path {
					t.Errorf("path %q outside the declared prefix logged as %q, want it unchanged", path, got)
				}
				return
			}
			kept, ok := strings.CutSuffix(got, "...(truncated)")
			if !ok || !strings.HasPrefix(path, kept) || len(kept) > 512 {
				t.Errorf("over-cap path outside the declared prefix (%d bytes) logged as %q, want a prefix of it cut at 512 bytes plus the marker", len(path), got)
			}
			return
		}
		if !allowed[got] {
			t.Errorf("path %q under the declared prefix logged as %q, want a registered template or the unmatched marker; any other value carries client-supplied bytes into the access log", path, got)
		}
	})
}

// FuzzRequestLogger_loggedPathIsBounded pins the access log's path bound at the
// boundary that actually matters: r.URL.Path is entirely client-controlled, it
// lands in an aggregated log store, and net/http will carry a megabyte of it.
//
// Three invariants, asserted for BOTH path resolutions that can produce a
// recorded value from client bytes — the raw default and a caller's own
// WithPathFunc return:
//
//   - the recorded value never exceeds the cap plus the truncation marker, so a
//     flood of hostile URLs cannot evict an operator's warnings from the
//     retention window;
//   - a valid-UTF-8 input yields a valid-UTF-8 output, i.e. the cut never splits
//     a rune into bytes the log store rewrites as U+FFFD (an INVALID input
//     cannot become valid, so the invariant is conditional on the input — the
//     honest form of "never emit a split rune");
//   - a within-cap input is returned byte-identical, so the bound is invisible
//     to every real path.
func FuzzRequestLogger_loggedPathIsBounded(f *testing.F) {
	const (
		pathCap = 512
		marker  = "...(truncated)"
	)
	for _, seed := range []string{
		"", "/", "/api/thing", "/caf\u00e9", "/\U0001D11E",
		strings.Repeat("/a", 400),
		"/" + strings.Repeat("a", pathCap-1),   // exactly at the cap
		"/" + strings.Repeat("a", pathCap),     // one byte over
		"/" + strings.Repeat("a", pathCap*4),   // far over
		"/" + strings.Repeat("caf\u00e9", 200), // multi-byte runes across the cut
		"/" + strings.Repeat("a", pathCap-1) + "\u00e9x",
		"/" + strings.Repeat("a", pathCap-2) + "\u20acx",
		"/" + strings.Repeat("a", pathCap-2) + "\U0001D11Ex",
		"/\xff\xfe" + strings.Repeat("a", pathCap), // invalid UTF-8, over the cap
		"/a\nb", "/a\x00b",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, path string) {
		// Both resolutions, checked with the same invariants: a caller who
		// installs a path policy must not be able to opt out of the floor.
		for _, tc := range []struct {
			name string
			opts []webhttp.LogOption
		}{
			{"raw path", nil},
			{"caller transform", []webhttp.LogOption{
				webhttp.WithPathFunc(func(r *http.Request) string { return r.URL.Path }),
			}},
		} {
			logCap := &captureHandler{}
			opts := append([]webhttp.LogOption{webhttp.WithLogger(slog.New(logCap))}, tc.opts...)
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://x", nil)
			req.URL.Path = path
			webhttp.RequestLogger(okHandler(), opts...).ServeHTTP(httptest.NewRecorder(), req)

			recs := logCap.snapshot()
			if len(recs) != 1 {
				t.Fatalf("%s: got %d log records, want exactly 1", tc.name, len(recs))
			}
			got, _ := attrsOf(recs[0])["path"].(string)

			if len(got) > pathCap+len(marker) {
				t.Errorf("%s: input of %d bytes logged as %d bytes, want at most %d",
					tc.name, len(path), len(got), pathCap+len(marker))
			}
			if utf8.ValidString(path) && !utf8.ValidString(got) {
				t.Errorf("%s: valid-UTF-8 input of %d bytes logged as invalid UTF-8 (a rune was split)",
					tc.name, len(path))
			}
			if len(path) <= pathCap && got != path {
				// The one documented exception: an EMPTY return from a path
				// policy is the policy's failure signal, and fail-closed
				// coercion is older and more important than this bound.
				if tc.name != "caller transform" || path != "" {
					t.Errorf("%s: within-cap input %q logged as %q, want it byte-identical", tc.name, path, got)
				}
			}
		}
	})
}

// FuzzRouteMetricLabels_methodIsAlwaysAPermittedLabel pins the metric's method
// label to its closed set for ANY request method. r.Method is untrusted input
// that reaches a handler with only its CHARSET validated by net/http — a token
// of pure punctuation arrives intact, as does a megabyte of one letter — and a
// metric label is the one place that must not carry it: a series once minted is
// permanent for the process lifetime here and in every observer scraping it, so
// an unbounded label domain is a remote memory-exhaustion vector against the
// whole monitoring chain (CWE-770), reachable without authenticating because the
// hook fires from the access-log defer, outside every app auth gate.
//
// The invariant is an enumeration, not a reimplementation of the derivation: the
// returned method must be one of exactly ten values — the nine standard methods
// (RFC 9110 §9.3 plus PATCH) or the "other" bucket. Asserted for every pattern
// shape, because the whole point of the rework is that the bound owes nothing to
// the route table: an app with a "/" catch-all (never an empty r.Pattern) must
// be as bounded as one without.
func FuzzRouteMetricLabels_methodIsAlwaysAPermittedLabel(f *testing.F) {
	for _, m := range []string{
		"", "GET", "HEAD", "POST", "PUT", "DELETE", "CONNECT", "OPTIONS",
		"TRACE", "PATCH", "get", "Get", "gEt", "GETX", "GE", "PROPFIND",
		"PURGE", "M!#$%&'*+-.^_`|~", "GET ", " GET", "GET\tHEAD", "GET\r\nX",
		strings.Repeat("M", 25), strings.Repeat("M", 4096), "café", "\x00",
	} {
		f.Add(m)
	}

	permitted := map[string]bool{
		http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
		http.MethodPut: true, http.MethodDelete: true, http.MethodConnect: true,
		http.MethodOptions: true, http.MethodTrace: true, http.MethodPatch: true,
		"other": true,
	}

	f.Fuzz(func(t *testing.T, m string) {
		// Built as a struct literal rather than httptest.NewRequest, which
		// validates the method and panics on inputs a real request line carries.
		for _, pattern := range []string{
			"", "/", "/api/sessions/", "GET /beat/{id}", "GET example.com/beat/{id}",
		} {
			r := &http.Request{Method: m, Pattern: pattern, URL: &url.URL{Path: "/x"}}
			method, path := webhttp.RouteMetricLabels(r)
			if !permitted[method] {
				t.Fatalf("method %q with pattern %q produced label %q, which is outside the ten permitted values",
					m, pattern, method)
			}
			// The path label owes nothing to the method, so it must stay a
			// server-registered pattern or the fixed marker no matter what
			// arrives on the request line.
			wantPath := pattern
			if pattern == "" {
				wantPath = "unmatched"
			} else if _, template, ok := strings.Cut(pattern, " "); ok {
				wantPath = template
			}
			if path != wantPath {
				t.Fatalf("method %q with pattern %q produced path label %q, want %q",
					m, pattern, path, wantPath)
			}
		}
	})
}
