package webhttp_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/webhttp/v4"
)

// gzipChunked answers body as JSON, written in the chunk sizes cuts names
// (cycled, 1 to 256 bytes each), to a GET offering the given lines.
func gzipChunked(mw webhttp.Middleware, body, cuts []byte, accept []string) *httptest.ResponseRecorder {
	h := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for i, rest := 0, body; len(rest) > 0; i++ {
			n := len(rest)
			if len(cuts) > 0 {
				n = min(int(cuts[i%len(cuts)])+1, len(rest))
			}
			_, _ = w.Write(rest[:n])
			rest = rest[n:]
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, line := range accept {
		req.Header.Add("Accept-Encoding", line)
	}
	rec := httptest.NewRecorder()
	mw(http.HandlerFunc(h)).ServeHTTP(rec, req)
	return rec
}

// FuzzGzip_sameResponseAnyChunkingAndLineSplit holds that the client decodes
// exactly the body written, that neither the handler's write sizes nor the
// split of Accept-Encoding into field lines changes the encoding chosen, that
// only a body of at least the threshold is compressed, and that every
// response, each a candidate, carries Vary: Accept-Encoding once.
func FuzzGzip_sameResponseAnyChunkingAndLineSplit(f *testing.F) {
	f.Add([]byte(`{"a":1}`), []byte{3}, uint16(4), "gzip")
	f.Add(bytes.Repeat([]byte("x"), 3000), []byte{1, 200, 7}, uint16(1024), "br, gzip;q=0.5")
	f.Add(bytes.Repeat([]byte("y"), 2048), []byte{255}, uint16(1024), "gzip;q=0, *")
	f.Add(bytes.Repeat([]byte("y"), 2048), []byte{0}, uint16(1024), "*, gzip;q=0")
	f.Add([]byte("z"), []byte{}, uint16(0), "*;q=0, x-gzip")
	f.Add(bytes.Repeat([]byte("w"), 1100), []byte{50}, uint16(1024), " , ,GZIP;Q=0.000")
	f.Add(bytes.Repeat([]byte("v"), 1024), []byte{9, 9}, uint16(1024), "gzip;fooq=0")
	f.Add([]byte{}, []byte{}, uint16(0), "gzip")
	f.Fuzz(func(t *testing.T, body, cuts []byte, minBytes uint16, accept string) {
		mw := webhttp.Gzip(webhttp.WithGzipMinBytes(int(minBytes)))
		whole := gzipChunked(mw, body, nil, []string{accept})
		split := gzipChunked(mw, body, cuts, strings.Split(accept, ","))
		ce := whole.Header().Get("Content-Encoding")
		if got := split.Header().Get("Content-Encoding"); got != ce {
			t.Fatalf("Content-Encoding %q in one write on one line, %q in chunks over lines", ce, got)
		}
		if ce == "gzip" && len(body) < max(int(minBytes), 1) {
			t.Fatalf("a %d-byte body was compressed under a %d-byte threshold", len(body), minBytes)
		}
		for _, rec := range []*httptest.ResponseRecorder{whole, split} {
			if vary := rec.Header().Values("Vary"); !slices.Equal(vary, []string{"Accept-Encoding"}) {
				t.Fatalf("Vary = %q, want [Accept-Encoding]", vary)
			}
			got := rec.Body.Bytes()
			switch ce {
			case "gzip":
				got = gunzip(t, got)
			case "":
			default:
				t.Fatalf("Content-Encoding = %q, want gzip or none", ce)
			}
			if !bytes.Equal(got, body) {
				t.Fatalf("the client decodes %d bytes, want the %d written", len(got), len(body))
			}
		}
	})
}
