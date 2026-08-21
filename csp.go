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
// matching on a tag-name boundary at BOTH ends (neither "<scriptfoo" nor
// "</scriptfoo>" belongs to this element, matching the tokenizer), quote-aware
// attribute scanning (a '>' or "src=" inside a quoted attribute value does not
// confuse it), and "src" matched only as a real attribute name (srcset and
// data-src do not count). It is an extractor for pages the APP controls, not an
// HTML sanitizer for untrusted input. It returns an empty slice on script-less
// or malformed input — an element whose end tag is truncated by end-of-input has
// no span a browser would hash, so it yields nothing; a caller whose page is
// known to carry inline scripts should treat an empty result as a malformed
// build and fail startup rather than degrade its policy.
func InlineScriptHashes(html []byte) []string {
	return inlineElementHashes(html, "script", hasSrcAttr)
}

// InlineStyleHashes scans HTML for inline <style> elements and returns a CSP
// source token 'sha256-<base64>' for each, hashing the exact bytes between the
// element's '>' and its '</style>' — precisely the content a browser hashes for
// a Content-Security-Policy style-src hash.
//
// It is the style-src counterpart of InlineScriptHashes and shares its scanner
// core, so the byte-boundary and malformed-tag behavior cannot drift between the
// two. It exists for the same reason: an app serving a build-controlled embedded
// page can pin style-src to exact hashes instead of 'unsafe-inline', computed at
// startup from the very bytes it will serve. A pre-JS loading overlay whose CSS
// must paint before the external stylesheet loads is the classic case — and one
// a hash suits well, because that block is build-controlled.
//
// There is no skip rule, unlike the script scanner's external-src case: a <style>
// element always carries its content inline (a stylesheet reference is
// <link rel="stylesheet">, which style-src covers via 'self'). A media attribute
// does not change what a browser hashes, so such blocks are hashed like any
// other.
//
// Note what a style-src hash does NOT cover: inline style ATTRIBUTES
// (style="..."), which are governed by style-src-attr and need 'unsafe-hashes'
// rather than an element hash. This function is for <style> ELEMENTS only, so an
// app whose markup or renderer sets style attributes cannot drop 'unsafe-inline'
// on the strength of these tokens alone. A renderer driving CSSOM property
// setters (element.style.color = …) emits no attribute and is unaffected.
//
// Same posture as the script scanner: an extractor for pages the APP controls,
// not an HTML sanitizer for untrusted input. It returns an empty slice on
// style-less or malformed input; a caller whose page is known to carry an inline
// style block should treat an empty result as a malformed build and fail startup
// rather than degrade its policy.
func InlineStyleHashes(html []byte) []string {
	return inlineElementHashes(html, "style", nil)
}

// inlineElementHashes is the shared scanner behind InlineScriptHashes and
// InlineStyleHashes: it walks every `tag` element in html and hashes each one's
// exact content bytes. `skip`, when non-nil, is consulted with the opening tag's
// attribute bytes and suppresses the hash for that element (the script scanner's
// external-src case); a nil skip hashes every element found.
//
// One core rather than one scanner per tag, because the delicate parts — where
// the content starts and ends, quote-aware tag scanning, the fold-preserving
// index arithmetic that keeps slices addressing the ORIGINAL bytes — are exactly
// what a browser must agree with byte for byte. A second copy that drifted would
// produce a policy that silently blocks the page it was meant to protect.
func inlineElementHashes(html []byte, tag string, skip func(attrs []byte) bool) []string {
	openTag := "<" + tag
	closeTag := "</" + tag
	var out []string
	for i := 0; i < len(html); {
		open := findElementOpen(html, i, openTag)
		if open < 0 {
			break
		}
		gt := openTagEnd(html, open+len(openTag))
		if gt < 0 {
			break
		}
		closeIdx := findElementClose(html, gt+1, closeTag)
		if closeIdx < 0 {
			break
		}
		if skip == nil || !skip(html[open+len(openTag):gt]) {
			out = append(out, cspHash(html[gt+1:closeIdx]))
		}
		i = closeIdx + len(closeTag)
	}
	return out
}

// cspHash returns the CSP source token 'sha256-<std-base64>' for content.
func cspHash(content []byte) string {
	sum := sha256.Sum256(content)
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// findElementOpen returns the index at or after `from` of the next occurrence of
// `openTag` (e.g. "<script", "<style") — case-insensitive, and only where the tag
// name is followed by a tag boundary so "<scriptfoo" or "<styles" does not
// match — or -1.
func findElementOpen(html []byte, from int, openTag string) int {
	for i := from; ; {
		s := indexFoldASCII(html, i, openTag)
		if s < 0 {
			return -1
		}
		after := s + len(openTag)
		if after >= len(html) || isTagNameBoundary(html[after]) {
			return s
		}
		i = after
	}
}

// findElementClose returns the index at or after `from` of the end tag that
// closes the element — case-insensitive, and only where the tag name is followed
// by a tag boundary, so "</scriptfoo>" does not end a <script> — or -1. It is the
// close-side mirror of findElementOpen, and the boundary requirement is what
// keeps the hashed span the span a browser hashes: the tokenizer leaves script or
// style data only on "</name" followed by whitespace, '/' or '>', and at
// end-of-input it does not leave it at all (those bytes are content). Ending the
// span early would emit a token for content no browser ever hashes, so the policy
// built from it would block the page it was computed from.
func findElementClose(html []byte, from int, closeTag string) int {
	for i := from; ; {
		s := indexFoldASCII(html, i, closeTag)
		if s < 0 {
			return -1
		}
		after := s + len(closeTag)
		if after < len(html) && isTagNameBoundary(html[after]) {
			return s
		}
		i = after
	}
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
