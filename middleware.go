package webhttp

import (
	"errors"
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

// The two relational rules an HSTS value carries, both of them preload
// submission criteria the browser preload list enforces (hstspreload.org):
// preload is meaningless without includeSubDomains, and a preload submission
// needs a max-age of at least one year. Both are policed, and for the same
// reason: a policy asking for preload while failing a submission criterion is
// a policy that will never be preloaded, which is silently not what the caller
// asked for. Without Preload set, MaxAge is unconstrained — a short max-age is
// a valid working policy, it just cannot be preloaded.
var (
	ErrHSTSPreloadWithoutSubdomains = errors.New("webhttp: HSTS preload requires IncludeSubdomains")
	ErrHSTSPreloadMaxAgeTooShort    = errors.New("webhttp: HSTS preload requires MaxAge of at least one year")
)

// hstsPreloadMinMaxAge is the preload list's minimum max-age (one year), the
// value hstspreload.org states as a submission requirement.
const hstsPreloadMinMaxAge = 365 * 24 * time.Hour

// HSTS is the Strict-Transport-Security policy passed to WithHSTS. The fields
// are named rather than positional because two of them are booleans standing
// for different directives: as a pair of adjacent arguments they could be
// handed over transposed and the header would still build.
//
// The zero value is MEANINGFUL but is not "off": it renders "max-age=0", the
// directive that tells a browser to FORGET an HSTS policy it already holds for
// the host. HSTS off is leaving the option out of SecurityHeaders entirely.
//
// A negative MaxAge is clamped to zero (same "forget it" instruction), and
// MaxAge is truncated to whole seconds, which is the only unit the header has.
type HSTS struct {
	// MaxAge is how long a browser should keep enforcing HTTPS for the host.
	MaxAge time.Duration
	// IncludeSubdomains extends the policy to every subdomain
	// (includeSubDomains). Set it only when every subdomain is served over
	// HTTPS; a browser that has seen it will refuse a plain-HTTP subdomain for
	// the whole MaxAge window.
	IncludeSubdomains bool
	// Preload asks for inclusion in the browsers' built-in preload list, so the
	// very first request to the host is upgraded. It REQUIRES IncludeSubdomains
	// (see HSTS.Validate) and, for an actual submission, a MaxAge of at least a
	// year. Preloading is effectively permanent from an operations point of
	// view: removal takes months to reach users.
	Preload bool
}

// Validate reports whether the policy is internally consistent: it returns
// ErrHSTSPreloadWithoutSubdomains when Preload is set without
// IncludeSubdomains, ErrHSTSPreloadMaxAgeTooShort when Preload is set with a
// MaxAge under one year (which also catches the classic unit mistake — an
// integer count of seconds assigned to a time.Duration field is nanoseconds,
// so 31536000 renders max-age=0, "forget me", beside "preload me"), and nil
// otherwise.
//
// It is the STRICT door for a caller that builds its HSTS value from
// configuration and wants to refuse to boot on a contradiction — the same split
// ParseCIDRs and ParseHostList offer, where the invalid input is returned and
// the caller decides between rejecting it and proceeding. WithHSTS itself takes
// the lenient half of that split, because a SecurityOption has no way to return
// anything: it drops the offending directive and logs.
func (h HSTS) Validate() error {
	if !h.Preload {
		return nil
	}
	if !h.IncludeSubdomains {
		return ErrHSTSPreloadWithoutSubdomains
	}
	if h.MaxAge < hstsPreloadMinMaxAge {
		return ErrHSTSPreloadMaxAgeTooShort
	}
	return nil
}

// header renders the Strict-Transport-Security value for h. It emits the
// preload directive only for a policy that Validate accepts, so the refusal has
// exactly one definition and takes effect here.
func (h HSTS) header() string {
	secs := max(int64(h.MaxAge.Seconds()), 0)
	v := "max-age=" + strconv.FormatInt(secs, 10)
	if h.IncludeSubdomains {
		v += "; includeSubDomains"
	}
	if h.Preload && h.Validate() == nil {
		v += "; preload"
	}
	return v
}

// WithHSTS enables the Strict-Transport-Security header from the given policy.
// HSTS is OFF by default: enable it only for a service reached exclusively over
// HTTPS, because a browser that sees the header will refuse plain-HTTP and
// untrusted-certificate connections to the host for the whole max-age window.
//
//	webhttp.SecurityHeaders(webhttp.WithHSTS(webhttp.HSTS{
//		MaxAge:            365 * 24 * time.Hour,
//		IncludeSubdomains: true,
//		Preload:           true,
//	}))
//
// A policy asking for Preload WITHOUT IncludeSubdomains is REFUSED, not
// repaired: the header is emitted without the preload directive, and the
// contradiction is logged at Warn naming the field to set (once per WithHSTS
// call, which is wiring time rather than a request path). That is the honest
// rendering — the browsers' preload list rejects such a submission
// (hstspreload.org), so a header carrying the directive would advertise a
// posture the operator does not actually get, and the failure would surface
// only at submission time.
//
// The two rejected alternatives are worth recording. PANICKING is wrong for a
// value that can legitimately come from configuration; the library's answer for
// a caller that wants to fail closed at startup is HSTS.Validate, which returns
// the same verdict as an error. And SILENTLY SETTING IncludeSubdomains would
// widen a security policy the caller did not ask for: HSTS on every subdomain
// is a strictly larger commitment than HSTS on one host, and inferring it from
// a neighbouring flag is exactly the kind of repair CanonicalHost refuses for
// the same reason.
func WithHSTS(policy HSTS) SecurityOption {
	if err := policy.Validate(); err != nil {
		fix := "set HSTS.IncludeSubdomains, or clear HSTS.Preload"
		if errors.Is(err, ErrHSTSPreloadMaxAgeTooShort) {
			// Name the unit too: an integer count of seconds in a Duration
			// field is nanoseconds, which is how a policy meant to say one
			// year ends up saying max-age=0.
			fix = "set HSTS.MaxAge to at least 365*24*time.Hour (a bare 31536000 is nanoseconds), or clear HSTS.Preload"
		}
		slog.Warn("webhttp: HSTS preload directive dropped from the header",
			"error", err,
			"fix", fix,
		)
	}
	v := policy.header()
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

// NoStore returns middleware that sets Cache-Control: no-store on every
// response passing through it, before the next handler runs.
//
// The value is FIXED. This is deliberately not a cache-policy API: the one
// header every dynamic surface needs is worth a name, and anything richer is
// per-asset POLICY that already has a home in WithStaticCacheControl. Three
// consumers had hand-written this exact three-line wrapper — for a
// state-reporting API, for an SPA's HTML and API responses, and for an /api/
// subtree whose handlers set no cache directive at all — and the library
// already set the same value internally at ReadinessHandler without exporting a
// composable form.
//
// PLACEMENT AND OVERRIDE ORDERING STAY APP-OWNED, exactly like
// WithStaticCacheControl's policy hook. The header is set before next runs and
// is not locked: a handler or an inner middleware that needs a different value
// for its own responses (a long-lived immutable asset, a per-asset cache
// policy, a cacheable preview image) simply Sets Cache-Control itself and wins,
// which is why the usual placement is innermost in the Chain — early enough to
// land before any handler writes a status, late enough that the asset paths
// that need to override it still can. Scope it by mounting it on the subtree
// that needs it rather than by configuring it.
//
// It is NOT the middleware for a conditional no-store: a response that must
// only be uncacheable when it carries a Set-Cookie (the OWASP session-
// management rule) has a different trigger and a different concern, and belongs
// with the code that sets the cookie.
func NoStore() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
		})
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
// That buffering writer is also not unwrappable, so a handler under it cannot
// reach the ResponseWriter net/http gave the request: on a body-reading route,
// LimitBody's over-limit read still fails with a *http.MaxBytesError but net/http
// is never told to close the connection (see LimitBody).
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
