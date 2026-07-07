package webhttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// HeaderRequestID is the canonical request-id header. RequestLogger reads it
// from the inbound request and echoes it on the response.
const HeaderRequestID = "X-Request-ID"

// requestIDMaxLen bounds an accepted inbound request id.
const requestIDMaxLen = 64

// ValidRequestID reports whether s is a well-formed request id: between 1 and
// 64 characters, each an ASCII letter, digit, underscore, or hyphen. Anything
// else (empty, too long, or containing another byte) is rejected, so a client
// cannot smuggle log-forging newlines or header-splitting content through the
// echoed id.
func ValidRequestID(s string) bool {
	if s == "" || len(s) > requestIDMaxLen {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '_',
			c == '-':
		default:
			return false
		}
	}
	return true
}

// NewRequestID returns a fresh request id: 16 cryptographically random bytes,
// hex-encoded to 32 characters. If the system random source fails, it falls
// back to a UTC timestamp in the "20060102T150405" layout, which contains no
// dot and stays within the ValidRequestID character set.
func NewRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UTC().Format("20060102T150405")
	}
	return hex.EncodeToString(b[:])
}

// requestIDKey is the private context key under which the request id is stored.
type requestIDKey struct{}

// WithRequestID returns a copy of ctx carrying the request id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFromContext returns the request id stored in ctx by WithRequestID,
// or "" if none is present.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// logConfig holds resolved RequestLogger configuration.
type logConfig struct {
	logger          *slog.Logger
	skipPaths       map[string]struct{}
	skipFunc        func(*http.Request) bool
	recordMetric    func(method, path string, status int, d time.Duration)
	clientIPFunc    func(*http.Request) string
	clientIPTrusted []*net.IPNet
	logClientIP     bool
}

// LogOption configures RequestLogger.
type LogOption func(*logConfig)

// WithLogger sets the slog.Logger used for access-log lines. Defaults to
// slog.Default().
func WithLogger(l *slog.Logger) LogOption {
	return func(c *logConfig) { c.logger = l }
}

// WithSkipPaths marks exact request paths (compared against r.URL.Path) that
// should pass through WITHOUT an access-log line AND without a metric hook. Use
// it for long-lived streams (SSE, WebSocket) whose single open-forever request
// would otherwise emit one misleading high-latency line and a synthetic status
// at close. The request id is still minted, echoed, and threaded into the
// context for skipped paths. Because the match is exact, streaming routes with
// path parameters (e.g. "/ws/{id}") need WithSkipFunc instead.
func WithSkipPaths(paths ...string) LogOption {
	return func(c *logConfig) {
		if c.skipPaths == nil {
			c.skipPaths = make(map[string]struct{}, len(paths))
		}
		for _, p := range paths {
			c.skipPaths[p] = struct{}{}
		}
	}
}

// WithSkipFunc registers a predicate; when it returns true for a request, that
// request is passed through WITHOUT an access-log line or metric (like a
// WithSkipPaths match), while the request id is still minted, echoed, and
// threaded. Use it for streaming routes with path parameters (e.g. "/ws/{id}")
// that an exact WithSkipPaths match cannot cover.
func WithSkipFunc(fn func(*http.Request) bool) LogOption {
	return func(c *logConfig) { c.skipFunc = fn }
}

// WithRecordMetric registers a hook invoked once per logged request with the
// final method, path, status, and latency. It fires from a deferred call, so a
// panicking handler is still recorded. Requests skipped via WithSkipPaths or
// WithSkipFunc are excluded from the hook as well as from access logging: a
// stream's open-to-close duration paired with a synthetic status is misleading,
// which is the whole reason the path is skipped.
func WithRecordMetric(fn func(method, path string, status int, d time.Duration)) LogOption {
	return func(c *logConfig) { c.recordMetric = fn }
}

// WithClientIP adds a "client_ip" attribute to the access-log line, set to the
// best-effort client IP resolved by ClientIP with the given trusted proxy
// ranges. With no trusted ranges the immediate socket peer is logged (the
// spoof-proof default); pass the reverse-proxy CIDRs to resolve the real client
// from a trusted X-Forwarded-For, exactly as ClientIP does. The attribute is
// omitted entirely unless this option is supplied, so the default access line
// is unchanged. It is resolved once per request, inside the deferred access
// log, so it costs nothing on skipped (streaming) paths.
func WithClientIP(trusted ...*net.IPNet) LogOption {
	return func(c *logConfig) {
		c.logClientIP = true
		c.clientIPTrusted = trusted
		c.clientIPFunc = nil
	}
}

