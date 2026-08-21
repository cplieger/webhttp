package webhttp

import (
	"crypto/sha256"
	"encoding/base64"
	"slices"
	"strings"
	"testing"
)

// hashToken computes the expected CSP source token for an inline script body,
// independently of the production cspHash (same math, separate expression).
func hashToken(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

func TestInlineScriptHashes(t *testing.T) {
	cases := []struct {
		name string
		html string
		want []string
	}{
		{"no scripts", `<html><body>hi</body></html>`, nil},
		{"single inline", `<head><script>let a=1;</script></head>`, []string{hashToken("let a=1;")}},
		{"external skipped", `<script src="/vendor/x.js"></script>`, nil},
		{"external with type skipped", `<script type="module" src="/x.js"></script>`, nil},
		{"mixed inline and external", `<script src="/x.js"></script><script>b=2</script>`, []string{hashToken("b=2")}},
		{
			"two inline preserve order",
			`<script type="importmap">{"i":1}</script><script type="module">go()</script>`,
			[]string{hashToken(`{"i":1}`), hashToken("go()")},
		},
		{"case-insensitive tag", `<SCRIPT>x=3</SCRIPT>`, []string{hashToken("x=3")}},
		{"data-src is not a src attribute", `<script data-src="x">y=4</script>`, []string{hashToken("y=4")}},
		{"newlines hashed verbatim", "<script>\n  z=5\n</script>", []string{hashToken("\n  z=5\n")}},
		{"scriptfoo is not a script tag", `<scriptfoo>nope</scriptfoo>`, nil},
		{"gt inside quoted attribute does not end the tag", `<script data-x="a>b">q=6</script>`, []string{hashToken("q=6")}},
		{"srcset is not src", `<script srcset="x">r=7</script>`, []string{hashToken("r=7")}},
		// A bare "src" is not the external form the skip rule names ("src="), and
		// the scanner must not read past the attribute bytes looking for the '='
		// that is not there.
		{"src with no value is not the external form", `<script src>x=8</script>`, []string{hashToken("x=8")}},
		{"src with spaces around its = is still external", `<script src = "/x.js"></script>`, nil},
		{"unclosed script yields nothing", `<script>never closed`, nil},
		{"open tag never terminated yields nothing", `<script type=module`, nil},
		{"input ending inside the tag name yields nothing", `<p><script`, nil},
		// The close side needs the same tag-name boundary the open side has:
		// "</scriptfoo>" is content, not this element's end tag, so the span runs
		// on to the real one. Ending it early would hash bytes no browser hashes
		// and ship a policy that blocks the page.
		{
			"a longer close tag name does not end the element",
			`<script>a="</scriptfoo>";b=9</script>`,
			[]string{hashToken(`a="</scriptfoo>";b=9`)},
		},
		// An end tag truncated by end-of-input never leaves script data either, so
		// there is no span to hash.
		{"close tag truncated at end of input yields nothing", `<script>x=10</script`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := InlineScriptHashes([]byte(tc.html))
			if !slices.Equal(got, tc.want) {
				t.Errorf("InlineScriptHashes(%q) = %v, want %v", tc.html, got, tc.want)
			}
		})
	}
}

func TestInlineStyleHashes(t *testing.T) {
	cases := []struct {
		name string
		html string
		want []string
	}{
		{"no styles", `<html><body>hi</body></html>`, nil},
		{"single inline", `<head><style>body{margin:0}</style></head>`, []string{hashToken("body{margin:0}")}},
		{
			"two inline preserve order",
			`<style>a{color:red}</style><style>b{color:blue}</style>`,
			[]string{hashToken("a{color:red}"), hashToken("b{color:blue}")},
		},
		{"case-insensitive tag", `<STYLE>c{top:0}</STYLE>`, []string{hashToken("c{top:0}")}},
		{"newlines hashed verbatim", "<style>\n  d{left:0}\n</style>", []string{hashToken("\n  d{left:0}\n")}},
		{"styles is not a style tag", `<styles>nope</styles>`, nil},
		{"unclosed style yields nothing", `<style>never closed`, nil},
		// The shared core's close-side boundary rule, on the tag whose plural is
		// the natural collision: "</styles>" is content, not this element's end
		// tag.
		{
			"a longer close tag name does not end the element",
			`<style>a{content:"</styles>"}</style>`,
			[]string{hashToken(`a{content:"</styles>"}`)},
		},
		{"close tag truncated at end of input yields nothing", `<style>j{z:0}</style`, nil},
		{
			"gt inside quoted attribute does not end the tag",
			`<style media="all and (min-width:1px)" data-x="a>b">e{right:0}</style>`,
			[]string{hashToken("e{right:0}")},
		},
		// A media attribute does not change what a browser hashes, and unlike the
		// script scanner there is no external-reference form to skip: a stylesheet
		// reference is <link rel="stylesheet">, which style-src covers via 'self'.
		{"media attribute still hashed", `<style media="print">f{bottom:0}</style>`, []string{hashToken("f{bottom:0}")}},
		{"src attribute is meaningless on style and does not skip it", `<style src="x">g{gap:0}</style>`, []string{hashToken("g{gap:0}")}},
		// A <link rel=stylesheet> is not a <style> element and must not be hashed.
		{"link stylesheet ignored", `<link rel="stylesheet" href="/style.css">`, nil},
		{"style attribute is not a style element", `<div style="color:red">x</div>`, nil},
		// Boundary cases inherited from web-terminal-kiro's local scanner when the
		// library took over the style half: attributes and tag case must not enter
		// the hash, and a spaced close tag still terminates the block.
		{"attributes excluded from the hash", `<style type="text/css" media="all">h{x:0}</style>`, []string{hashToken("h{x:0}")}},
		{"spaced close tag", `<style>i{y:0}</style >`, []string{hashToken("i{y:0}")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := InlineStyleHashes([]byte(tc.html))
			if !slices.Equal(got, tc.want) {
				t.Errorf("InlineStyleHashes(%q) = %v, want %v", tc.html, got, tc.want)
			}
		})
	}
}

