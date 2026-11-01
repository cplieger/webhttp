package webhttp

import (
	"net/http"
	"slices"
	"testing"
)

func TestFieldList(t *testing.T) {
	cases := []struct {
		name   string
		lines  []string
		syntax listSyntax
		want   []string
	}{
		{"no field", nil, tokenList, nil},
		{"empty elements and OWS dropped", []string{" a ,, \tb\t, "}, tokenList, []string{"a", "b"}},
		{"field lines joined in order", []string{"a, b", "c"}, tokenList, []string{"a", "b", "c"}},
		{"an empty line between lines", []string{"a", "", "b"}, tokenList, []string{"a", "b"}},
		{"a token list splits at a comma inside quotes", []string{`"a,b", c`}, tokenList, []string{`"a`, `b"`, "c"}},
		{"a stray quote in a token list swallows nothing", []string{`"1.2.3.4`, "5.6.7.8"}, tokenList, []string{`"1.2.3.4`, "5.6.7.8"}},
		{"a quoted-string keeps its comma", []string{`for="a,b";by=c, for=d`}, quotedStringList, []string{`for="a,b";by=c`, "for=d"}},
		{"a quoted-pair escapes the quote", []string{`x="a\",b", y`}, quotedStringList, []string{`x="a\",b"`, "y"}},
		{"an escaped backslash ends with the quote", []string{`x="a\\", y`}, quotedStringList, []string{`x="a\\"`, "y"}},
		{"an unclosed quoted-string runs to the end", []string{`x="a, b`, "c"}, quotedStringList, []string{`x="a, b,c`}},
		{"an opaque-tag keeps its comma and star", []string{`"other,*,tag", W/"a,*"`}, entityTagList, []string{`"other,*,tag"`, `W/"a,*"`}},
		{"a backslash in an opaque-tag escapes nothing", []string{`"a\", "b"`}, entityTagList, []string{`"a\"`, `"b"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for _, line := range tc.lines {
				h.Add("X-List", line)
			}
			if got := slices.Collect(fieldList(h, "X-List", tc.syntax)); !slices.Equal(got, tc.want) {
				t.Errorf("fieldList(%q) = %q, want %q", tc.lines, got, tc.want)
			}
		})
	}

	t.Run("a caller stops the scan", func(t *testing.T) {
		h := http.Header{"X-List": {"a, b, c"}}
		var got []string
		for elem := range fieldList(h, "X-List", tokenList) {
			got = append(got, elem)
			if elem == "b" {
				break
			}
		}
		if !slices.Equal(got, []string{"a", "b"}) {
			t.Errorf("elements read before the break = %q, want [a b]", got)
		}
	})
}
