package webhttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
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
// hex-encoded to 32 characters. crypto/rand.Read never returns an error (since
// Go 1.24 it crashes the program irrecoverably if the platform random source
// fails), so id generation cannot degrade.
func NewRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // never fails; crashes irrecoverably on entropy failure
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
	pathFunc        func(*http.Request) string
	recordMetric    func(method, path string, status int, d time.Duration)
	recordMetricReq func(r *http.Request, status int, d time.Duration)
	logLevel        func(r *http.Request, status int) slog.Level
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

// redactedPathFallback is the fail-closed placeholder recorded as the path
// when a WithPathFunc transform fails (panics or returns ""). The raw
// r.URL.Path is deliberately never the fallback: the transform exists because
// the raw path may embed a secret, so a broken transform must not silently
// reopen the leak it was installed to close.
const redactedPathFallback = "(path-redaction-failed)"

// WithPathFunc sets the PATH POLICY for the access-log line: fn is called once
// per logged request, at emit time, and its return value replaces r.URL.Path
// as the recorded path. Use it when a route embeds a credential or other
// sensitive segment (e.g. "/api/sessions/{token}") that should be logged as a
// token-free template or truncated form — the middle ground between logging
// the raw path and losing the whole access record to WithSkipPaths or
// WithSkipFunc.
//
// The returned value is "the path as recorded": it feeds the access line's
// "path" attribute, the legacy WithRecordMetric hook's path argument, and the
// "path" attribute of the package's hook-failure diagnostics. It does NOT
// feed WithRecordMetricRequest — that hook receives the *http.Request itself
// and owns its own representation (r.Pattern is the usual bounded-cardinality
// choice).
//
// fn runs inside the deferred emit, after routing, so http.ServeMux has
// already populated r.Pattern (empty when nothing matched) and a transform
// may return the matched template with its own fail-closed fallback for
// unmatched requests. The WithRecordMetricRequest caveat applies equally:
// middleware between RequestLogger and the mux that replaces the request
// (r.WithContext and friends return a clone) hides the populated fields.
//
// Skip predicates (WithSkipPaths, WithSkipFunc) always test the raw
// r.URL.Path, and a skipped request never calls fn.
//
// Fail-closed: if fn panics or returns the empty string, the line records the
// "(path-redaction-failed)" placeholder — never the raw path — and a panic is
// additionally logged through the package's hook-isolation guard, whose own
// diagnostic also omits the raw path. A nil fn is ignored (the skip-nil
// option convention).
func WithPathFunc(fn func(*http.Request) string) LogOption {
	return func(c *logConfig) {
		if fn != nil {
			c.pathFunc = fn
		}
	}
}

// WithRecordMetric registers a hook invoked once per logged request with the
// final method, path, status, and latency. It fires from a deferred call, so a
// panicking handler is still recorded. Requests skipped via WithSkipPaths or
// WithSkipFunc are excluded from the hook as well as from access logging: a
// stream's open-to-close duration paired with a synthetic status is misleading,
// which is the whole reason the path is skipped. WithRecordMetric and
// WithRecordMetricRequest (the request-aware variant) are mutually exclusive;
// whichever is applied last wins.
func WithRecordMetric(fn func(method, path string, status int, d time.Duration)) LogOption {
	return func(c *logConfig) {
		c.recordMetric = fn
		c.recordMetricReq = nil
	}
}

// WithRecordMetricRequest is the request-aware variant of WithRecordMetric:
// fn is invoked once per logged request with the *http.Request itself, the
// final status, and the latency. Because http.ServeMux assigns the matched
// pattern to the request in place, fn observes a populated r.Pattern after
// routing (empty when nothing matched, e.g. a 404), so a caller can key
// bounded-cardinality metrics on the route TEMPLATE rather than the raw URL
// path — the guard that keeps a scanner from minting unbounded label series.
// Caveat: middleware between RequestLogger and the mux that replaces the
// request (r.WithContext and friends return a clone) hides those fields — the
// mux populates the clone, not the request this hook received.
//
// Like WithRecordMetric it fires from a deferred call (a panicking handler is
// still recorded) and is excluded on paths skipped via WithSkipPaths or
// WithSkipFunc. The two options are mutually exclusive; whichever is applied
// last wins. A nil fn is ignored (the package's skip-nil option convention),
// so a trailing WithRecordMetricRequest(nil) neither enables the hook nor
// clears a prior WithRecordMetric.
func WithRecordMetricRequest(fn func(r *http.Request, status int, d time.Duration)) LogOption {
	return func(c *logConfig) {
		if fn == nil {
			return
		}
		c.recordMetricReq = fn
		c.recordMetric = nil
	}
}

