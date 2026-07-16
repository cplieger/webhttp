package webhttp

import (
	"crypto/sha256"
	"encoding/base64"
)

// InlineScriptHashes scans HTML for inline <script> elements (those WITHOUT a
// src attribute) and returns a CSP source token 'sha256-<base64>' for each,
// hashing the exact bytes between the element's '>' and its '</script>' —
// precisely the content a browser hashes for a Content-Security-Policy
// script-src hash. External (src=) scripts are skipped; 'self' already covers
// them.
//
// It exists so an app serving a build-controlled embedded page (an importmap
// plus a module bootstrap are the classic pair) can pin its script-src to
// exact hashes instead of 'unsafe-inline', computing the tokens at startup
// from the very bytes it will serve — a policy that then survives any
// reformat or rebuild with no hand-maintained hash constant. Feed the result
// into the app's policy string and pass that via WithCSP; the library builds
// no policy itself (a CSP is application-specific).
//
// The scanner is byte-precise and dependency-free: case-insensitive tag
// matching, quote-aware attribute scanning (a '>' or "src=" inside a quoted
// attribute value does not confuse it), and "src" matched only as a real
// attribute name (srcset and data-src do not count). It is an extractor for
// pages the APP controls, not an HTML sanitizer for untrusted input. It
// returns an empty slice on script-less or malformed input; a caller whose
// page is known to carry inline scripts should treat an empty result as a
// malformed build and fail startup rather than degrade its policy.
func InlineScriptHashes(html []byte) []string {
	var out []string
	for i := 0; i < len(html); {
		open := findScriptOpen(html, i)
		if open < 0 {
			break
		}
		gt := openTagEnd(html, open+len("<script"))
		if gt < 0 {
			break
		}
		closeIdx := findScriptClose(html, gt+1)
		if closeIdx < 0 {
			break
		}
		if !hasSrcAttr(html[open+len("<script") : gt]) {
			out = append(out, cspHash(html[gt+1:closeIdx]))
		}
		i = closeIdx + len("</script")
	}
	return out
}

// cspHash returns the CSP source token 'sha256-<std-base64>' for content.
func cspHash(content []byte) string {
	sum := sha256.Sum256(content)
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// findScriptOpen returns the index at or after `from` of the next "<script"
// tag start — case-insensitive, and only where "<script" is followed by a tag
// boundary so "<scriptfoo" does not match — or -1.
func findScriptOpen(html []byte, from int) int {
	for i := from; ; {
		s := indexFoldASCII(html, i, "<script")
		if s < 0 {
			return -1
		}
		after := s + len("<script")
		if after >= len(html) || isTagNameBoundary(html[after]) {
			return s
		}
		i = after
	}
}

// findScriptClose returns the index at or after `from` of the next "</script"
// (case-insensitive), or -1.
func findScriptClose(html []byte, from int) int {
	return indexFoldASCII(html, from, "</script")
}

// openTagEnd returns the index of the '>' that closes an opening tag starting
// at `from`, skipping any '>' inside a quoted attribute value, or -1.
func openTagEnd(html []byte, from int) int {
	var quote byte
	for i := from; i < len(html); i++ {
		switch c := html[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return i
		}
	}
	return -1
}

// hasSrcAttr reports whether the opening-tag attribute bytes of a <script>
// element (the bytes between "<script" and its closing '>') declare a src
// attribute. It matches `src` only at an attribute-name position and skips
// quoted values, so "srcset", "data-src", and a "src=" inside a value are not
// mistaken for it.
func hasSrcAttr(attrs []byte) bool {
	var quote byte
	atName := true
	for i := range attrs {
		switch c := attrs[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote, atName = c, false
		case isASCIISpace(c):
			atName = true
		case atName && matchesSrcHere(attrs, i):
			return true
		default:
			atName = false
		}
	}
	return false
}

// matchesSrcHere reports whether attrs at index i begins the attribute name
// "src" (case-insensitive) followed, after optional whitespace, by '=' — a real
// src attribute rather than a longer name such as "srcset".
func matchesSrcHere(attrs []byte, i int) bool {
	if !hasFoldPrefix(attrs[i:], "src") {
		return false
	}
	j := i + len("src")
	for j < len(attrs) && isASCIISpace(attrs[j]) {
		j++
	}
	return j < len(attrs) && attrs[j] == '='
}

// indexFoldASCII returns the index at or after `from` of the first
// ASCII-case-insensitive match of the lowercase literal `needle` in b, or -1.
// It scans b directly (no allocation), so returned indices address the original
// bytes — required for slicing the exact content a browser hashes.
func indexFoldASCII(b []byte, from int, needle string) int {
	for i := from; i <= len(b)-len(needle); i++ {
		if hasFoldPrefix(b[i:], needle) {
			return i
		}
	}
	return -1
}

// hasFoldPrefix reports whether b begins with the lowercase ASCII literal
// `lowerNeedle`, comparing ASCII letters case-insensitively.
func hasFoldPrefix(b []byte, lowerNeedle string) bool {
	if len(b) < len(lowerNeedle) {
		return false
	}
	for i := range len(lowerNeedle) {
		if lowerASCII(b[i]) != lowerNeedle[i] {
			return false
		}
	}
	return true
}

// lowerASCII returns c lowercased if it is an ASCII uppercase letter, else c.
func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// isTagNameBoundary reports whether c ends an HTML tag name ('>', '/', or ASCII
// whitespace).
func isTagNameBoundary(c byte) bool {
	return c == '>' || c == '/' || isASCIISpace(c)
}

// isASCIISpace reports whether c is an HTML ASCII whitespace byte.
func isASCIISpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f':
		return true
	default:
		return false
	}
}
