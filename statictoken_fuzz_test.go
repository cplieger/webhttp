package webhttp_test

import (
	"strings"
	"testing"

	"github.com/cplieger/webhttp"
)

// FuzzStaticTokenVerifier drives NewStaticTokenVerifier(...).Verify over
// arbitrary configured/presented pairs and asserts, for every input:
//
//  1. It never panics (fuzz-harness invariant).
//  2. Fail-closed: an empty configured secret NEVER verifies, whatever is
//     presented — including the empty string, the sha256("")==sha256("")
//     trap the constructor guard exists for — and the zero-value verifier
//     behaves identically.
//  3. String-equality oracle: for a non-empty configured secret the decision
//     equals plain string equality. The hash-then-compare pipeline changes
//     the timing profile, never the accept set — no false accepts from a
//     hashing bug (truncated digest, wrong input hashed), no false rejects.
//  4. Identity: every non-empty configured secret verifies against itself,
//     and the verifier is reusable (a second Verify sees the same digest).
func FuzzStaticTokenVerifier(f *testing.F) {
	seeds := [][2]string{
		{"", ""},
		{"", "presented"},
		{"", " "},
		{"secret", ""},
		{"secret", "secret"},
		{"secret", "Secret"},
		{"secret", "secret "},
		{"a", "aa"},
		{"aa", "a"},
		{"a\x00b", "ab"},
		{"pässwörd-日本語", "pässwörd-日本語"},
		{strings.Repeat("k", 300), strings.Repeat("k", 300)},
		{strings.Repeat("k", 300), strings.Repeat("k", 299) + "x"},
	}
	for _, s := range seeds {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, configured, presented string) {
		v := webhttp.NewStaticTokenVerifier(configured)
		got := v.Verify(presented)
		if configured == "" {
			if got {
				t.Fatalf("NewStaticTokenVerifier(%q).Verify(%q) = true; empty configured must fail closed", configured, presented)
			}
			var zero webhttp.StaticTokenVerifier
			if zero.Verify(presented) {
				t.Fatalf("zero StaticTokenVerifier.Verify(%q) = true; zero value must fail closed", presented)
			}
			return
		}
		if want := configured == presented; got != want {
			t.Fatalf("NewStaticTokenVerifier(%q).Verify(%q) = %v, want %v (string-equality oracle)", configured, presented, got, want)
		}
		if !v.Verify(configured) {
			t.Fatalf("NewStaticTokenVerifier(%q).Verify(<itself>) = false, want true (identity)", configured)
		}
	})
}
