package webhttp

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"maps"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
)

// defaultGzipMinBytes is where gzip starts to pay: under about 1 KiB its
// 18 bytes of framing and the compressor's setup outweigh the saving.
const defaultGzipMinBytes = 1024

// gzipWriters recycles compressors: a flate writer allocates large hash
// tables, so building one per response would dominate small responses.
var gzipWriters sync.Pool

// GzipOption configures Gzip.
type GzipOption func(*gzipConfig)

type gzipConfig struct {
	skip     func(*http.Request) bool
	types    []string
	minBytes int
}

// WithGzipMinBytes sets the smallest body Gzip compresses, 1024 bytes by
// default; a shorter body is sent as written. An n below 1 compresses every
// non-empty body.
func WithGzipMinBytes(n int) GzipOption {
	return func(c *gzipConfig) { c.minBytes = max(n, 1) }
}

// WithGzipSkip makes Gzip hand a request for which skip reports true to the
// next handler unwrapped, with no Vary added: for a route whose writes must
// reach the client as they happen, such as an event stream or a WebSocket. A
// nil skip is ignored.
func WithGzipSkip(skip func(*http.Request) bool) GzipOption {
	return func(c *gzipConfig) {
		if skip != nil {
			c.skip = skip
		}
	}
}

// WithGzipTypes sets the media types Gzip compresses, replacing the default
// "application/json" and "+json". An entry is a type/subtype, matched against
// the Content-Type after mime.ParseMediaType (case-folded, parameters
// dropped), or a "+suffix", matched against the subtype's structured suffix
// after its last "+" (RFC 6839). A Content-Type that fails to parse is never
// compressed. With no entries nothing is compressed. It panics on an entry of
// neither form, or one holding a wildcard.
func WithGzipTypes(types ...string) GzipOption {
	set := make([]string, 0, len(types))
	for _, t := range types {
		norm, ok := gzipTypeEntry(t)
		if !ok {
			panic(fmt.Sprintf("webhttp: WithGzipTypes entry %q is neither a type/subtype nor a +suffix", t))
		}
		set = append(set, norm)
	}
	return func(c *gzipConfig) { c.types = set }
}

func gzipTypeEntry(entry string) (string, bool) {
	e := lowerASCIIString(entry)
	if strings.Contains(e, "*") {
		return "", false
	}
	if suffix, ok := strings.CutPrefix(e, "+"); ok {
		return e, isToken(suffix) && !strings.Contains(suffix, "+")
	}
	typ, sub, ok := strings.Cut(e, "/")
	return e, ok && isToken(typ) && isToken(sub)
}

func isToken(s string) bool {
	return s != "" && tokenLen(s) == len(s)
}

func (c *gzipConfig) compressible(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	_, sub, _ := strings.Cut(mt, "/")
	suffix := ""
	if i := strings.LastIndexByte(sub, '+'); i >= 0 {
		suffix = sub[i:]
	}
	for _, t := range c.types {
		if t == mt || t == suffix {
			return true
		}
	}
	return false
}

// candidate reports whether Accept-Encoding chooses a final response's
// coding, so it carries Vary: a 2xx to 5xx status other than 204, no
// Content-Encoding and a listed Content-Type. A 206 or 304 counts, since it
// carries the Vary a 200 would (RFC 9110 sections 15.3.7, 15.4.5).
func (c *gzipConfig) candidate(status int, h http.Header) bool {
	return status >= http.StatusOK && status <= 599 && status != http.StatusNoContent &&
		h.Get("Content-Encoding") == "" && c.compressible(h.Get("Content-Type"))
}

// compressesStatus reports whether a candidate with this status may be
// compressed: a 304 has no body, and a 206 is a byte range of the identity
// representation, which re-encoding would corrupt.
func compressesStatus(status int) bool {
	return status != http.StatusPartialContent && status != http.StatusNotModified
}

// Gzip returns middleware that gzips a response when the request offers gzip
// and the response has no Content-Encoding, a WithGzipTypes Content-Type, a
// status other than 1xx, 204, 206 or 304, and WithGzipMinBytes of body. Every
// response of a listed type with no Content-Encoding and a final status other
// than 204 carries Vary: Accept-Encoding, whatever the outcome; a compressed
// one drops Content-Length and Accept-Ranges and weakens its ETag. Until the
// size is reached the status and body are held back, and a Flush or the
// handler's return sends them as written. Place it inside Recoverer, so a
// panic before that point still answers 500.
func Gzip(opts ...GzipOption) Middleware {
	c := &gzipConfig{minBytes: defaultGzipMinBytes, types: []string{"application/json", "+json"}}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.skip != nil && c.skip(r) {
				next.ServeHTTP(w, r)
				return
			}
			// A HEAD response has no body to compress, so it stays identity.
			g := &gzipWriter{rw: w, cfg: c, accepts: r.Method != http.MethodHead && acceptsGzip(r)}
			returned := false
			defer func() {
				if !returned {
					g.abort()
				}
			}()
			next.ServeHTTP(g, r)
			returned = true
			g.finish()
		})
	}
}

