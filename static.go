package webhttp

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// StaticOption configures StaticHandler.
type StaticOption func(*staticConfig)

// staticConfig holds resolved StaticHandler configuration.
type staticConfig struct {
	cacheControl func(assetPath string) string
}

// WithStaticCacheControl sets the per-asset Cache-Control policy. fn receives
// the normalized asset path — no leading slash, and "index.html" for the root
// ("/") request — and returns the Cache-Control value for that asset; an empty
// return omits the header. The default policy returns "no-cache" for every
// asset: revalidate on every load (the content-hash ETag makes that a cheap
// 304), the right choice when asset paths are stable rather than
// content-addressed. Supply a policy to differ per path, for example immutable
// large fonts alongside no-cache app code. A nil fn is ignored, keeping the
// default.
//
// The policy is MECHANISM-free: it chooses header values only. Whether an
// asset is ETagged, gzipped, or revalidated is the handler's fixed behavior.
func WithStaticCacheControl(fn func(assetPath string) string) StaticOption {
	return func(c *staticConfig) {
		if fn != nil {
			c.cacheControl = fn
		}
	}
}

// StaticHandler serves an embedded (or any fs.FS) static tree with the
// revalidation and compression plumbing embed.FS is missing.
//
// embed.FS reports a zero ModTime, so a bare http.FileServer emits neither
// Last-Modified nor ETag, leaving the browser no way to revalidate: every full
// page load re-downloads every asset. The bytes of an embedded tree are fixed
// for the process lifetime, so StaticHandler walks the tree ONCE at
// construction and precomputes, per file:
//
//   - a content-hash ETag (sha256), so http.ServeContent answers a matching
//     If-None-Match with 304 Not Modified;
//   - a gzip representation (best compression), kept only when it is actually
//     smaller — already-compressed formats (woff2, png) and tiny files stay
//     identity-only.
//
// Responses for known assets carry the ETag, a Cache-Control value from the
// cache policy (default "no-cache"; see WithStaticCacheControl), and
// Vary: Accept-Encoding (bodies vary by encoding, so shared caches must key on
// it on every path). A client that offers gzip on a non-Range GET/HEAD gets
// the precompressed body with Content-Encoding: gzip and a DISTINCT ETag
// (the identity tag suffixed -gz), including its own 304 handling; everything
// else falls through to the identity http.FileServer, which serves the
// index.html for "/" and handles Range, directories, and 404s.
//
// The error is non-nil only when walking fsys fails (an unreadable embedded
// tree is a malformed build; fail startup rather than serve a partial site).
// Serving is allocation-free per request apart from net/http's own work: all
// hashing and compression happened at construction.
func StaticHandler(fsys fs.FS, opts ...StaticOption) (http.Handler, error) {
	c := &staticConfig{
		cacheControl: func(string) string { return "no-cache" },
	}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	etags, gzipped, err := buildStaticMaps(fsys)
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		etag, known := etags[name]
		if known {
			h := w.Header()
			h.Set("ETag", etag)
			if cc := c.cacheControl(name); cc != "" {
				h.Set("Cache-Control", cc)
			}
			// Asset bodies vary by Accept-Encoding (some carry a gzip
			// representation), so shared caches must key on it on every path.
			h.Add("Vary", "Accept-Encoding")
		}
		if gz, ok := gzipped[name]; ok && serveGzip(w, r, etag, gz) {
			return
		}
		fileServer.ServeHTTP(w, r)
	}), nil
}

// gzAsset is a precomputed gzip representation of an embedded asset.
type gzAsset struct {
	contentType string
	body        []byte
}

// buildStaticMaps walks the static tree once, computing a content-hash ETag
// for every file and a gzip body for each asset that compresses smaller.
func buildStaticMaps(fsys fs.FS) (etags map[string]string, gzipped map[string]gzAsset, err error) {
	etags = make(map[string]string)
	gzipped = make(map[string]gzAsset)
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		b, readErr := fs.ReadFile(fsys, p)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(b)
		etags[p] = fmt.Sprintf(`"%x"`, sum[:])
		if gz, ok := gzipAsset(b, p); ok {
			gzipped[p] = gz
		}
		return nil
	})
	return etags, gzipped, err
}

// gzipAsset returns the gzip representation of b, or ok=false when gzip does
// not shrink it: already-compressed assets (woff2, which embeds Brotli) and
// small files whose gzip framing outweighs the savings. Plain otf/ttf outline
// fonts are not pre-compressed and do shrink (~30%), so they are stored and
// served gzipped.
func gzipAsset(b []byte, name string) (gzAsset, bool) {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression) // level is constant-valid
	if _, err := zw.Write(b); err != nil {
		return gzAsset{}, false
	}
	if err := zw.Close(); err != nil {
		return gzAsset{}, false
	}
	if buf.Len() >= len(b) {
		return gzAsset{}, false
	}
	ct := mime.TypeByExtension(path.Ext(name))
	if ct == "" {
		ct = http.DetectContentType(b)
	}
	return gzAsset{contentType: ct, body: bytes.Clone(buf.Bytes())}, true
}

// serveGzip writes the precompressed representation of an asset and reports
// true when it handled the response. It returns false — leaving the caller to
// fall back to the identity http.FileServer — for Range requests, non-GET/HEAD
// methods, or clients that do not offer gzip.
func serveGzip(w http.ResponseWriter, r *http.Request, etag string, gz gzAsset) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if r.Header.Get("Range") != "" || !acceptsGzip(r) {
		return false
	}
	h := w.Header()
	// A gzip body is a distinct representation, so it carries its own ETag;
	// Vary: Accept-Encoding (set by the caller) keeps caches from crossing it
	// with the identity body.
	gzEtag := `"` + strings.Trim(etag, `"`) + `-gz"`
	h.Set("ETag", gzEtag)
	if ifNoneMatchContains(r.Header.Get("If-None-Match"), gzEtag) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	h.Set("Content-Encoding", "gzip")
	h.Set("Content-Type", gz.contentType)
	h.Set("Content-Length", strconv.Itoa(len(gz.body)))
	if r.Method == http.MethodHead {
		return true
	}
	_, _ = w.Write(gz.body)
	return true
}

// acceptsGzip reports whether the request's Accept-Encoding header offers gzip
// with a non-zero quality value.
func acceptsGzip(r *http.Request) bool {
	for part := range strings.SplitSeq(r.Header.Get("Accept-Encoding"), ",") {
		token, qual := part, "1"
		if i := strings.IndexByte(part, ';'); i >= 0 {
			token = part[:i]
			if j := strings.Index(part[i:], "q="); j >= 0 {
				qual = part[i+j+2:]
			}
		}
		if !strings.EqualFold(strings.TrimSpace(token), "gzip") {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(qual), 64)
		return err != nil || q != 0
	}
	return false
}

// ifNoneMatchContains reports whether an If-None-Match header value matches the
// given ETag (or the "*" wildcard).
func ifNoneMatchContains(header, etag string) bool {
	for tok := range strings.SplitSeq(header, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "*" || tok == etag {
			return true
		}
	}
	return false
}
