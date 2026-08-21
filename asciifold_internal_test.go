package webhttp

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
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

// TestLowerASCIIString pins the string-shaped fold: ASCII case-insensitive,
// byte-exact outside ASCII, and identity-returning (no copy) when there is
// nothing to fold.
func TestLowerASCIIString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, want string
	}{
		{name: "already lower", in: "webterm.example.com", want: "webterm.example.com"},
		{name: "all upper", in: "WEBTERM.EXAMPLE.COM", want: "webterm.example.com"},
		{name: "mixed", in: "Webterm.Example.COM", want: "webterm.example.com"},
		{name: "empty", in: "", want: ""},
		{name: "digits and punctuation untouched", in: "my_service-1.2:80", want: "my_service-1.2:80"},
		// Both ends of 'A'-'Z' are INCLUDED, and the endpoints need their own
		// cases because the scan that decides whether to copy is what sees a
		// byte first: an endpoint the scan skips stays unfolded even though the
		// copy loop it precedes would have lowered it. The cases above cannot
		// show that — their first uppercase byte is interior to the range.
		{name: "leading A folds", in: "AURORA", want: "aurora"},
		{name: "leading Z folds", in: "ZONE", want: "zone"},
		// The whole point: a non-ASCII rune keeps its bytes, so a later
		// byte-class check can refuse it. strings.ToLower would fold the
		// first two into ASCII.
		{name: "dotted capital I keeps its bytes", in: "k\u0130bana", want: "k\u0130bana"},
		{name: "kelvin sign keeps its bytes", in: "\u212Aibana", want: "\u212Aibana"},
		{name: "long s keeps its bytes", in: "localho\u017ft", want: "localho\u017ft"},
		{name: "non-ascii beside ascii upper", in: "K\u212AX", want: "k\u212Ax"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := lowerASCIIString(tc.in); got != tc.want {
				t.Errorf("lowerASCIIString(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// Idempotence: the fold is a projection.
			if got := lowerASCIIString(tc.want); got != tc.want {
				t.Errorf("lowerASCIIString not idempotent: lowerASCIIString(%q) = %q, want %q", tc.want, got, tc.want)
			}
		})
	}
}

// TestLowerASCIIStringLaunderingSetIsExactlyTwoRunes is the tripwire behind
// CanonicalHost's byte-exact claim, and it is an exhaustive statement rather
// than a sample: over all 1,114,112 code points, exactly two runes lowercase
// under strings.ToLower into a string made only of bytes CanonicalHost's
// validHostname accepts — U+0130 to "i" and U+212A to "k". Measured identical
// on go1.26.7 (Unicode 15) and go1.27.0 (Unicode 17), so the set is a property
// of Unicode case mapping and not of one toolchain.
//
// Two things this catches that a fixed table cannot. A future Unicode bump
// adding a third such rune fails here, naming it, instead of silently widening
// every gate built on a Unicode fold. And it red-checks the local fold: if
// lowerASCIIString ever started agreeing with strings.ToLower on these runes,
// the second half fails.
func TestLowerASCIIStringLaunderingSetIsExactlyTwoRunes(t *testing.T) {
	t.Parallel()

	hostByte := func(b byte) bool {
		switch {
		case b >= '0' && b <= '9', b >= 'a' && b <= 'z', b == '-', b == '_', b == '.':
			return true
		}
		return false
	}
	allHostBytes := func(s string) bool {
		if s == "" {
			return false
		}
		for i := range len(s) {
			if !hostByte(s[i]) {
				return false
			}
		}
		return true
	}

	var launder []rune
	for r := rune(utf8.RuneSelf); r <= utf8.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		if allHostBytes(strings.ToLower(string(r))) {
			launder = append(launder, r)
		}
	}
	want := []rune{'\u0130', '\u212A'}
	if !slices.Equal(launder, want) {
		t.Errorf("runes whose strings.ToLower lands in the hostname byte class = %U, want %U;\n"+
			"a rune added here launders a non-ASCII authority onto an ASCII allowlist key under any Unicode fold",
			launder, want)
	}

	// The local fold refuses every one of them, which is what makes
	// CanonicalHost's "no IDN mapping is performed" claim true.
	for _, r := range launder {
		if got := lowerASCIIString(string(r)); got != string(r) {
			t.Errorf("lowerASCIIString(%q) = %q, want the input unchanged (U+%04X)", string(r), got, r)
		}
	}
}
