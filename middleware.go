package webhttp

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"time"
)

// Middleware is the standard net/http decorator shape: it wraps an http.Handler
// and returns a new one. It is a type alias, so any plain
// func(http.Handler) http.Handler value is a Middleware without conversion.
type Middleware = func(http.Handler) http.Handler

// Chain wraps h with the given middlewares and returns the composed handler.
// The FIRST middleware listed becomes the OUTERMOST wrapper: it is the first to
// see the request on the way in and the last to touch the response on the way
// out. The LAST middleware listed sits closest to h.
//
// So Chain(h, A, B, C) is equivalent to A(B(C(h))): a request flows
// A -> B -> C -> h and the response unwinds h -> C -> B -> A. List middlewares
// in the order you want them to execute; a typical stack puts logging outermost
// so it observes the final status, then a panic recoverer, then app concerns:
//
//	handler := webhttp.Chain(mux,
//		webhttp.Logging(),         // outermost: logs every request
//		webhttp.Recoverer(),       // catches panics from everything below it
//		webhttp.SecurityHeaders(), // sets headers before the app runs
//	)
//
// A nil middleware in the list is skipped, so callers can include entries
// conditionally.
func Chain(h http.Handler, mw ...Middleware) http.Handler {
	// Apply in reverse so the first-listed middleware ends up outermost.
	for _, m := range slices.Backward(mw) {
		if m != nil {
			h = m(h)
		}
	}
	return h
}

// recoverConfig holds resolved Recoverer configuration.
type recoverConfig struct {
	logger    *slog.Logger
	hook      func(v any, stack []byte)
	responder ErrorResponder
}

// RecoverOption configures Recoverer.
type RecoverOption func(*recoverConfig)

// WithRecoverLogger sets the slog.Logger used to report a recovered panic.
// Defaults to slog.Default().
func WithRecoverLogger(l *slog.Logger) RecoverOption {
	return func(c *recoverConfig) { c.logger = l }
}

// WithPanicHook registers a callback invoked with the recovered value and the
// captured stack when a panic is caught. Use it to increment a metric or notify
// an error tracker. It runs after the panic is logged and before the 500
// response is written; a nil hook is ignored.
func WithPanicHook(fn func(v any, stack []byte)) RecoverOption {
	return func(c *recoverConfig) {
		if fn != nil {
			c.hook = fn
		}
	}
}

// WithRecoverResponder sets the ErrorResponder that writes the 500 body after a
// recovered panic (only when the response is not already committed). It defaults
// to WriteError - the JSON envelope; supply one to render the 500 on a different
// content type, for example an XML endpoint returning its own error document.
// The responder owns writing the status and headers. A nil responder is ignored,
// keeping the default.
func WithRecoverResponder(fn ErrorResponder) RecoverOption {
	return func(c *recoverConfig) {
		if fn != nil {
			c.responder = fn
		}
	}
}

// Recoverer returns middleware that recovers a panic from a downstream handler,
// logs it at Error with the stack and the request id (via
// RequestIDFromContext), fires any WithPanicHook callback, and writes a 500
// error via the configured ErrorResponder - WriteError by default, i.e. the
// JSON envelope {"error":"internal server error","code":"internal_error"}.
// Override the responder with WithRecoverResponder to render the 500 on another
// content type. Without it, a handler panic unwinds to net/http, which closes
// the connection abruptly with no response body.
//
// The http.ErrAbortHandler sentinel is deliberately NOT recovered: per the
// net/http contract it is re-panicked so the server aborts the response the way
// the handler intended (it is not logged and fires no hook).
//
// Placement relative to Logging matters. Put Recoverer INSIDE RequestLogger,
// i.e. Logging outermost, as in Chain(mux, Logging(), Recoverer()): the
// recovered request then records a 500 before RequestLogger's deferred access
// line runs, so the request logs as 500. If Recoverer sits OUTSIDE the logger,
// RequestLogger's deferred line runs during the panic unwind and records the
// StatusRecorder's default 200, a misleading access line even though the client
// still receives the 500.
//
// The 500 is best-effort and never double-writes: if the handler already
// committed the response (wrote headers or body) before panicking, the status
// is on the wire and cannot be changed, so Recoverer skips the body entirely
// (it still logs the panic and fires the hook) rather than corrupting the
// partial response or mislabeling its status under an outer Logging. To detect
// commitment it observes the response through a StatusRecorder, reusing an
// existing one (such as RequestLogger's) when present.
func Recoverer(opts ...RecoverOption) Middleware {
	c := &recoverConfig{}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	if c.responder == nil {
		c.responder = WriteError
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The 500 must not be written onto a response the handler already
			// committed, so the recovery body needs to know whether the response
			// was written. If w already reports that (e.g. RequestLogger's
			// StatusRecorder when Recoverer sits inside Logging), use it as-is;
			// otherwise wrap it in a StatusRecorder that both observes commitment
			// and stays transparent to streaming.
			committed, ok := w.(committedResponse)
			if !ok {
				sr := NewStatusRecorder(w)
				w, committed = sr, sr
			}
			defer c.recoverPanic(w, committed, r)
			next.ServeHTTP(w, r)
		})
	}
}

