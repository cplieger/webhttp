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
		{"unclosed script yields nothing", `<script>never closed`, nil},
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
