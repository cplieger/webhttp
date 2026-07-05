package webhttp_test

import (
	"net/http"
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