// WithLogLevel sets the LEVEL POLICY for the access-log line: fn is called
// once per logged request with the request and the final status, and the line
// is emitted at the returned level. The default without this option is
// slog.LevelInfo, unchanged. The hook chooses the level only — the line's
// attributes and emission rules (deferred emit, skip paths) are the logger's
// fixed mechanism. ProbeLogLevel is the named preset over this hook for the
// routine-machine-probe case; reach for the raw hook only when a custom
// policy is genuinely needed.
//
// The canonical use is scrape-noise control on a polled service: map 2xx/3xx
// to slog.LevelDebug so a 15-second Prometheus scrape stays out of the log
// stream at the default level while staying visible under LOG_LEVEL=debug,
// and raise 4xx to Warn / 5xx to Error so failures surface. Because fn also
// receives the request, a policy can key on the path instead (quiet only the
// scrape route, keep everything else at Info).
//
// A request suppressed by WithSkipPaths or WithSkipFunc emits no line at all,
// so fn is never called for it. A panicking fn is contained: the failure is
// logged and the line falls back to Info, mirroring the package's other
// callback guards. A nil fn is ignored (the skip-nil option convention).
func WithLogLevel(fn func(r *http.Request, status int) slog.Level) LogOption {
	return func(c *logConfig) {
		if fn != nil {
			c.logLevel = fn
		}
	}
}