// acceptsGzip reports whether r's Accept-Encoding, an RFC 9110 section 5.6.1
// list over every field line, offers gzip or its alias x-gzip (section
// 8.4.1.3): named with a non-zero weight, or matched by "*" when not named
// (section 12.5.3). An unparseable weight counts as an offer, since the
// client named the coding.
func acceptsGzip(r *http.Request) bool {
	wildcard := false
	for elem := range fieldList(r.Header, "Accept-Encoding", tokenList) {
		coding, offered := encodingOffer(elem)
		switch {
		case equalASCIIFold(coding, "gzip"), equalASCIIFold(coding, "x-gzip"):
			return offered
		case coding == "*":
			wildcard = offered
		}
	}
	return wildcard
}

// encodingOffer splits one Accept-Encoding element into its coding and
// whether it is offered: false only for a well-formed zero weight.
func encodingOffer(elem string) (coding string, offered bool) {
	coding, params, _ := strings.Cut(elem, ";")
	coding = strings.Trim(coding, " \t")
	for param := range strings.SplitSeq(params, ";") {
		name, value, _ := strings.Cut(param, "=")
		if equalASCIIFold(strings.Trim(name, " \t"), "q") {
			return coding, !zeroQValue(strings.Trim(value, " \t"))
		}
	}
	return coding, true
}

// zeroQValue reports whether v is a zero qvalue, RFC 9110 section 12.4.2:
// "0", optionally followed by "." and at most three zeros.
func zeroQValue(v string) bool {
	frac, ok := strings.CutPrefix(v, "0")
	if !ok {
		return false
	}
	if frac == "" {
		return true
	}
	frac, ok = strings.CutPrefix(frac, ".")
	return ok && len(frac) <= 3 && strings.Trim(frac, "0") == ""
}

// addVaryAcceptEncoding adds Accept-Encoding to Vary unless it is listed or
// Vary is "*", which already varies on every field (RFC 9110 section 12.5.5).
func addVaryAcceptEncoding(h http.Header) {
	for elem := range fieldList(h, "Vary", tokenList) {
		if elem == "*" || equalASCIIFold(elem, "Accept-Encoding") {
			return
		}
	}
	h.Add("Vary", "Accept-Encoding")
}

type gzipState uint8

const (
	gzipPending   gzipState = iota // no final status yet
	gzipBuffering                  // compressible but under minBytes, held back
	gzipIdentity
	gzipCompressed
	gzipHijacked
)

var (
	_ http.Flusher  = (*gzipWriter)(nil)
	_ http.Hijacker = (*gzipWriter)(nil)
	// http.ResponseController finds Unwrap through an anonymous interface,
	// so nothing else fails to compile without it; SetWriteDeadline and
	// EnableFullDuplex would then answer ErrNotSupported under Gzip.
	_ interface{ Unwrap() http.ResponseWriter } = (*gzipWriter)(nil)
)

// gzipWriter is not safe for concurrent use, like the writer it wraps.
type gzipWriter struct {
	rw  http.ResponseWriter
	cfg *gzipConfig
	zw  *gzip.Writer
	// held is the header as it stood at the held-back final WriteHeader,
	// the one the commit sends.
	held    http.Header
	buf     []byte
	status  int
	accepts bool
	state   gzipState
}

func (g *gzipWriter) Header() http.Header { return g.rw.Header() }

// WriteHeader decides at once for a response that cannot be compressed, so
// a streaming handler keeps an unbuffered writer; a compressible response's
// status waits for its size. A later final status is ignored.
func (g *gzipWriter) WriteHeader(code int) {
	if g.state != gzipPending {
		return
	}
	if code >= 100 && code <= 199 && code != http.StatusSwitchingProtocols {
		g.rw.WriteHeader(code)
		return
	}
	h := g.rw.Header()
	candidate := g.cfg.candidate(code, h)
	if candidate {
		addVaryAcceptEncoding(h)
	}
	if !candidate || !g.accepts || !compressesStatus(code) {
		g.state = gzipIdentity
		g.rw.WriteHeader(code)
		return
	}
	g.state, g.status, g.held = gzipBuffering, code, h.Clone()
}

// implicitStatus sends the 200 net/http implies when a handler writes, flushes
// or returns before any final WriteHeader.
func (g *gzipWriter) implicitStatus() {
	if g.state == gzipPending {
		g.WriteHeader(http.StatusOK)
	}
}

func (g *gzipWriter) Write(p []byte) (int, error) {
	g.implicitStatus()
	switch g.state {
	case gzipBuffering:
		if len(g.buf)+len(p) < g.cfg.minBytes {
			g.buf = append(g.buf, p...)
			return len(p), nil
		}
		if err := g.commitCompressed(); err != nil {
			return 0, err
		}
		return g.zw.Write(p)
	case gzipCompressed:
		return g.zw.Write(p)
	case gzipHijacked:
		return 0, http.ErrHijacked
	default:
		return g.rw.Write(p)
	}
}

