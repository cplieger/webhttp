package webhttp

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// errFailFS is the sentinel failure failFS returns from every Open.
var errFailFS = errors.New("static_test: fs open failed")

// staticTestFS builds the fixture tree StaticHandler tests serve: a small
// index.html, a highly compressible stylesheet, a tiny incompressible file,
// and a "font" under the path a cache policy typically special-cases.
func staticTestFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":            &fstest.MapFile{Data: []byte("<html><body>" + strings.Repeat("terminal ", 200) + "</body></html>")},
		"style.css":             &fstest.MapFile{Data: []byte(strings.Repeat("body{color:#b48eff}\n", 500))},
		"tiny.txt":              &fstest.MapFile{Data: []byte("x")},
		"vendor/fonts/mono.otf": &fstest.MapFile{Data: []byte(strings.Repeat("glyph outline data ", 400))},
	}
}

func TestStaticHandlerETagAndRevalidation(t *testing.T) {
	h, err := StaticHandler(staticTestFS())
	if err != nil {
		t.Fatalf("StaticHandler: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("static response has no ETag; the browser cannot revalidate an embedded asset and re-downloads it every load")
	}
	if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
		t.Errorf("ETag = %q, want a quoted opaque validator", etag)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want the %q default", cc, "no-cache")
	}
	if v := rec.Header().Get("Vary"); !strings.Contains(v, "Accept-Encoding") {
		t.Errorf("Vary = %q, want it to contain Accept-Encoding", v)
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("If-None-Match", etag)
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("conditional GET / with matching If-None-Match = %d, want 304", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 response body = %q, want empty", rec2.Body.String())
	}
}

func TestStaticHandlerGzipNegotiation(t *testing.T) {
	h, err := StaticHandler(staticTestFS())
	if err != nil {
		t.Fatalf("StaticHandler: %v", err)
	}

	t.Run("offering gzip yields a compressed body that decodes to the identity bytes", func(t *testing.T) {
		idRec := httptest.NewRecorder()
		h.ServeHTTP(idRec, httptest.NewRequest(http.MethodGet, "/style.css", nil))
		identity := bytes.Clone(idRec.Body.Bytes())

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/style.css", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /style.css (gzip) = %d, want 200", rec.Code)
		}
		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Errorf("Content-Encoding = %q, want %q", got, "gzip")
		}
		if etag := rec.Header().Get("ETag"); !strings.HasSuffix(etag, `-gz"`) {
			t.Errorf("gzip ETag = %q, want a distinct tag ending in -gz\"", etag)
		}
		zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatalf("response body is not valid gzip: %v", err)
		}
		decoded, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("read gzip body: %v", err)
		}
		if !bytes.Equal(decoded, identity) {
			t.Error("gzip response body did not decode to the identity (uncompressed) response bytes")
		}
	})

	t.Run("without Accept-Encoding the identity path serves uncompressed bytes", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/style.css", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /style.css = %d, want 200", rec.Code)
		}
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Content-Encoding = %q, want empty on the identity path", got)
		}
		if etag := rec.Header().Get("ETag"); strings.HasSuffix(etag, `-gz"`) {
			t.Errorf("identity ETag = %q, must not carry the -gz suffix", etag)
		}
	})

	t.Run("conditional gzip GET with the gz ETag yields 304", func(t *testing.T) {
		first := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/style.css", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		h.ServeHTTP(first, req)
		gzEtag := first.Header().Get("ETag")

		rec := httptest.NewRecorder()
		req2 := httptest.NewRequest(http.MethodGet, "/style.css", nil)
		req2.Header.Set("Accept-Encoding", "gzip")
		req2.Header.Set("If-None-Match", gzEtag)
		h.ServeHTTP(rec, req2)
		if rec.Code != http.StatusNotModified {
			t.Errorf("conditional gzip GET = %d, want 304", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("304 body len = %d, want 0", rec.Body.Len())
		}
	})

	t.Run("Range request falls back to the identity file server", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/style.css", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		req.Header.Set("Range", "bytes=0-9")
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Content-Encoding"); got == "gzip" {
			t.Error("Range request served the gzip representation; want identity fallback")
		}
	})

	t.Run("unknown path falls through to the file server 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/absent.js", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET /absent.js = %d, want 404", rec.Code)
		}
		if etag := rec.Header().Get("ETag"); etag != "" {
			t.Errorf("unknown path carries ETag %q, want none", etag)
		}
	})
}