// WithClientIPFunc is like WithClientIP but resolves the "client_ip" attribute
// with a caller-supplied function instead of a fixed trusted-proxy set. Use it
// when the trusted set is not known at construction — e.g. it is reloaded from
// config at runtime behind a hot-reloadable resolver — or when client-IP
// resolution is otherwise app-specific: fn is called once per logged request
// (never on a skipped path), and its result is logged verbatim as "client_ip".
// It composes with WithRecordMetric. WithClientIP and WithClientIPFunc both
// enable the attribute and are mutually exclusive; whichever is applied last
// wins.
func WithClientIPFunc(fn func(*http.Request) string) LogOption {
	return func(c *logConfig) {
		c.logClientIP = true
		c.clientIPFunc = fn
		c.clientIPTrusted = nil
	}
}

// resolveClientIP returns the value logged as "client_ip": the caller's resolver
// when WithClientIPFunc was supplied, otherwise the spoof-proof ClientIP over the
// fixed trusted-proxy set.
func (c *logConfig) resolveClientIP(r *http.Request) string {
	if c.clientIPFunc != nil {
		return c.clientIPFunc(r)
	}
	return ClientIP(r, c.clientIPTrusted...)
}

// emitAccessLog writes the single access-log line and fires the optional metric
// hook. RequestLogger defers it, so a panicking handler is still logged with its
// recorded status (rec is read when the deferred call runs).
func (c *logConfig) emitAccessLog(rec *StatusRecorder, r *http.Request, path, id string, start time.Time) {
	d := time.Since(start)
	args := []any{
		"method", r.Method,
		"path", path,
		"status", rec.Status(),
		"duration_ms", d.Milliseconds(),
		"request_id", id,
	}
	if c.logClientIP {
		args = append(args, "client_ip", c.resolveClientIP(r))
	}
	c.logger.Info("http", args...)
	if c.recordMetric != nil {
		c.recordMetric(r.Method, path, rec.Status(), d)
	}
}

// RequestLogger returns middleware that gives each request a request id, echoes
// it on the response HeaderRequestID header, threads it through the request
// context (see RequestIDFromContext), and emits one access-log line at Info
// after next returns:
//
//	logger.Info("http", "method", …, "path", …, "status", …,
//		"duration_ms", …, "request_id", …)
//
// With WithClientIP the line additionally carries a "client_ip" attribute
// resolved by ClientIP (spoof-proof, honoring only the trusted proxy ranges
// passed to the option); WithClientIPFunc is the variant that resolves it with
// a caller-supplied function, for a dynamic (e.g. config-reloaded) trusted set.
//
// It records the status via a StatusRecorder that stays transparent to
// http.ResponseController, so wrapped handlers can still Flush and Hijack. An
// inbound HeaderRequestID is reused when it satisfies ValidRequestID; otherwise
// a new id is minted with NewRequestID.
//
// A request matched by WithSkipPaths or WithSkipFunc still gets an id minted,
// echoed, and threaded, but is served through the raw writer with no recorder,
// no access-log line, and no metric hook.
//
// The access-log line and metric hook are emitted from a deferred call, so a
// handler that panics is still logged (the status shows the recorded value)
// before the panic continues up the stack to net/http.
func RequestLogger(next http.Handler, opts ...LogOption) http.Handler {
	c := &logConfig{}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		id := r.Header.Get(HeaderRequestID)
		if !ValidRequestID(id) {
			id = NewRequestID()
		}
		w.Header().Set(HeaderRequestID, id)
		r = r.WithContext(WithRequestID(r.Context(), id))

		path := r.URL.Path

		_, skipPath := c.skipPaths[path]
		if skipPath || (c.skipFunc != nil && c.skipFunc(r)) {
			next.ServeHTTP(w, r)
			return
		}

		rec := NewStatusRecorder(w)
		defer c.emitAccessLog(rec, r, path, id, start)
		next.ServeHTTP(rec, r)
	})
}
