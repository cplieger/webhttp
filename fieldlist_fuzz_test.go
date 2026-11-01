package webhttp

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// listPieces are the fragments an element is built from: plain bytes, which
// never hold a comma, OWS or (outside tokenList) a quote or backslash, and
// the bytes a quoted run may hold, commas included.
var listPieces = map[listSyntax]struct{ plain, quoted []string }{
	tokenList:        {plain: []string{"a", "Z", ";", "=", "/", "*", "~", `"`, `\`}},
	quotedStringList: {plain: []string{"a", "Z", ";", "=", "/", "*", "~"}, quoted: []string{",", " ", "*", `\"`, `\\`, `\,`, "a"}},
	entityTagList:    {plain: []string{"a", "Z", ";", "=", "/", "*", "~", "W/"}, quoted: []string{",", "*", `\`, "a", "/"}},
}

func buildListElements(ops []byte, syntax listSyntax) []string {
	pieces := listPieces[syntax]
	var elems []string
	var cur strings.Builder
	end := func() {
		if e := strings.TrimLeft(cur.String(), " "); e != "" {
			elems = append(elems, e)
		}
		cur.Reset()
	}
	for _, b := range ops {
		arg := int(b >> 2)
		switch b & 3 {
		case 0:
			end()
		case 1:
			cur.WriteString(pieces.plain[arg%len(pieces.plain)])
		case 2:
			cur.WriteString(" " + pieces.plain[arg%len(pieces.plain)])
		default:
			if len(pieces.quoted) == 0 {
				cur.WriteString(pieces.plain[arg%len(pieces.plain)])
				continue
			}
			cur.WriteByte('"')
			for i := range arg % 6 {
				cur.WriteString(pieces.quoted[(arg+i)%len(pieces.quoted)])
			}
			cur.WriteByte('"')
		}
	}
	end()
	return elems
}

func joinListElements(elems []string, seps []byte) []string {
	separators := []string{",", ", ", " ,\t", ", ,", "\n"}
	var b strings.Builder
	for i, e := range elems {
		if i > 0 {
			sep := ","
			if len(seps) > 0 {
				sep = separators[int(seps[i%len(seps)])%len(separators)]
			}
			b.WriteString(sep)
		}
		b.WriteString(e)
	}
	return strings.Split(b.String(), "\n")
}

// FuzzFieldList_roundTrip holds that a list built from known elements, in
// each syntax, reads back as exactly those elements however the gaps between
// them are written and the lines are split.
func FuzzFieldList_roundTrip(f *testing.F) {
	f.Add([]byte{5, 0, 9, 0, 13}, []byte{1, 4, 3}, uint8(tokenList))
	f.Add([]byte{1, 3 | 3<<2, 0, 1}, []byte{0}, uint8(quotedStringList))
	f.Add([]byte{3 | 5<<2, 0, 3 | 9<<2, 1, 0, 2}, []byte{2, 4}, uint8(quotedStringList))
	f.Add([]byte{3 | 4<<2, 0, 3 | 2<<2}, []byte{1}, uint8(entityTagList))
	f.Add([]byte{29, 3 | 13<<2, 0, 3 | 7<<2}, []byte{3, 4}, uint8(entityTagList))
	f.Fuzz(func(t *testing.T, ops, seps []byte, syn uint8) {
		syntax := listSyntax(syn % 3)
		elems := buildListElements(ops, syntax)
		lines := joinListElements(elems, seps)
		h := http.Header{"X-List": lines}
		if got := slices.Collect(fieldList(h, "X-List", syntax)); !slices.Equal(got, elems) {
			t.Fatalf("lines %q read as %q, want %q", lines, got, elems)
		}
	})
}