// TestStaticHandlerCachePolicy pins the WithStaticCacheControl contract: the
// policy sees the normalized asset path ("index.html" for "/"), its return
// value becomes the header, and an empty return omits the header entirely.
func TestStaticHandlerCachePolicy(t *testing.T) {
	var sawRoot string
	h, err := StaticHandler(staticTestFS(), WithStaticCacheControl(func(p string) string {
		if p == "index.html" {
			sawRoot = p
		}
		switch {
		case strings.HasPrefix(p, "vendor/fonts/"):
			return "public, max-age=2592000, immutable"
		case p == "tiny.txt":
			return ""
		default:
			return "no-cache, must-revalidate"
		}
	}))
	if err != nil {
		t.Fatalf("StaticHandler: %v", err)
	}

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	if got := get("/vendor/fonts/mono.otf").Header().Get("Cache-Control"); got != "public, max-age=2592000, immutable" {
		t.Errorf("font Cache-Control = %q, want the immutable policy value", got)
	}
	if got := get("/style.css").Header().Get("Cache-Control"); got != "no-cache, must-revalidate" {
		t.Errorf("css Cache-Control = %q, want %q", got, "no-cache, must-revalidate")
	}
	rec := get("/tiny.txt")
	if got, ok := rec.Header()["Cache-Control"]; ok {
		t.Errorf("empty policy return still set Cache-Control %v, want the header omitted", got)
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("empty cache policy must not suppress the ETag; revalidation stays mechanism")
	}
	get("/")
	if sawRoot != "index.html" {
		t.Errorf(`policy saw %q for the root request, want "index.html"`, sawRoot)
	}
}

// TestStaticHandlerFontsGainETagAndGzip pins that mechanism is uniform: even
// an asset under an immutable cache policy carries the content-hash ETag and a
// gzip representation when it compresses smaller (plain otf outline data
// does).
func TestStaticHandlerFontsGainETagAndGzip(t *testing.T) {
	h, err := StaticHandler(staticTestFS(), WithStaticCacheControl(func(p string) string {
		if strings.HasPrefix(p, "vendor/fonts/") {
			return "public, max-age=2592000, immutable"
		}
		return "no-cache"
	}))
	if err != nil {
		t.Fatalf("StaticHandler: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/vendor/fonts/mono.otf", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rec, req)
	if rec.Header().Get("ETag") == "" {
		t.Error("font carries no ETag; post-TTL revalidation would re-download the full body")
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("font Content-Encoding = %q, want gzip (repetitive outline fixture compresses)", got)
	}
}

func TestStaticHandlerWalkErrorFailsLoud(t *testing.T) {
	// An fs.FS whose walk fails must abort construction: a partial asset map
	// would serve some files without validators.
	if _, err := StaticHandler(failFS{}); err == nil {
		t.Error("StaticHandler(failFS) = nil error, want the walk error surfaced")
	}
}

// failFS is an fs.FS whose Open always fails, forcing the construction walk to
// error.
type failFS struct{}

func (failFS) Open(string) (fs.File, error) { return nil, errFailFS }

func TestAcceptsGzip(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   bool
	}{
		{"plain gzip", "gzip", true},
		{"gzip with q=1", "gzip;q=1.0", true},
		{"gzip explicitly disabled q=0", "gzip;q=0", false},
		{"gzip disabled q=0.0", "gzip;q=0.0", false},
		{"gzip with fractional q", "gzip;q=0.5", true},
		{"gzip with space before params", "gzip ; q=0", false},
		{"second token offers gzip", "deflate, gzip", true},
		{"second token gzip disabled", "deflate, gzip;q=0", false},
		{"no gzip offered", "br, deflate", false},
		{"identity only", "identity", false},
		{"empty header", "", false},
		{"wildcard not treated as gzip", "*", false},
		{"malformed q is permissive", "gzip;q=bogus", true},
		{"q=0 with trailing parameter still refuses", "gzip;q=0;dummy=x", false},
		{"q=0.0 with trailing parameter still refuses", "gzip;q=0.0;dummy=x", false},
		{"non-zero q with trailing parameter accepts", "gzip;q=0.5;dummy=x", true},
		{"malformed q with trailing parameter is permissive", "gzip;q=bogus;dummy=x", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set("Accept-Encoding", tc.header)
			}
			if got := acceptsGzip(req); got != tc.want {
				t.Errorf("acceptsGzip(Accept-Encoding=%q) = %v, want %v", tc.header, got, tc.want)
			}
		})
	}
}

