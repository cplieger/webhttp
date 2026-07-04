package webhttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
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
	logger       *slog.Logger
	skipPaths    map[string]struct{}
	recordMetric func(method, path string, status int, d time.Duration)
}

// LogOption configures RequestLogger.
type LogOption func(*logConfig)

// WithLogger sets the slog.Logger used for access-log lines. Defaults to
// slog.Default().
func WithLogger(l *slog.Logger) LogOption {
	return func(c *logConfig) { c.logger = l }
}

// WithSkipPaths marks request paths that should pass through WITHOUT an
// access-log line. Use it for long-lived streams (SSE, WebSocket) whose single
// open-forever request would otherwise emit one misleading high-latency line at
// close. The request id is still minted, echoed, and threaded into the context
// for skipped paths.
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

// WithRecordMetric registers a hook invoked once per request with the final
// method, path, status, and latency. It fires for both logged and skipped
// paths, so a metrics pipeline stays complete even where access logging is
// suppressed. Because a skipped path is served through the raw writer (no
// status recorder), its status is reported as http.StatusOK.
func WithRecordMetric(fn func(method, path string, status int, d time.Duration)) LogOption {
	return func(c *logConfig) { c.recordMetric = fn }
}

// RequestLogger returns middleware that gives each request a request id, echoes
// it on the response HeaderRequestID header, threads it through the request
// context (see RequestIDFromContext), and emits one access-log line at Info
// after next returns:
//
//	logger.Info("http", "method", …, "path", …, "status", …,
//		"duration_ms", …, "request_id", …)
//
// It records the status via a StatusRecorder that stays transparent to
// http.ResponseController, so wrapped handlers can still Flush and Hijack. An
// inbound HeaderRequestID is reused when it satisfies ValidRequestID; otherwise
// a new id is minted with NewRequestID.
//
// Paths registered with WithSkipPaths still get an id minted, echoed, and
// threaded, but are served through the raw writer with no recorder and no log
// line. A WithRecordMetric hook, if set, still fires for them.
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

		if _, skip := c.skipPaths[path]; skip {
			next.ServeHTTP(w, r)
			if c.recordMetric != nil {
				c.recordMetric(r.Method, path, http.StatusOK, time.Since(start))
			}
			return
		}

		rec := NewStatusRecorder(w)
		next.ServeHTTP(rec, r)

		c.logger.Info("http",
			"method", r.Method,
			"path", path,
			"status", rec.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", id,
		)
		if c.recordMetric != nil {
			c.recordMetric(r.Method, path, rec.Status(), time.Since(start))
		}
	})
}
