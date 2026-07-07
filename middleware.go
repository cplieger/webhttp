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
// and writes the 500 JSON error unless the response was already committed.
func (c *recoverConfig) recoverPanic(w http.ResponseWriter, committed committedResponse, r *http.Request) {
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
	// Only write the 500 when the response has not been committed. Writing onto
	// an already-started response corrupts the body and, under an outer Logging,
	// would mislog the status as the handler's first (e.g. 200) rather than 500.
	if !committed.Wrote() {
		WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
	}
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

// ClientIP returns the best-effort client IP for r.
//
// The spoofing model is the point of this helper. X-Forwarded-For is set by
// clients and intermediaries and is trivially forgeable, so it is consulted
// ONLY when the immediate TCP peer (the host part of r.RemoteAddr) is a proxy
// you control, i.e. it falls inside one of the caller-supplied trusted ranges:
//
//   - With NO trusted ranges, or when the direct peer is not inside one, the
//     forwarded header is ignored entirely and the peer address (which cannot
//     be spoofed at this layer) is returned. This is the safe default: no
//     header a client sends can move the result off the real socket peer.
//   - When the peer IS a trusted proxy, X-Forwarded-For is walked from the
//     RIGHT. Each entry that is itself a trusted proxy is skipped (those are
//     your own hops, which appended the address they saw); the first untrusted
//     entry from the right is returned as the client. A malformed entry at that
//     boundary stops the walk, and the peer is returned.
//
// The right-to-left, skip-trusted walk is the only correct reading when the
// proxy APPENDS the peer it observed to X-Forwarded-For (as Caddy and most
// reverse proxies do): the LEFTMOST entry is then whatever the client SENT and
// is attacker-controlled, while the rightmost entries are the trustworthy hops.
// Consequently the trusted set must contain EVERY proxy hop between the client
// and this server; if a hop is missing from the set the walk stops there and
// that hop's address is returned as the client.
//
// X-Real-IP is deliberately NOT consulted: it is client-settable and a proxy
// such as Caddy does not overwrite it, so honoring it would reintroduce a spoof
// vector. It can return as an explicit opt-in if a proxy that overwrites it is
// ever adopted.
//
// The caller supplies the trusted CIDRs (typically the reverse proxy's address
// range); the library hardcodes none. A malformed r.RemoteAddr with no port is
// used verbatim as the host and, being unparseable, is never trusted.
func ClientIP(r *http.Request, trusted ...*net.IPNet) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr had no host:port form; use it as-is.
		host = r.RemoteAddr
	}

	peer := net.ParseIP(host)
	// Consult X-Forwarded-For only when the immediate peer is a trusted proxy.
	// With no trusted ranges (or an unparseable peer) this is always false, so
	// the socket peer is returned: the spoof-proof default.
	if peer == nil || !ipInTrusted(peer, trusted) {
		return host
	}

	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Walk right-to-left, skipping our own trusted hops; the first untrusted
		// entry from the right is the client.
		for _, part := range slices.Backward(strings.Split(xff, ",")) {
			ip := net.ParseIP(strings.TrimSpace(part))
			if ip == nil {
				break // malformed boundary: stop, trust nothing further left
			}
			if ipInTrusted(ip, trusted) {
				continue // our own proxy hop, keep walking left
			}
			return ip.String()
		}
	}
	// Trusted peer but no usable forwarded entry: fall back to the peer.
	return host
}

// ipInTrusted reports whether ip is contained in any of the given trusted
// networks.
func ipInTrusted(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

// ParseCIDRs parses trusted reverse-proxy entries into the *net.IPNet set that
// ClientIP and WithClientIP consult. Each entry is either CIDR notation
// ("10.0.0.0/8") or a bare IP address ("192.168.1.5", "::1"), a bare address
// being treated as a single host (/32 for IPv4, /128 for IPv6). Surrounding
// whitespace is trimmed and blank entries are skipped.
//
// It returns the successfully parsed networks and, separately, the list of
// entries that were neither a valid CIDR nor a valid IP. Returning both (rather
// than failing on the first bad entry) lets a strict caller reject the input —
// e.g. config-file validation surfacing the offending entry — while a lenient
// caller logs the bad entries and proceeds with the valid subset, e.g. reading
// a TRUSTED_PROXIES environment variable where one typo should not disable proxy
// awareness entirely. Both usages share this one parser instead of each app
// reimplementing the CIDR/bare-IP handling. The returned slice is passed
// straight to ClientIP(r, nets...) or WithClientIP(nets...); an empty result
// means "trust nothing", the spoof-proof default that logs the socket peer.
func ParseCIDRs(entries []string) (nets []*net.IPNet, invalid []string) {
	for _, e := range entries {
		s := strings.TrimSpace(e)
		if s == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(s); err == nil {
			nets = append(nets, n)
			continue
		}
		if ip := net.ParseIP(s); ip != nil {
			bits := 8 * net.IPv4len
			if ip.To4() == nil {
				bits = 8 * net.IPv6len
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		invalid = append(invalid, s)
	}
	return nets, invalid
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
// Because the timeout body is produced inside http.TimeoutHandler, outside
// request scope, it carries no request_id (unlike WriteError). The 503 envelope
// is otherwise identical to the package's other JSON errors.
func RouteTimeout(h http.Handler, d time.Duration, msg string) http.Handler {
	if d <= 0 {
		// A non-positive timeout means "no timeout": return h unwrapped so its
		// response is untouched.
		return h
	}
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
