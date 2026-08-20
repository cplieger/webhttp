package webhttp

import (
	"strings"
	"testing"
)

// TestEqualASCIIFold pins the relation the package's safety comparisons rest
// on: case-insensitive within ASCII, byte-exact outside it. The cases that
// matter are the last three — they are the ones where strings.EqualFold
// answers differently, which is the whole reason this function exists.
func TestEqualASCIIFold(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		s, t string
		want bool
	}{
		{name: "identical", s: "localhost", t: "localhost", want: true},
		{name: "upper vs lower", s: "LOCALHOST", t: "localhost", want: true},
		{name: "mixed case", s: "LocalHost", t: "lOcAlHoSt", want: true},
		{name: "different length", s: "localhost", t: "localhos", want: false},
		{name: "different text", s: "localhost", t: "localhosu", want: false},
		{name: "empty pair", s: "", t: "", want: true},
		{name: "empty against non-empty", s: "", t: "l", want: false},
		{name: "digits and punctuation are untouched", s: "gzip-1.0", t: "GZIP-1.0", want: true},
		// The two divergences from strings.EqualFold: byte sequences Unicode
		// simple folding maps onto an ASCII letter, which is how a non-ASCII
		// input launders into a match against an ASCII literal.
		{name: "long s does not fold to s", s: "localho\u017ft", t: "localhost", want: false},
		{name: "kelvin sign does not fold to k", s: "\u212Aelvin", t: "kelvin", want: false},
		// U+0130 is the OTHER laundering class and is worth pinning next to
		// them: strings.EqualFold does NOT match it (Unicode full folding maps
		// it to two runes, and SimpleFold is 1:1, so it cannot), but
		// strings.ToLower DOES map it to 'i'. So a gate written with ToLower +
		// exact match launders where one written with EqualFold does not.
		// An ASCII fold is closed against both.
		{name: "dotted capital I does not fold to i", s: "\u0130nput", t: "input", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := equalASCIIFold(tc.s, tc.t); got != tc.want {
				t.Errorf("equalASCIIFold(%q, %q) = %v, want %v", tc.s, tc.t, got, tc.want)
			}
			// Symmetry is a property of the relation, not an accident of
			// argument order.
			if got := equalASCIIFold(tc.t, tc.s); got != tc.want {
				t.Errorf("equalASCIIFold(%q, %q) = %v, want %v (asymmetric)", tc.t, tc.s, got, tc.want)
			}
		})
	}
}

// TestEqualASCIIFoldDivergesFromStringsEqualFold is the red-check for the
// reason this function exists: it asserts that the laundering inputs above are
// exactly the cases where the stdlib would have said yes. If a future Go
// release stopped folding them, this test says so rather than letting the local
// helper look gratuitous.
//
// It also pins the second, separate laundering channel: U+0130 passes
// strings.EqualFold (Unicode SIMPLE folding cannot reach it) but strings.ToLower
// maps it to 'i'. The two mechanisms admit different inputs, so a gate is only
// closed when it folds ASCII rather than picking either stdlib helper.
func TestEqualASCIIFoldDivergesFromStringsEqualFold(t *testing.T) {
	t.Parallel()
	foldLaunderings := []struct{ nonASCII, ascii string }{
		{"localho\u017ft", "localhost"},
		{"\u212Aelvin", "kelvin"},
	}
	for _, l := range foldLaunderings {
		if !strings.EqualFold(l.nonASCII, l.ascii) {
			t.Errorf("strings.EqualFold(%q, %q) = false; this input no longer launders, so the divergence case is stale", l.nonASCII, l.ascii)
		}
		if equalASCIIFold(l.nonASCII, l.ascii) {
			t.Errorf("equalASCIIFold(%q, %q) = true, want false", l.nonASCII, l.ascii)
		}
	}

	// The ToLower channel, which EqualFold does not cover.
	if got := strings.ToLower("\u0130"); got != "i" {
		t.Errorf(`strings.ToLower("\u0130") = %q, want "i"; the ToLower laundering channel has changed`, got)
	}
	if equalASCIIFold("\u0130nput", "input") {
		t.Error(`equalASCIIFold("\u0130nput", "input") = true, want false`)
	}
}
