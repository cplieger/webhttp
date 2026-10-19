package webhttp

import (
	"iter"
	"net/http"
	"strings"
)

// listSyntax is how a list-valued field quotes its elements, which decides
// whether a comma separates them (RFC 9110 section 5.6.1).
type listSyntax uint8

const (
	// tokenList has no quoting: a '"' is an ordinary byte, so a client's
	// stray quote cannot swallow the elements after it.
	tokenList listSyntax = iota
	// quotedStringList holds RFC 9110 section 5.6.4 quoted-strings, with
	// quoted-pair escapes.
	quotedStringList
	// entityTagList holds RFC 9110 section 8.8.3 opaque-tags: quoted, with
	// no escape, so a '\' before the closing quote is part of the tag.
	entityTagList
)

// fieldList yields h's name field as one RFC 9110 section 5.6.1 list: field
// lines joined with "," (section 5.2), so a proxy adding a line cannot leave
// a client's line read alone; split at each comma outside quotes; elements
// trimmed of OWS; empty elements dropped. A caller bounding its elements
// stops the loop; the scan up to there is linear in the field's length.
func fieldList(h http.Header, name string, syntax listSyntax) iter.Seq[string] {
	return func(yield func(string) bool) {
		field := strings.Join(h.Values(name), ",")
		start, inQuote := 0, false
		for i := 0; i < len(field); i++ {
			switch c := field[i]; {
			case syntax == quotedStringList && inQuote && c == '\\':
				i++
			case syntax != tokenList && c == '"':
				inQuote = !inQuote
			case c == ',' && !inQuote:
				if elem := strings.Trim(field[start:i], " \t"); elem != "" && !yield(elem) {
					return
				}
				start = i + 1
			}
		}
		if elem := strings.Trim(field[start:], " \t"); elem != "" {
			yield(elem)
		}
	}
}

// tokenLen is the length of the RFC 9110 section 5.6.2 token at the front of s.
func tokenLen(s string) int {
	for i := range len(s) {
		if !tokenByte(s[i]) {
			return i
		}
	}
	return len(s)
}

func tokenByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

func qdtextByte(c byte) bool {
	return c == '\t' || c == ' ' || c == 0x21 || c >= 0x23 && c <= 0x5b || c >= 0x5d && c <= 0x7e || c >= 0x80
}

func quotedPairByte(c byte) bool {
	return c == '\t' || c >= 0x20 && c != 0x7f
}
