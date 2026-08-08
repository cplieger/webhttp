package webhttp_test

import (
	"strings"
	"testing"

	"github.com/cplieger/webhttp"
)

// FuzzCanonicalRequestPath_idempotentAndExactVerdict fuzzes the parser over
// arbitrary bytes (a request path is untrusted input) and holds five invariants:
//
//  1. Idempotence — CanonicalRequestPath(clean) returns (clean, true). The
//     value handed back must itself BE canonical, or a caller that refuses a
//     request and redirects to clean would refuse the redirect too, and a
//     caller comparing against a stored clean path could never match.
//  2. Verdict exactness — canonical is exactly p == clean, never a looser or
//     stricter reading of "the mux routes this". A verdict that drifts from the
//     returned value is the one way this surface can lie to both of its
//     callers at once.
//  3. Rooted output — clean is never empty and always begins with "/", so a
//     caller can treat it as a path without re-checking (net/http's own
//     cleanPath roots its input the same way).
//  4. Structurally canonical output — clean contains no "//" and no "." or
//     ".." element. This is what "canonical" MEANS spelled out
//     structurally, independent of the equality in invariant 1, so a cleaner
//     that became idempotent by simply returning its input would still fail.
//  5. Bounded output — len(clean) <= len(p)+1. Rooting adds at most one byte
//     and cleaning only removes them, so no input can make the result grow.
func FuzzCanonicalRequestPath_idempotentAndExactVerdict(f *testing.F) {
	seeds := []string{
		"", "/", "//", "///", "/beat", "/beat/", "/beat/api", "/beat/api/",
		"/beat//", "/beat//api", "//beat/api", "/beat/./api",
		"/beat/api/../ghost", "/beat/..", "/beat/../", "/../beat",
		"/.", "/./", "/..", "/a/b/../../..", "beat/api", ".", "..",
		"/beat/%2e%2e/ghost", "/beat/%2F/api", "/beat/{id}",
		"/api/sessions//title", "/api/sessions/..", "/dump/.",
		"/beat/\x00", "/beat/ ", "/beat/\n", "/beat/\\..", "/beat/é",
		"\x80\x81", "/beat/api?x=1", "/beat/api#frag", strings.Repeat("/a", 512),
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		clean, canonical := webhttp.CanonicalRequestPath(p)

		// Invariant 3: rooted, non-empty.
		if clean == "" || clean[0] != '/' {
			t.Fatalf("CanonicalRequestPath(%q) clean = %q, want a non-empty rooted path", p, clean)
		}

		// Invariant 2: the verdict is exactly the equality.
		if canonical != (p == clean) {
			t.Fatalf("CanonicalRequestPath(%q) canonical = %v but clean = %q (equal: %v)",
				p, canonical, clean, p == clean)
		}

		// Invariant 4: the output is structurally canonical.
		if strings.Contains(clean, "//") {
			t.Fatalf("CanonicalRequestPath(%q) clean = %q contains a repeated slash", p, clean)
		}
		for elem := range strings.SplitSeq(clean, "/") {
			if elem == "." || elem == ".." {
				t.Fatalf("CanonicalRequestPath(%q) clean = %q retains a %q element", p, clean, elem)
			}
		}

		// Invariant 5: rooting costs at most one byte, cleaning only removes.
		if len(clean) > len(p)+1 {
			t.Fatalf("CanonicalRequestPath(%q) clean = %q grew the input (%d > %d)",
				p, clean, len(clean), len(p)+1)
		}

		// Invariant 1: idempotence — the returned path is itself canonical.
		again, againCanonical := webhttp.CanonicalRequestPath(clean)
		if again != clean || !againCanonical {
			t.Fatalf("CanonicalRequestPath(%q) = (%q, %v), want (%q, true): cleaning is not idempotent for input %q",
				clean, again, againCanonical, clean, p)
		}
	})
}