// committedResponse reports whether a response has already been committed
// (status or body written). Recoverer uses it to skip the 500 body when a
// panicking handler had already started the response.
type committedResponse interface {
	Wrote() bool
}

// recoverPanic is the deferred recovery body for the Recoverer middleware. It
// re-panics http.ErrAbortHandler untouched (the net/http silent-abort contract)
// and otherwise logs the panic with its stack and request id, fires any hook,
// and writes the 500 error via the configured responder unless the response was
// already committed.
func (c *recoverConfig) recoverPanic(w http.ResponseWriter, committed committedResponse, r *http.Request) {
	v := recover()
	if v == nil {
		return
	}
	if v == http.ErrAbortHandler {
		panic(v)
	}
	stack := debug.Stack()
	requestID := RequestIDFromContext(r.Context())
	c.logger.Error("webhttp: recovered from panic",
		"panic", v,
		"stack", string(stack),
		"request_id", requestID,
	)
	if c.hook != nil {
		c.fireHook(v, stack, requestID)
	}
	// Only write the 500 when the response has not been committed. Writing onto
	// an already-started response corrupts the body and, under an outer Logging,
	// would mislog the status as the handler's first (e.g. 200) rather than 500.
	if !committed.Wrote() {
		c.writeRecoverResponse(w, r, committed, requestID)
	}
}

// fireHook runs the caller-supplied panic hook in isolation: a panic inside it
// is logged as a secondary failure (carrying the original request id) and
// swallowed, so it cannot abort recovery before the 500 is written.
func (c *recoverConfig) fireHook(v any, stack []byte, requestID string) {
	defer func() {
		if hv := recover(); hv != nil {
			c.logger.Error("webhttp: panic hook failed", "panic", hv, "stack", string(debug.Stack()), "request_id", requestID)
		}
	}()
	c.hook(v, stack)
}

// writeRecoverResponse writes the 500 via the configured responder in isolation:
// a pre-commit responder panic falls back to the default JSON 500 (re-guarded by
// !Wrote() so it never double-writes), while a post-commit responder panic can
// only be logged.
func (c *recoverConfig) writeRecoverResponse(w http.ResponseWriter, r *http.Request, committed committedResponse, requestID string) {
	defer func() {
		if rv := recover(); rv != nil {
			c.logger.Error("webhttp: recover responder failed", "panic", rv, "stack", string(debug.Stack()), "request_id", requestID)
			if !committed.Wrote() {
				WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
			}
		}
	}()
	c.responder(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
}

// securityConfig holds resolved SecurityHeaders configuration. An empty field
// means the corresponding header is not sent.
type securityConfig struct {
	frameOptions      string
	referrerPolicy    string
	csp               string
	permissionsPolicy string
	coop              string
	hsts              string
}

// SecurityOption configures SecurityHeaders.
type SecurityOption func(*securityConfig)

// WithCSP sets the Content-Security-Policy header. The library never builds a
// policy for you: a CSP is application-specific (it must match the app's script
// and style sources, nonces, or hashes), so pass the exact policy string the
// app needs. Unset by default.
func WithCSP(policy string) SecurityOption {
	return func(c *securityConfig) { c.csp = policy }
}

// WithFrameOptions overrides the X-Frame-Options header (default "DENY"). Pass
// an empty string to omit the header, for example when a Content-Security-Policy
// frame-ancestors directive supersedes it.
func WithFrameOptions(v string) SecurityOption {
	return func(c *securityConfig) { c.frameOptions = v }
}

// WithReferrerPolicy overrides the Referrer-Policy header (default
// "strict-origin-when-cross-origin"). Pass an empty string to omit it.
func WithReferrerPolicy(v string) SecurityOption {
	return func(c *securityConfig) { c.referrerPolicy = v }
}

// WithPermissionsPolicy sets the Permissions-Policy header (browser feature
// gating, e.g. "geolocation=(), camera=()"). Unset by default.
func WithPermissionsPolicy(v string) SecurityOption {
	return func(c *securityConfig) { c.permissionsPolicy = v }
}

// WithCOOP sets the Cross-Origin-Opener-Policy header (e.g. "same-origin").
// Unset by default.
func WithCOOP(v string) SecurityOption {
	return func(c *securityConfig) { c.coop = v }
}

// WithHSTS enables the Strict-Transport-Security header with the given max-age
// and the includeSubDomains and preload directives. HSTS is OFF by default:
// enable it only for a service reached exclusively over HTTPS, because a
// browser that sees the header will refuse plain-HTTP and untrusted-certificate
// connections to the host for the whole max-age window. A negative max-age is
// clamped to zero (which instructs browsers to forget the policy).
func WithHSTS(maxAge time.Duration, includeSubdomains, preload bool) SecurityOption {
	secs := max(int64(maxAge.Seconds()), 0)
	v := "max-age=" + strconv.FormatInt(secs, 10)
	if includeSubdomains {
		v += "; includeSubDomains"
	}
	if preload {
		v += "; preload"
	}
	return func(c *securityConfig) { c.hsts = v }
}

// SecurityHeaders returns middleware that sets a baseline of response security
// headers before calling the next handler. Set by default are
// X-Content-Type-Options: nosniff, X-Frame-Options: DENY, and Referrer-Policy:
// strict-origin-when-cross-origin. The X-Frame-Options and Referrer-Policy
// defaults are configurable (override with a value, or omit with an empty
// string); nosniff is set by default with no option to change it here.
// Content-Security-Policy, Permissions-Policy, Cross-Origin-Opener-Policy, and
// Strict-Transport-Security are off unless their options are supplied.
//
// All of these are set BEFORE next runs, so none is immutable: the middleware
// establishes the baseline but does not lock it. A handler that needs a
// different value for a specific response can still override (or delete) any of
// them, nosniff included, on the response header.
func SecurityHeaders(opts ...SecurityOption) Middleware {
	c := &securityConfig{
		frameOptions:   "DENY",
		referrerPolicy: "strict-origin-when-cross-origin",
	}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			setIfNonEmpty(h, "X-Frame-Options", c.frameOptions)
			setIfNonEmpty(h, "Referrer-Policy", c.referrerPolicy)
			setIfNonEmpty(h, "Content-Security-Policy", c.csp)
			setIfNonEmpty(h, "Permissions-Policy", c.permissionsPolicy)
			setIfNonEmpty(h, "Cross-Origin-Opener-Policy", c.coop)
			setIfNonEmpty(h, "Strict-Transport-Security", c.hsts)
			next.ServeHTTP(w, r)
		})
	}
}

