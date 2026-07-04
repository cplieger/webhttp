package webhttp

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
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
	logger *slog.Logger
	hook   func(v any, stack []byte)
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
	return func(c *recoverConfig) { c.hook = fn }
}

// Recoverer returns middleware that recovers a panic from a downstream handler,
// logs it at Error with the stack and the request id (via
// RequestIDFromContext), fires any WithPanicHook callback, and writes a 500
// JSON error via WriteError(w, r, 500, "internal_error", "internal server
// error"). Without it, a handler panic unwinds to net/http, which closes the
// connection abruptly with no response body.
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
// The 500 is best-effort: if the handler already wrote response headers before
// panicking, the status is on the wire and cannot be changed (net/http logs a
// "superfluous WriteHeader" warning and the error body is appended).
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

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer c.recoverPanic(w, r)
			next.ServeHTTP(w, r)
		})
	}
}

// recoverPanic is the deferred recovery body for the Recoverer middleware. It
// re-panics http.ErrAbortHandler untouched (the net/http silent-abort contract)
// and otherwise logs the panic with its stack and request id, fires any hook,
// and writes the 500 JSON error.
func (c *recoverConfig) recoverPanic(w http.ResponseWriter, r *http.Request) {
	v := recover()
	if v == nil {
		return
	}
	if v == http.ErrAbortHandler {
		panic(v)
	}
	stack := debug.Stack()
	c.logger.Error("webhttp: recovered from panic",
		"panic", v,
		"stack", string(stack),
		"request_id", RequestIDFromContext(r.Context()),
	)
	if c.hook != nil {
		c.hook(v, stack)
	}
	WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
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
// headers before calling the next handler. The always-on default is
// X-Content-Type-Options: nosniff, plus X-Frame-Options: DENY and
// Referrer-Policy: strict-origin-when-cross-origin (each overridable, or
// omittable with an empty value). Content-Security-Policy, Permissions-Policy,
// Cross-Origin-Opener-Policy, and Strict-Transport-Security are off unless their
// options are supplied.
//
// Headers are set before next runs, so a handler that needs a different value
// for a specific response can still override them.
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

// ClientIP returns the best-effort client IP for r.
//
// The spoofing model is the point of this helper. The X-Forwarded-For and
// X-Real-IP headers are set by clients and intermediaries and are trivially
// forgeable, so they can only be trusted when the immediate peer is a proxy you
// control:
//
//   - With NO trusted ranges, forwarded headers are ignored entirely and the
//     host part of r.RemoteAddr (the TCP peer, which cannot be spoofed at this
//     layer) is returned. This is the safe default.
//   - With one or more trusted ranges, the headers are honored ONLY when
//     r.RemoteAddr's IP falls inside a trusted range, meaning a proxy you
//     control set them. In that case the leftmost X-Forwarded-For entry (the
//     original client) is returned, falling back to X-Real-IP, then to the peer
//     address. When the peer is not trusted, its address is returned and the
//     forwarded headers are ignored.
//
// The caller supplies the trusted CIDRs (typically the reverse proxy's
// address range); the library hardcodes none. A malformed r.RemoteAddr with no
// port is used verbatim as the host.
func ClientIP(r *http.Request, trusted ...*net.IPNet) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr had no host:port form; use it as-is.
		host = r.RemoteAddr
	}

	// No trusted proxies: the peer address is the only trustworthy source.
	if len(trusted) == 0 {
		return host
	}

	peer := net.ParseIP(host)
	if peer == nil || !ipInAny(peer, trusted) {
		// The direct peer is not a trusted proxy (or is unparseable), so any
		// forwarded header is attacker-controlled and must be ignored.
		return host
	}

	// The peer is a trusted proxy, so its forwarded headers are believable.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
			return ip.String()
		}
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		if ip := net.ParseIP(xr); ip != nil {
			return ip.String()
		}
	}
	// Trusted peer but no usable forwarded header: fall back to the peer.
	return host
}

// ipInAny reports whether ip is contained in any of the given networks.
func ipInAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n != nil && n.Contains(ip) {
			return true
		}
	}
	return false
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
// It CANNOT wrap streaming or hijacking handlers: http.TimeoutHandler buffers
// the entire response in memory to be able to discard it on timeout, so SSE,
// WebSocket upgrades, and other long-lived or flushing responses do not work
// through it. Apply RouteTimeout only to bounded request/response handlers, and
// use per-request deadlines (http.ResponseController.SetWriteDeadline) for
// streaming routes instead.
//
// Because the timeout body is produced inside http.TimeoutHandler, outside
// request scope, it carries no request_id (unlike WriteError). The 503 envelope
// is otherwise identical to the package's other JSON errors.
func RouteTimeout(h http.Handler, d time.Duration, msg string) http.Handler {
	if msg == "" {
		msg = "request timed out"
	}
	env, err := json.Marshal(ErrorResponse{Error: msg, Code: "timeout"})
	if err != nil {
		// ErrorResponse is a fixed struct of strings, so marshaling cannot fail;
		// this keeps the timeout body valid JSON even if that ever changes.
		env = []byte(`{"error":"request timed out","code":"timeout"}`)
	}
	th := http.TimeoutHandler(h, d, string(env))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		th.ServeHTTP(&jsonTimeoutWriter{ResponseWriter: w}, r)
	})
}
