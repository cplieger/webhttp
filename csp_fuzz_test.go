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

// FuzzInlineStyleHashesTokensWellFormedAndDeterministic is the style-scanner
// counterpart, holding the same invariants (never panics, every token is a
// well-formed CSP sha256 source token, the token count never exceeds the count of
// "<style" occurrences, and the scanner is pure) plus one the script target
// cannot express: a <style> hash must never be produced for a style ATTRIBUTE or
// a <link rel=stylesheet>, neither of which a style-src element hash covers.
//
// It exists as its own target rather than as extra assertions on the script one
// because both funnel through the shared inlineElementHashes core: a boundary
// bug there would show up on whichever tag the fuzzer happened to explore, and
// two targets explore both.
func FuzzInlineStyleHashesTokensWellFormedAndDeterministic(f *testing.F) {
	seeds := []string{
		"",
		"<html><body>hi</body></html>",
		"<style>body{margin:0}</style>",
		"<style>a{color:red}</style><style>b{color:blue}</style>",
		"<STYLE>c{top:0}</STYLE>",
		`<style media="print">f{bottom:0}</style>`,
		`<style media="all and (min-width:1px)" data-x="a>b">e{right:0}</style>`,
		"<style>never closed",
		"<styles>nope</styles>",
		`<link rel="stylesheet" href="/style.css">`,
		`<div style="color:red">x</div>`,
		"</style><style></style>",
		`<style src="x">g{gap:0}</style>`,
		// Both tags on one page: the shared core must not cross the boundary.
		`<style>body{margin:0}</style><script type="module">boot()</script>`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	styleOpenRE := regexp.MustCompile(`(?i)<style`)
	f.Fuzz(func(t *testing.T, html []byte) {
		got := InlineStyleHashes(html)
		for _, tok := range got {
			if !cspTokenRE.MatchString(tok) {
				t.Fatalf("malformed CSP token %q for input %q", tok, html)
			}
		}
		if opens := len(styleOpenRE.FindAll(html, -1)); len(got) > opens {
			t.Fatalf("returned %d tokens but input has only %d <style openings", len(got), opens)
		}
		again := InlineStyleHashes(bytes.Clone(html))
		if !slices.Equal(got, again) {
			t.Fatalf("scanner is not deterministic: first %v, second %v", got, again)
		}
		// No <style> element means no style hash, however many style attributes or
		// stylesheet links the input carries.
		if len(got) > 0 && !styleOpenRE.Match(html) {
			t.Fatalf("returned %d tokens with no <style> opening in %q", len(got), html)
		}
	})
}