// TestInlineHashesSurviveLengthChangingFold is the sharpest boundary case, and it
// is why the scanner folds ASCII in place instead of calling bytes.ToLower: a
// unicode-aware fold can CHANGE a rune's byte length (U+0130 folds to the 1-byte
// 'i'), sliding every index computed on the folded copy off the ORIGINAL bytes so
// the scanner hashes the wrong content and ships a policy that blocks the page.
// Pure-ASCII content cannot tell the two implementations apart, so only content
// carrying such a rune tests it. Inherited from web-terminal-kiro's local style
// scanner when the library took over that half; asserted for both tags, since
// they now share one core.
func TestInlineHashesSurviveLengthChangingFold(t *testing.T) {
	const content = "\n  body { content: '\u0130' }\n"
	want := hashToken(content)

	if got := InlineStyleHashes([]byte("<html><style>" + content + "</style></html>")); !slices.Equal(got, []string{want}) {
		t.Errorf("InlineStyleHashes = %v, want %v (a length-changing fold slides the content indices)", got, []string{want})
	}
	if got := InlineScriptHashes([]byte("<html><script>" + content + "</script></html>")); !slices.Equal(got, []string{want}) {
		t.Errorf("InlineScriptHashes = %v, want %v (a length-changing fold slides the content indices)", got, []string{want})
	}
}

// The two extractors share one scanner core, so pin that they stay independent:
// scripts must not pick up style content and vice versa, on a page carrying both.
func TestInlineHashesDoNotCrossContaminate(t *testing.T) {
	html := []byte(`<head><style>body{margin:0}</style><script type="module">boot()</script></head>`)
	styles := InlineStyleHashes(html)
	scripts := InlineScriptHashes(html)
	if !slices.Equal(styles, []string{hashToken("body{margin:0}")}) {
		t.Errorf("InlineStyleHashes = %v, want the style body only", styles)
	}
	if !slices.Equal(scripts, []string{hashToken("boot()")}) {
		t.Errorf("InlineScriptHashes = %v, want the script body only", scripts)
	}
	// A style hash must never equal the script hash on this page, which would mean
	// the shared core sliced the wrong content boundary for one of them.
	if styles[0] == scripts[0] {
		t.Error("style and script hashes are identical; the shared scanner sliced the same bytes twice")
	}
}

// TestCSPHashTokenFormat pins that cspHash emits a CSP-grammar source token: a
// standard-base64 encoding of a 32-byte sha256 digest wrapped as 'sha256-...'.
// It validates the encoding/format without hardcoding any expected hash value.
func TestCSPHashTokenFormat(t *testing.T) {
	tok := cspHash([]byte("console.log(1)"))
	if !strings.HasPrefix(tok, "'sha256-") || !strings.HasSuffix(tok, "'") {
		t.Fatalf("token = %q, want the 'sha256-<base64>' form", tok)
	}
	b64 := strings.TrimSuffix(strings.TrimPrefix(tok, "'sha256-"), "'")
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("hash %q is not valid standard base64: %v", b64, err)
	}
	if len(raw) != sha256.Size {
		t.Errorf("decoded hash = %d bytes, want %d (sha256)", len(raw), sha256.Size)
	}
}
