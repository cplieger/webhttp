package webhttp

// This file holds the package's ASCII case-folding primitives. They are
// deliberately ASCII-only: every string this package folds is a protocol token
// whose grammar is ASCII by definition, and folding wider than the grammar can
// only admit input the grammar already excludes.

// equalASCIIFold reports whether s and t are equal under ASCII-only case
// folding: bytes 'A'-'Z' fold to 'a'-'z' and every other byte must match
// exactly.
//
// It exists because strings.EqualFold applies Unicode simple folding, which is
// the wrong relation for an ASCII token compared against a fixed literal.
// Under Unicode folding "localhoſt" (U+017F LATIN SMALL LETTER LONG S) equals
// "localhost", so a bind classifier built on EqualFold reports an address as
// loopback that no resolver maps to loopback — a safety verdict strictly more
// permissive than the thing it models. The two grammars folded here are ASCII
// by definition: "localhost" per RFC 6761 §6.3, and a content-coding token per
// RFC 9110 §8.4.1.
//
// Byte length is a sound early exit precisely because the comparison is
// bytewise: folding never changes a byte's width, so differing lengths cannot
// fold equal.
func equalASCIIFold(s, t string) bool {
	if len(s) != len(t) {
		return false
	}
	for i := range len(s) {
		if lowerASCII(s[i]) != lowerASCII(t[i]) {
			return false
		}
	}
	return true
}

// lowerASCII returns c lowercased if it is an ASCII uppercase letter, else c.
// Bytes above 0x7F are returned unchanged, which is what keeps the comparisons
// built on it byte-exact outside ASCII.
func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}
