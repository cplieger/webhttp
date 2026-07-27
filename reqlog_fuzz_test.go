package webhttp_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
			if got != path {
				t.Errorf("path %q outside the declared prefix logged as %q, want it unchanged", path, got)
			}
			return
		}
		if !allowed[got] {
			t.Errorf("path %q under the declared prefix logged as %q, want a registered template or the unmatched marker; any other value carries client-supplied bytes into the access log", path, got)
		}
	})
}