func (g *gzipWriter) commitCompressed() error {
	g.state = gzipCompressed
	g.sendHeld(true)
	zw, _ := gzipWriters.Get().(*gzip.Writer)
	if zw == nil {
		zw = gzip.NewWriter(g.rw)
	} else {
		zw.Reset(g.rw)
	}
	g.zw = zw
	_, err := zw.Write(g.buf)
	g.buf = nil
	return err
}

func (g *gzipWriter) commitIdentity() error {
	g.state = gzipIdentity
	g.sendHeld(false)
	if len(g.buf) == 0 {
		return nil
	}
	_, err := g.rw.Write(g.buf)
	g.buf = nil
	return err
}

// sendHeld writes the held status with the header held at WriteHeader. A
// declared trailer's later value goes back after WriteHeader, once net/http
// has snapshotted the header block, so it is sent as a trailer only.
func (g *gzipWriter) sendHeld(compressed bool) {
	h := g.rw.Header()
	trailers := restoreHeader(h, g.held)
	g.held = nil
	if compressed {
		markCompressed(h)
	}
	g.rw.WriteHeader(g.status)
	maps.Copy(g.rw.Header(), trailers)
}

// restoreHeader resets h to held, since http.ResponseWriter's contract makes
// a change after WriteHeader a no-op but a trailer's. A key under
// http.TrailerPrefix is never a header line, so it stays as set; the values
// h holds for the trailers held declares are returned.
func restoreHeader(h, held http.Header) (trailers http.Header) {
	for name := range fieldList(held, "Trailer", tokenList) {
		name = http.CanonicalHeaderKey(name)
		if v, ok := h[name]; ok {
			if trailers == nil {
				trailers = http.Header{}
			}
			trailers[name] = v
		}
	}
	for k := range h {
		if !strings.HasPrefix(k, http.TrailerPrefix) {
			delete(h, k)
		}
	}
	for k, v := range held {
		if !strings.HasPrefix(k, http.TrailerPrefix) {
			h[k] = v
		}
	}
	return trailers
}

func markCompressed(h http.Header) {
	h.Set("Content-Encoding", "gzip")
	// Both describe the identity body: its length, and byte ranges into it.
	h.Del("Content-Length")
	h.Del("Accept-Ranges")
	// A strong validator names one representation's bytes (RFC 9110 section
	// 8.8.1); the gzip body is a different sequence of them.
	if etag := h.Get("ETag"); etag != "" && !strings.HasPrefix(etag, "W/") {
		h.Set("ETag", "W/"+etag)
	}
}

// Flush sends a held-back body as written, since a flushing handler wants
// its bytes now, and flushes a compressed stream.
func (g *gzipWriter) Flush() { _ = g.FlushError() }

// FlushError is Flush returning the underlying writer's error, for
// http.ResponseController.
func (g *gzipWriter) FlushError() error {
	g.implicitStatus()
	switch g.state {
	case gzipBuffering:
		if err := g.commitIdentity(); err != nil {
			return err
		}
	case gzipCompressed:
		if err := g.zw.Flush(); err != nil {
			return err
		}
	case gzipHijacked:
		return http.ErrHijacked
	default:
	}
	return http.NewResponseController(g.rw).Flush()
}

// Hijack hands the connection over and drops anything held back; the
// response is the hijacker's from then on.
func (g *gzipWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := http.NewResponseController(g.rw).Hijack()
	if err == nil {
		g.buf = nil
		g.release()
		g.state = gzipHijacked
	}
	return conn, brw, err
}

// Unwrap lets http.ResponseController reach the underlying writer's
// deadlines; Flush and Hijack stop here so they cannot bypass the buffer.
func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.rw }

// finish settles a response whose handler returned. The errors are dropped:
// the handler is gone, and a failed write means the client is too.
func (g *gzipWriter) finish() {
	g.implicitStatus()
	switch g.state {
	case gzipBuffering:
		_ = g.commitIdentity()
	case gzipCompressed:
		_ = g.zw.Close()
		g.release()
	default:
	}
}

// abort runs while a handler panic unwinds. A held-back body is dropped, so
// Recoverer's 500 goes out on a clean wire; a compressed stream is left
// without its trailer, so the client sees the truncation.
func (g *gzipWriter) abort() {
	if g.state == gzipBuffering {
		// The handler's Content-Length describes the dropped body, and would
		// misframe the 500.
		g.rw.Header().Del("Content-Length")
	}
	g.buf = nil
	g.release()
}

func (g *gzipWriter) release() {
	if g.zw == nil {
		return
	}
	g.zw.Reset(io.Discard)
	gzipWriters.Put(g.zw)
	g.zw = nil
}