func TestIfNoneMatchContains(t *testing.T) {
	const etag = `"abc-gz"`
	cases := []struct {
		name   string
		header string
		want   bool
	}{
		{"empty header", "", false},
		{"exact match", `"abc-gz"`, true},
		{"wildcard", "*", true},
		{"present in comma list", `"x", "abc-gz", "y"`, true},
		{"absent from list", `"x", "y"`, false},
		{"whitespace trimmed around match", `  "abc-gz"  `, true},
		{"identity etag does not match gz etag", `"abc"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ifNoneMatchContains(tc.header, etag); got != tc.want {
				t.Errorf("ifNoneMatchContains(%q, %q) = %v, want %v", tc.header, etag, got, tc.want)
			}
		})
	}
}

func TestGzipAsset(t *testing.T) {
	t.Run("compressible asset round-trips and carries extension content type", func(t *testing.T) {
		raw := []byte(strings.Repeat("body{color:#b48eff}\n", 500))
		gz, ok := gzipAsset(raw, "style.css")
		if !ok {
			t.Fatal("gzipAsset() ok = false for a highly compressible asset, want true")
		}
		if len(gz.body) >= len(raw) {
			t.Errorf("gzip body len = %d, want < raw len %d", len(gz.body), len(raw))
		}
		zr, err := gzip.NewReader(bytes.NewReader(gz.body))
		if err != nil {
			t.Fatalf("gzip.NewReader: %v", err)
		}
		got, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("read gzip body: %v", err)
		}
		if !bytes.Equal(got, raw) {
			t.Error("gzip body did not round-trip to the original asset bytes")
		}
		if !strings.HasPrefix(gz.contentType, "text/css") {
			t.Errorf("contentType = %q, want it to start with %q", gz.contentType, "text/css")
		}
	})

	t.Run("incompressible tiny asset is not gzipped", func(t *testing.T) {
		if _, ok := gzipAsset([]byte("x"), "tiny.txt"); ok {
			t.Error("gzipAsset() ok = true for a 1-byte asset gzip cannot shrink, want false")
		}
	})
}

// TestGzipAssetContentTypeFallback pins the mime-miss branch: a compressible asset whose
// extension mime.TypeByExtension cannot resolve must fall back to http.DetectContentType.
func TestGzipAssetContentTypeFallback(t *testing.T) {
	raw := []byte(strings.Repeat("plain text body line\n", 500))
	gz, ok := gzipAsset(raw, "asset.unknownext")
	if !ok {
		t.Fatal("gzipAsset() ok = false for a highly compressible asset, want true")
	}
	if gz.contentType == "" {
		t.Fatal("contentType is empty; the mime-miss fallback did not run")
	}
	if !strings.HasPrefix(gz.contentType, "text/plain") {
		t.Errorf("contentType = %q, want text/plain prefix (DetectContentType on text bytes)", gz.contentType)
	}
}

func TestServeGzip(t *testing.T) {
	raw := []byte(strings.Repeat("hello world\n", 300))
	gz, ok := gzipAsset(raw, "x.txt")
	if !ok {
		t.Fatal("setup: gzipAsset() ok = false, want true")
	}
	const etag = `"deadbeef"`
	const gzEtag = `"deadbeef-gz"`

	t.Run("GET offering gzip serves the compressed body", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x.txt", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		if !serveGzip(rec, req, etag, gz) {
			t.Fatal("serveGzip() = false, want true (it should have handled the gzip response)")
		}
		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Errorf("Content-Encoding = %q, want %q", got, "gzip")
		}
		if got := rec.Header().Get("ETag"); got != gzEtag {
			t.Errorf("ETag = %q, want %q", got, gzEtag)
		}
		zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatalf("gzip.NewReader: %v", err)
		}
		got, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("read gzip body: %v", err)
		}
		if !bytes.Equal(got, raw) {
			t.Error("served gzip body did not decode to the original bytes")
		}
	})

	t.Run("HEAD offering gzip sets headers but writes no body", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodHead, "/x.txt", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		if !serveGzip(rec, req, etag, gz) {
			t.Fatal("serveGzip() = false, want true")
		}
		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Errorf("Content-Encoding = %q, want %q", got, "gzip")
		}
		if rec.Body.Len() != 0 {
			t.Errorf("HEAD body len = %d, want 0", rec.Body.Len())
		}
	})

	t.Run("conditional GET with matching gz ETag yields 304", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x.txt", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		req.Header.Set("If-None-Match", gzEtag)
		if !serveGzip(rec, req, etag, gz) {
			t.Fatal("serveGzip() = false, want true")
		}
		if rec.Code != http.StatusNotModified {
			t.Errorf("status = %d, want 304", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("304 body len = %d, want 0", rec.Body.Len())
		}
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("304 Content-Encoding = %q, want empty", got)
		}
	})

	t.Run("falls through (returns false) and writes nothing", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			method string
			accept string
			rangeH string
		}{
			{"non GET/HEAD method", http.MethodPost, "gzip", ""},
			{"client does not offer gzip", http.MethodGet, "identity", ""},
			{"gzip explicitly disabled", http.MethodGet, "gzip;q=0", ""},
			{"range request", http.MethodGet, "gzip", "bytes=0-10"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(tc.method, "/x.txt", nil)
				req.Header.Set("Accept-Encoding", tc.accept)
				if tc.rangeH != "" {
					req.Header.Set("Range", tc.rangeH)
				}
				if serveGzip(rec, req, etag, gz) {
					t.Errorf("serveGzip() = true, want false (should fall back to the identity file server)")
				}
				if got := rec.Header().Get("Content-Encoding"); got != "" {
					t.Errorf("Content-Encoding = %q, want empty on the fall-through path", got)
				}
			})
		}
	})
}
