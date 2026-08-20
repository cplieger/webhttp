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

// lowerASCIIString returns s with its ASCII uppercase letters lowercased and
// every other byte left alone. It is the string-shaped sibling of lowerASCII,
// for a canonicalizer that must RETURN the folded form rather than only compare
// it.
//
// It exists for the same reason equalASCIIFold does, in the direction that
// bites harder. strings.ToLower applies Unicode simple case mapping, and two
// already-assigned runes map into ASCII under it: U+0130 LATIN CAPITAL LETTER I
// WITH DOT ABOVE lowercases to "i" and U+212A KELVIN SIGN lowercases to "k".
// So a canonicalizer built on strings.ToLower turns a non-ASCII authority into
// an ASCII one, and an exact-match allowlist keyed on ASCII names then admits a
// wire value that is not any of them. Measured on go1.26.7 (Unicode 15) and
// go1.27.0 (Unicode 17): those two runes are the complete set that maps into
// the byte class CanonicalHost accepts, identically on both, so this is a
// property of Unicode case mapping rather than of one release.
//
// Note the laundering channels differ between the two stdlib helpers, which is
// why neither is safe here: U+017F and U+212A pass strings.EqualFold while
// U+0130 does not (Unicode FULL folding maps it to two runes and SimpleFold is
// 1:1), and U+0130 and U+212A pass strings.ToLower while U+017F does not. An
// ASCII fold is closed against all three.
//
// It allocates only when s actually carries an ASCII uppercase letter, so the
// common already-lowercase authority costs a scan and no copy.
func lowerASCIIString(s string) string {
	i := 0
	for ; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			break
		}
	}
	if i == len(s) {
		return s
	}
	b := []byte(s)
	for ; i < len(b); i++ {
		b[i] = lowerASCII(b[i])
	}
	return string(b)
}
