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

// WithStaticCacheControl sets the per-asset Cache-Control policy, "no-cache"
// for every asset by default. fn receives the asset path with no leading
// slash, "index.html" for "/", and returns the header value; an empty value
// omits the header. It chooses the value only: ETags, gzip and revalidation
// are fixed. A nil fn keeps the default.
func WithStaticCacheControl(fn func(assetPath string) string) StaticOption {
	return func(c *staticConfig) {
		if fn != nil {
			c.cacheControl = fn
		}
	}
}

// StaticHandler serves fsys, typically an embed.FS, whose zero ModTime leaves
// a bare http.FileServer nothing to revalidate on. It reads the tree once and
// gives each file a sha256 ETag and, when smaller, a gzip body. A known asset
// carries its ETag, Cache-Control and Vary: Accept-Encoding; a GET or HEAD
// offering gzip without Range gets the gzip body under its own "-gz" ETag.
// Everything else falls through to http.FileServer. The error is non-nil only
// when walking fsys fails.
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
	if ifNoneMatch(r.Header, gzEtag) {
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

// ifNoneMatch reports whether h's If-None-Match, over every field line, is
// "*" as the whole field or lists an entity-tag weakly equal to etag, a
// strong entity-tag (RFC 9110 section 13.1.2).
func ifNoneMatch(h http.Header, etag string) bool {
	members, wildcard := 0, false
	for member := range fieldList(h, "If-None-Match", entityTagList) {
		members++
		wildcard = member == "*"
		if strings.TrimPrefix(member, "W/") == etag {
			return true
		}
	}
	return wildcard && members == 1
}