// setIfNonEmpty sets header key to val only when val is non-empty, so a cleared
// option leaves the header unsent.
func setIfNonEmpty(h http.Header, key, val string) {
	if val != "" {
		h.Set(key, val)
	}
}

// Logging returns a Chain-composable Middleware wrapping RequestLogger with the
// given options. It is exactly RequestLogger in middleware form; use
// RequestLogger directly when you are not composing with Chain. See
// RequestLogger for the request-id and access-log behavior and the available
// LogOption values.
func Logging(opts ...LogOption) Middleware {
	return func(next http.Handler) http.Handler {
		return RequestLogger(next, opts...)
	}
}

// jsonTimeoutWriter labels a 503 response that carries no Content-Type as JSON,
// so the timeout envelope written by http.TimeoutHandler is served as
// application/json (with nosniff) instead of being content-sniffed as
// text/plain. It only acts on a 503 with no Content-Type already set, so a
// downstream handler that finishes in time and sets its own headers is
// untouched.
type jsonTimeoutWriter struct {
	http.ResponseWriter
}

// WriteHeader applies the JSON content headers to an unlabeled 503 before
// forwarding the status to the underlying writer.
func (w *jsonTimeoutWriter) WriteHeader(code int) {
	if code == http.StatusServiceUnavailable && w.Header().Get("Content-Type") == "" {
		JSONHeaders(w)
	}
	w.ResponseWriter.WriteHeader(code)
}

// RouteTimeout wraps h with http.TimeoutHandler so a handler that runs longer
// than d is cut off with a 503, but replaces net/http's plain-text/HTML timeout
// body with a JSON ErrorResponse ({"error":msg,"code":"timeout"}) served as
// application/json. An empty msg defaults to "request timed out".
//
// A non-positive d disables the timeout: h is returned unwrapped, so its
// response passes through untouched with no 503 relabeling. (http.TimeoutHandler
// with a zero or negative duration would otherwise fire the timeout
// immediately.)
//
// The JSON relabeling keys on status alone: any 503 that reaches the client
// WITHOUT a Content-Type already set is served as application/json, because the
// wrapper cannot tell http.TimeoutHandler's own timeout envelope apart from a
// downstream handler's intentional 503. A handler that emits its own 503 must
// therefore set an explicit Content-Type, or it will be relabeled
// application/json (its body bytes are left unchanged; only the headers are
// added).
//
// It CANNOT wrap streaming or hijacking handlers: http.TimeoutHandler buffers
// the entire response in memory to be able to discard it on timeout, so SSE,
// WebSocket upgrades, and other long-lived or flushing responses do not work
// through it. Apply RouteTimeout only to bounded request/response handlers, and
// use per-request deadlines (http.ResponseController.SetWriteDeadline) for
// streaming routes instead.
//
// The timeout envelope follows the package's universal request-id correlation
// scheme, exactly like WriteError: it is rendered per request, so when the
// request context carries an id (RouteTimeout composed under Logging /
// RequestLogger) the 503 body includes request_id, and when it does not the
// field is omitted. http.TimeoutHandler requires the body pre-rendered at
// construction, which is why the handler is assembled per request around the
// request-scoped envelope.
func RouteTimeout(h http.Handler, d time.Duration, msg string) http.Handler {
	if d <= 0 {
		// A non-positive timeout means "no timeout": return h unwrapped so its
		// response is untouched.
		return h
	}
	if msg == "" {
		msg = "request timed out"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		th := http.TimeoutHandler(h, d, errorBodyJSON(r, "timeout", msg))
		th.ServeHTTP(&jsonTimeoutWriter{ResponseWriter: w}, r)
	})
}