// ProbeLogLevel is the fleet-standard access-log level policy for routine
// machine-probe endpoints — health checks, readiness probes, metrics scrapes
// (Docker HEALTHCHECK curls, Gatus monitors, Prometheus). A request whose
// r.URL.Path exactly matches one of paths logs at Debug when it succeeds
// (status < 400), Warn on a 4xx, and Error on a 5xx; every other request
// stays at the default Info.
//
// The point: a probe hitting a HEALTHY endpoint every 30 seconds is noise and
// stays out of the shipped log stream (Debug is dropped below the operating
// level — but becomes visible the moment an operator raises the level to
// debug, when "is the probe even reaching me, from where" is the question),
// while a FAILING probe — the readiness 503, the broken-install signal — is
// exactly what an operator greps for and surfaces at Warn/Error with its
// status, duration, and request id. Prefer this preset over skipping probe
// paths entirely (a skip hides the failure too) and over leaving them at
// Info (a line every 30s per prober). Skip lists remain the right tool for
// STREAMS (SSE, WebSocket), where one open-to-close line is misleading by
// shape, not merely noisy.
//
// It is a WithLogLevel policy under the hood, so the two are mutually
// exclusive (last applied wins), it composes with the skip options (a
// skipped path emits no line and never consults the policy), and a
// panicking policy falls back to Info per the WithLogLevel contract. With
// no paths every request logs at Info, as without the option.
func ProbeLogLevel(paths ...string) LogOption {
	probe := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		probe[p] = struct{}{}
	}
	return WithLogLevel(func(r *http.Request, status int) slog.Level {
		if _, ok := probe[r.URL.Path]; !ok {
			return slog.LevelInfo
		}
		switch {
		case status >= 500:
			return slog.LevelError
		case status >= 400:
			return slog.LevelWarn
		default:
			return slog.LevelDebug
		}
	})
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
// wins. A nil fn is ignored (matching the package's skip-nil option
// convention), so a trailing WithClientIPFunc(nil) neither enables the
// attribute nor clears a prior WithClientIP.
func WithClientIPFunc(fn func(*http.Request) string) LogOption {
	return func(c *logConfig) {
		if fn == nil {
			return
		}
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
//
// Both caller-supplied observability callbacks — the WithPathFunc transform,
// the WithClientIPFunc resolver, the WithLogLevel policy, and the
// WithRecordMetric hook — run through recover guards. This defer sits in
// the outer Logging layer, OUTSIDE Recoverer (Logging is outermost so it can log
// the recovered 500), so a panic raised here happens after Recoverer has already
// returned and would escape to net/http and close the connection. Isolating each
// callback keeps a buggy resolver or metric hook from turning an otherwise
// completed request into a connection reset; it degrades gracefully instead —
// the client_ip attribute is omitted, or the metric is skipped — mirroring
// Recoverer's isolation of its WithPanicHook.
func (c *logConfig) emitAccessLog(rec *StatusRecorder, r *http.Request, path, id string, start time.Time) {
	d := time.Since(start)
	status := rec.Status()
	if c.pathFunc != nil {
		path = c.safeLoggedPath(r, id)
	}
	args := []any{
		"method", r.Method,
		"path", path,
		"status", status,
		"duration_ms", d.Milliseconds(),
		"request_id", id,
	}
	if c.logClientIP {
		if ip, ok := c.safeClientIP(r, id, path); ok {
			args = append(args, "client_ip", ip)
		}
	}
	lvl := slog.LevelInfo
	if c.logLevel != nil {
		lvl = c.safeLogLevel(r, status, id, path)
	}
	c.logger.Log(context.Background(), lvl, "http", args...)
	if c.recordMetric != nil || c.recordMetricReq != nil {
		c.safeRecordMetric(r, path, status, d, id)
	}
}

// safeLoggedPath resolves the recorded path via the caller-supplied
// WithPathFunc transform in isolation, fail-closed on every failure shape: a
// panicking fn is logged as a hook failure and an empty return is coerced —
// both degrade to the redactedPathFallback placeholder rather than the raw
// r.URL.Path, because a broken transform must not reopen the credential leak
// it exists to close. The failure diagnostic carries only taint-free fields
// (panic value, stack, request id): the raw path is withheld by design, and
// the method is redundant with the paired access line (same request_id),
// which always still emits.
func (c *logConfig) safeLoggedPath(r *http.Request, id string) (path string) {
	defer func() {
		if v := recover(); v != nil {
			c.logger.Error("webhttp: path transform failed",
				"panic", v,
				"stack", string(debug.Stack()),
				"request_id", id,
			)
			path = redactedPathFallback
		}
	}()
	return c.resolvePathValue(r)
}

// resolvePathValue runs the transform and coerces an empty return to the
// fail-closed placeholder.
func (c *logConfig) resolvePathValue(r *http.Request) string {
	if p := c.pathFunc(r); p != "" {
		return p
	}
	return redactedPathFallback
}

// safeClientIP resolves the "client_ip" value in isolation. A panic in the
// caller-supplied WithClientIPFunc resolver (or in ClientIP) is logged as a hook
// failure and reported as ok=false, so emitAccessLog omits ONLY the client_ip
// attribute and the access line still emits, rather than letting the panic
// escape the outer Logging defer and close the connection.
func (c *logConfig) safeClientIP(r *http.Request, id, path string) (ip string, ok bool) {
	defer func() {
		if v := recover(); v != nil {
			c.logger.Error("webhttp: client_ip resolver failed",
				"panic", v,
				"stack", string(debug.Stack()),
				"request_id", id,
				"method", r.Method,
				"path", path,
			)
			ip, ok = "", false
		}
	}()
	return c.resolveClientIP(r), true
}

// safeLogLevel resolves the access-line level via the caller-supplied
// WithLogLevel policy in isolation. A panic in the policy is logged as a hook
// failure and the line falls back to Info — the access line itself must
// always emit, so a buggy level policy degrades to the default level rather
// than escaping the outer Logging defer (which runs outside Recoverer) and
// closing the connection.
func (c *logConfig) safeLogLevel(r *http.Request, status int, id, path string) (lvl slog.Level) {
	defer func() {
		if v := recover(); v != nil {
			c.logger.Error("webhttp: log level hook failed",
				"panic", v,
				"stack", string(debug.Stack()),
				"request_id", id,
				"method", r.Method,
				"path", path,
				"status", status,
			)
			lvl = slog.LevelInfo
		}
	}()
	return c.logLevel(r, status)
}

// safeRecordMetric fires the caller-supplied metric hook (WithRecordMetric or
// WithRecordMetricRequest — mutual exclusion means at most one is set) in
// isolation. A panic in the hook is logged as a hook failure and swallowed —
// the metric for this request is skipped — so it cannot escape the outer
// Logging defer (which runs outside Recoverer) and turn a completed request
// into a net/http connection-closing panic.
func (c *logConfig) safeRecordMetric(r *http.Request, path string, status int, d time.Duration, id string) {
	defer func() {
		if v := recover(); v != nil {
			c.logger.Error("webhttp: metric hook failed",
				"panic", v,
				"stack", string(debug.Stack()),
				"request_id", id,
				"method", r.Method,
				"path", path,
				"status", status,
				"duration_ms", d.Milliseconds(),
			)
		}
	}()
	if c.recordMetricReq != nil {
		c.recordMetricReq(r, status, d)
		return
	}
	c.recordMetric(r.Method, path, status, d)
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
// With WithPathFunc the recorded path is fn's return instead of r.URL.Path —
// the token-redaction middle ground between logging a credential-bearing path
// raw and skipping its access record entirely (fail-closed placeholder when
// the transform fails; see the option).
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
