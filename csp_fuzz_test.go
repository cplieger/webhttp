package webhttp

import (
	"bytes"
	"regexp"
	"slices"
	"testing"
)

// cspTokenRE is the exact shape of a valid CSP sha256 source token: 43 base64
// characters plus the '=' pad (a 32-byte digest), quoted.
var cspTokenRE = regexp.MustCompile(`^'sha256-[A-Za-z0-9+/]{43}='$`)

// FuzzInlineScriptHashesTokensWellFormedAndDeterministic fuzzes the inline
// <script> scanner with arbitrary HTML-ish bytes and holds three invariants:
// it never panics, every returned token is a well-formed CSP sha256 source
// token, the token count never exceeds the count of "<script" occurrences
// (each token consumes one opening tag), and a second scan of the same input
// returns the identical result (the scanner is a pure function).
func FuzzInlineScriptHashesTokensWellFormedAndDeterministic(f *testing.F) {
	seeds := []string{
		"",
		"<html><body>hi</body></html>",
		"<script>let a=1;</script>",
		`<script src="/vendor/x.js"></script>`,
		`<script type="importmap">{"i":1}</script><script type="module">go()</script>`,
		"<SCRIPT>x=3</SCRIPT>",
		`<script data-src="x">y=4</script>`,
		`<script data-x="a>b">q=6</script>`,
		"<script>never closed",
		"<scriptfoo>nope</scriptfoo>",
		`<script srcset="x">r=7</script>`,
		"<script src='a'><script>nested</script>",
		"</script><script></script>",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	scriptOpenRE := regexp.MustCompile(`(?i)<script`)
	f.Fuzz(func(t *testing.T, html []byte) {
		got := InlineScriptHashes(html)
		for _, tok := range got {
			if !cspTokenRE.MatchString(tok) {
				t.Fatalf("malformed CSP token %q for input %q", tok, html)
			}
		}
		if opens := len(scriptOpenRE.FindAll(html, -1)); len(got) > opens {
			t.Fatalf("returned %d tokens but input has only %d <script openings", len(got), opens)
		}
		again := InlineScriptHashes(bytes.Clone(html))
		if !slices.Equal(got, again) {
			t.Fatalf("scanner is not deterministic: first %v, second %v", got, again)
		}
	})
}
