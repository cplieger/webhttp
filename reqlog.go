package webhttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
	"unicode/utf8"
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
	recordMetric    func(RequestMetric)
	recordMetricReq func(r *http.Request, status int, d time.Duration)
	recordRoute     func(RequestMetric)
	logLevel        func(r *http.Request, status int) slog.Level
	clientIPFunc    func(*http.Request) string
	clientIPTrusted []*net.IPNet
	maxLoggedPath   int
	logClientIP     bool
	skipUpgrades    bool
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

// WithSkipUpgrades selects whether the access record is suppressed for a request
// whose response SWITCHED PROTOCOLS — and for nothing else. Enabled (true) it is
// what a WebSocket route should use instead of a WithSkipFunc predicate that
// PREDICTS which requests will upgrade.
//
// The decision comes from the response, not from the request. The record is
// suppressed when the recorded status is 101 (the handshake went through the
// ResponseWriter, as coder/websocket's WriteHeader(101)-then-hijack does) or
// when the handler hijacked the connection before recording anything at all
// (the handler wrote the handshake onto the connection itself, as
// gorilla/websocket does). Those are the two shapes a completed upgrade takes,
// and in both the exchange has ENDED rather than completed, so the one line
// that would be emitted when the socket finally closes — hours later, carrying
// a session-length duration and a status net/http never sent — describes
// something that never happened. Every OTHER outcome on the same route keeps
// its record with its real status, duration, request id, and client ip: the 400
// for a malformed Sec-WebSocket-Key, the 403 for a disallowed origin, the 405
// for a non-GET, the 426 for missing upgrade headers. Those refusals are what
// an operator greps for when a browser cannot attach or a reverse proxy mangles
// a handshake.
//
// Why the decision belongs here: a skip predicate has to answer before the
// handler runs, so it must model the handshake policy of whichever library will
// answer the request, and it drifts when that library changes. A consumer's
// predicate checked that Sec-WebSocket-Key was present exactly once;
// coder/websocket base64-decodes that header and requires exactly 16 bytes,
// answering 400 otherwise, so every malformed-key 400 was suppressed as if it
// had upgraded — as was the cross-origin 403 the same predicate could not model
// — losing status, duration, request id, and client ip for exactly the refused
// requests worth seeing. The fact the predicate was guessing at is known HERE,
// once the response exists.
//
// The interaction with the skip options is one-way. WithSkipPaths and
// WithSkipFunc are evaluated before the handler and bypass the recorder
// entirely, so a request either of them matches is skipped whatever status it
// ends up with, and this option cannot bring its record back. This option only
// ever REMOVES a record that would otherwise have been emitted.
//
// Suppression removes the whole record, the same pairing WithSkipPaths has and
// for the same reason: no access line and no metric hook, and neither the
// WithLogLevel policy nor a WithPathFunc / WithTemplatePathsUnder transform is
// consulted. The request id is still minted, echoed, and threaded, as on every
// path through this middleware. A handler that PANICS after the switch is not
// hidden by that: Recoverer logs the panic and its stack from its own line,
// which is where a crash mid-session belongs — the access line it would
// otherwise pair with says 101, not 500, because the status was decided at the
// handshake.
//
// Two boundaries, because both KEEP their record:
//
//   - A handler that never calls WriteHeader records the implicit 200 net/http
//     sends. That is not 101, so an ordinary request that writes only a body is
//     logged exactly as it is without this option.
//   - A handler that writes an explicit status and THEN hijacks (an HTTP CONNECT
//     tunnel answering 200) keeps its record with that status: it told us what
//     it answered, and only 101 says "this is no longer an HTTP exchange". A
//     consumer that wants such a tunnel silent still has WithSkipFunc, whose
//     prediction problem does not apply to a test on the method.
//
// It takes no status argument on purpose. 101 is the one status that means the
// exchange ended rather than completed, so a general skip-these-statuses option
// would add exactly one capability: silencing refusals. A 404 flood is a broken
// route and a 401 flood is an attack, and deleting their records is CWE-778
// rather than noise control — which WithLogLevel (or the ProbeLogLevel preset)
// already does properly, by lowering a line's level instead of removing it.
//
// HTTP/2's extended-CONNECT upgrade (RFC 8441) answers 2xx rather than 101 and
// is deliberately out of scope; it cannot arise for this option's audience
// anyway, since a server that hijacks to speak WebSocket needs HTTP/1.1.
//
// The default (false) matches leaving the option out: every request keeps its
// record whatever status it ends on. Passing false explicitly is how a caller
// threads its own computed flag without branching around the option, and options
// are last-wins, so a later WithSkipUpgrades(false) restores the records an
// earlier true suppressed.
func WithSkipUpgrades(skip bool) LogOption {
	return func(c *logConfig) { c.skipUpgrades = skip }
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
// "path" attribute of the package's hook-failure diagnostics. It feeds NEITHER
// request-derived metric hook: WithRecordMetricRequest receives the request
// itself and owns its own representation, and WithRecordRouteMetric's path label
// is the matched route, which is a bound of its own and not a redaction policy.
// fn's return is still length-bounded before it is recorded (512 bytes by
// default, WithMaxLoggedPath to change it): a transform is a REDACTION policy,
// and nothing about redacting a token also bounds the path it was in.
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

// WithTemplatePathsUnder declares URL prefixes whose concrete paths carry a
// CREDENTIAL, so the access log records the matched ROUTE TEMPLATE for them
// instead of the path itself: "/api/sessions/{id}" rather than
// "/api/sessions/6f3a…". Prefer it over WithPathFunc for this job — it is the
// same protection expressed as data, and the reason that matters is below.
//
// The template comes from r.Pattern, which http.ServeMux populates during
// routing, so the router is the source of truth for what matched. The method
// prefix ServeMux includes ("DELETE /api/sessions/{id}") is stripped, since the
// method is already its own attribute on the line.
//
// Three cases, and the middle one is the whole point:
//
//   - a path under a declared prefix that MATCHED a route: the template.
//   - a path under a declared prefix that matched NOTHING (a 404 on
//     "/api/sessions/6f3a…/nope"): the prefix plus an "(unmatched)" marker.
//     Never the raw path — an unrouted request under a credential-bearing
//     prefix still has the credential in it, and this is exactly the leak a
//     path policy exists to close. It is also visible as unmatched rather than
//     mislabelled onto a route it is not, so a NEW upstream subroute shows up
//     in the log as something to wire rather than disappearing.
//   - a path outside every declared prefix: recorded unchanged. Deliberately
//     not the template, because a static mount's pattern is "/" and logging
//     that would collapse every asset onto one line, losing which asset 404'd.
//     Credential-bearing routes are a per-route-family fact, not a per-app one,
//     which is why this option takes prefixes rather than being a global switch.
//
// Why this exists as a declarative option rather than leaving callers to write
// their own WithPathFunc: two apps in this fleet hand-rolled the identical
// transform over the same upstream route table and DIVERGED on the unmatched
// case — one returned "" (indistinguishable from a broken transform) and one
// returned an "(unmapped)" marker. A free-form hook makes that outcome the
// default. Expressed as data, the policy has one implementation, and the
// unmatched-route decision is made once here instead of once per consumer.
//
// Pair it with the prefix the route-owning package exports (e.g. the terminal
// engine's SessionsSubtreePath) rather than a local string literal, so the set
// of credential-bearing routes stays owned by whoever declares those routes.
//
// A prefix that is empty is ignored. Applying this option replaces any
// previously-set path policy (including WithPathFunc), and vice versa: there is
// one recorded path, so the last policy applied wins.
func WithTemplatePathsUnder(prefixes ...string) LogOption {
	kept := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return func(c *logConfig) {
		if len(kept) == 0 {
			return
		}
		c.pathFunc = func(r *http.Request) string {
			return templatePath(r, kept)
		}
	}
}

// unmatchedTemplateMarker is appended to a declared prefix when a request under
// it matched no route. It is NOT redactedPathFallback: that placeholder means
// "the path policy itself failed", and conflating a routine 404 with a broken
// policy would hide the latter. This one says "routed nowhere, and the path is
// withheld because it is under a credential-bearing prefix".
const unmatchedTemplateMarker = "(unmatched)"

// templatePath implements the WithTemplatePathsUnder policy. Split out from the
// option so the option stays a thin binding and this stays testable as a pure
// function of (request, prefixes).
func templatePath(r *http.Request, prefixes []string) string {
	p := r.URL.Path
	under := ""
	for _, prefix := range prefixes {
		// Longest matching prefix wins, so nested declarations behave the way a
		// reader expects rather than depending on argument order.
		if strings.HasPrefix(p, prefix) && len(prefix) > len(under) {
			under = prefix
		}
	}
	if under == "" {
		return p
	}
	// ServeMux's pattern is "[METHOD ][HOST]/path"; take everything from the
	// first "/" so the recorded value is a bare path template. A pattern with no
	// "/" at all cannot be a path and is treated as no match.
	if i := strings.Index(r.Pattern, "/"); i >= 0 {
		return r.Pattern[i:]
	}
	return under + unmatchedTemplateMarker
}

// defaultMaxLoggedPath is the byte cap the access line applies to the recorded
// path when no WithMaxLoggedPath tightens or widens it. Every path resolution
// ends in it — the raw r.URL.Path default, WithTemplatePathsUnder's template,
// and a caller's own WithPathFunc return — so it is a FLOOR no consumer can
// accidentally miss rather than a policy each one opts into.
//
// Why the bound exists at all: r.URL.Path is attacker-controlled, and net/http
// bounds the request line and headers TOGETHER at MaxHeaderBytes plus 4 KiB,
// which is 1 MiB plus 4 KiB at this package's own NewServer default. One
// request therefore buys a megabyte access-log line, and the line lands in the
// same aggregated store (Loki here) as the warnings an operator greps for, so a
// flood evicts the records that matter — the retention half of CWE-779. Audited
// across this package's ten consumers, nine emit the access line and exactly
// ONE bounded the path, by hand; the other eight, two of them publicly exposed,
// logged whatever arrived.
//
// Why 512: the cap must not silently truncate a path any consumer legitimately
// serves, because a version bump applies it to all of them without a line of
// their code changing, and a truncated path is one an operator cannot act on.
// Measured over the same consumers, the longest registered route pattern is 34
// bytes of path template ("/api/auth/webauthn/register/finish") and the longest
// embedded static asset path is 23 ("/icons/chevron-bold.svg"); the query
// string is never logged, so those are the real figures rather than whole-URL
// lengths. 512 leaves better than an order of magnitude of headroom over both
// while cutting the hostile case by roughly 2000x.
//
// It bounds the LOG, nothing else. The request is read, routed, and served
// exactly as before: request SIZE stays WithMaxHeaderBytes' job, so RFC 9112
// §3's recommendation that a recipient support request-lines of at least 8000
// octets is untouched.
const defaultMaxLoggedPath = 512

// truncatedPathMarker is appended to a recorded path the cap cut. Visible on
// purpose: a silently shortened path reads as a genuine request for a route
// that does not exist, sending an operator to hunt for a missing handler
// instead of a client sending a megabyte URL. It shares the shape of this
// package's other markers ("(unmatched)", "(path-redaction-failed)"). Unlike
// the method's marker it is not unforgeable — a client may send this text as a
// path — but only a cut value ends in it, and the length tells them apart.
const truncatedPathMarker = "...(truncated)"

// WithMaxLoggedPath sets the byte cap the access line applies to the recorded
// path, replacing the 512-byte default. Reach for it when the app's whole route
// table is short and a tighter log-line budget is worth more than the tail of a
// long path: knell serves /healthz, /metrics, and /beat/{id} with an id its
// config caps at 64 characters, so 128 covers every path it can legitimately
// answer.
//
// n counts PATH bytes. An over-cap value keeps at most n of them, cut on a
// UTF-8 rune boundary, followed by the 14-byte truncation marker, so the
// attribute is at most n+14 bytes; a value within the cap is recorded
// byte-identical. The cap applies to whatever the path policy produced,
// including this package's own fail-closed placeholders, so the guarantee has
// no branch-shaped hole.
//
// A non-positive n is ignored and the default stands (the skip-nil option
// convention). There is deliberately no way to switch the bound OFF: it exists
// because eight of the nine access-log consumers had no path bound of their
// own, so an option that could zero it would reopen precisely the hole it
// closes — and a config-driven 0 would do it silently.
func WithMaxLoggedPath(n int) LogOption {
	return func(c *logConfig) {
		if n > 0 {
			c.maxLoggedPath = n
		}
	}
}

// boundLoggedPath applies the recorded-path cap, cutting on a UTF-8 rune
// boundary rather than at the byte. A split rune is not merely ugly: every
// encoder between here and the log store rewrites the orphaned bytes as U+FFFD,
// so the tail of the value is corrupt for the reader, and on a path of
// multi-byte characters two distinct paths can land on the same rendered text.
// Backing off to the last rune start costs at most three bytes of the kept
// prefix.
func boundLoggedPath(p string, maxLen int) string {
	if len(p) <= maxLen {
		return p
	}
	cut := maxLen
	for cut > 0 && !utf8.RuneStart(p[cut]) {
		cut--
	}
	return p[:cut] + truncatedPathMarker
}

// maxLoggedMethod bounds the method the access line records. r.Method is
// caller-chosen text: http.ServeMux hands ANY method to a method-agnostic
// pattern — that is how such a pattern answers 405 — and measured over a real
// socket, a request line naming "M!#$%&'*+-.^_`|~" reaches a handler, as does
// one naming 300 bytes of "M". net/http validates the token's CHARSET before any
// handler runs (a method carrying a delimiter or a control byte is answered 400
// at parse time), which leaves LENGTH as the one axis still bounded only by the
// request line — a megabyte, as for the path.
//
// 24 is the registry ceiling plus headroom. The longest entry in IANA's HTTP
// Method Registry is UPDATEREDIRECTREF at 17 characters (RFC 4437 §7), with
// BASELINE-CONTROL at 16 second; the spare bytes cover an unregistered vendor
// method (PURGE, BAN, and the like are all shorter) so a real token still logs
// as itself. Nothing longer is a method any HTTP implementation issues, which is
// why this bound needs no option: it is a fact about HTTP, not a per-app policy,
// and a knob here would only let a consumer shorten it until DELETE stopped
// logging as DELETE.
const maxLoggedMethod = 24

// overlongMethodMarker replaces a method too long to be one. A fixed
// placeholder rather than a truncated token, because a truncated method LIES:
// "UPDATEREDIRECTR" reads as a method somebody tried, and an operator grepping
// the upstream for it finds nothing. The placeholder is also unforgeable —
// parentheses are delimiters, not token characters (RFC 9110 §5.6.2), and a
// request line spelling this value is answered 400 without reaching a handler
// (measured) — which a truncated prefix of a real method never is.
const overlongMethodMarker = "(overlong)"

// boundLoggedMethod applies the method cap. It runs at every place this package
// records a method VERBATIM — the access line, the three hook-failure
// diagnostics, and the legacy WithRecordMetric hook's method argument, so an
// over-long token cannot reach a metric label either. It is a LENGTH bound, so it
// is not the metric-label bound: RouteMetricLabels maps the method onto a closed
// ten-value set instead. Routing, the status, and any Allow header are untouched:
// this changes what is RECORDED, never what is served.
func boundLoggedMethod(m string) string {
	if len(m) <= maxLoggedMethod {
		return m
	}
	return overlongMethodMarker
}

// RequestMetric is one completed request as a metric hook observes it: the
// labels and the latency, already bounded by whichever hook produced it.
//
// It is a struct rather than four parameters because two of them are adjacent
// strings: a hook implemented as func(path, method string, ...) compiles, runs
// forever, and silently labels every series with the wrong dimension — and
// nothing in a metric can look wrong afterwards, since both values are
// plausible strings. Named fields make the implementor read Method and Path
// instead of counting positions. The stdlib passes multi-value hook data the
// same way: net/http/httptrace's GotConn and DNSDone take GotConnInfo and
// DNSDoneInfo rather than positional arguments.
type RequestMetric struct {
	// Method is the request method, bounded by the hook that produced it.
	Method string
	// Path is the label the hook chose: a bounded request path for
	// [WithRecordMetric], or the matched route template for
	// [WithRecordRouteMetric] (MetricLabelUnmatched when nothing matched).
	Path string
	// Status is the response status actually written.
	Status int
	// Latency is how long the handler took.
	Latency time.Duration
}

// WithRecordMetric registers a hook invoked once per logged request with the
// RequestMetric the access line RECORDED, so its two text fields carry exactly
// the log line's bounds: Method is capped at 24 bytes, which keeps an over-long
// token out of a metric label as well as out of the log, and Path is whatever
// the path policy produced, capped by WithMaxLoggedPath. With NO path policy
// configured — the default — Path is therefore the raw r.URL.Path truncated to
// 512 bytes. It fires from a deferred call, so a panicking handler is still
// recorded. Requests skipped via WithSkipPaths or WithSkipFunc are excluded
// from the hook as well as from access logging: a
// stream's open-to-close duration paired with a synthetic status is misleading,
// which is the whole reason the path is skipped.
//
// Its bounds are LENGTH bounds, not cardinality bounds: a raw r.URL.Path under
// the cap is still one label value per URL a scanner invents, and a method under
// 24 bytes is still one per token. For a metric this hook is the wrong shape
// unless a path policy already collapses the path onto route templates — reach
// for WithRecordRouteMetric, whose labels are bounded by construction. The three
// metric options are mutually exclusive; whichever is applied last wins.
func WithRecordMetric(fn func(RequestMetric)) LogOption {
	return func(c *logConfig) {
		c.recordMetric = fn
		c.recordMetricReq = nil
		c.recordRoute = nil
	}
}

// WithRecordMetricRequest is the request-aware variant of WithRecordMetric:
// fn is invoked once per logged request with the *http.Request itself, the
// final status, and the latency. Because http.ServeMux assigns the matched
// pattern to the request in place, fn observes a populated r.Pattern after
// routing (empty when nothing matched, e.g. a 404), so a caller can key a
// metric on the route TEMPLATE rather than on the raw URL path.
// Caveat: middleware between RequestLogger and the mux that replaces the
// request (r.WithContext and friends return a clone) hides those fields — the
// mux populates the clone, not the request this hook received.
//
// Prefer WithRecordRouteMetric. This hook hands fn the raw request, which makes
// the APP responsible for a bound it can get wrong, and one consumer did: it
// derived its labels from r.Method, wrote a comment asserting they were bounded,
// and shipped an attacker-controllable method label anyway, because it also
// registers a "/" catch-all — so its r.Pattern is never empty, the unmatched
// collapse it relied on can never fire, and every SPA fallthrough minted a
// series named by the caller. Nothing in the hook's signature could have caught
// that. WithRecordRouteMetric computes the bounded pair here and passes it in,
// so there is no derivation left for a consumer to get wrong; reach for this
// hook only when the metric genuinely needs something else from the request
// (say a per-tenant series keyed on an id the app validated itself), and use
// RouteMetricLabels when what it needs is the standard pair.
//
// Like WithRecordMetric it fires from a deferred call (a panicking handler is
// still recorded) and is excluded on paths skipped via WithSkipPaths or
// WithSkipFunc. The three metric options are mutually exclusive; whichever is
// applied last wins. A nil fn is ignored (the package's skip-nil option
// convention), so a trailing WithRecordMetricRequest(nil) neither enables the
// hook nor clears a prior hook.
func WithRecordMetricRequest(fn func(r *http.Request, status int, d time.Duration)) LogOption {
	return func(c *logConfig) {
		if fn == nil {
			return
		}
		c.recordMetricReq = fn
		c.recordMetric = nil
		c.recordRoute = nil
	}
}

// WithRecordRouteMetric registers the metric hook whose labels THIS PACKAGE
// derives, and is the recommended one: fn is invoked once per logged request
// with the bounded (method, path) label pair RouteMetricLabels computes for the
// request, plus the final status and the latency. The app never receives the
// raw request through this option, so it has no derivation to get wrong — that
// is the whole reason the option exists rather than only the function. Safety
// here is a property of the wiring, not of every consumer remembering to reach
// for the right helper: WithRecordMetricRequest's godoc records the consumer
// that reached for the wrong one, believed otherwise, and shipped an
// attacker-controllable method label.
//
// The labels are bounded BY CONSTRUCTION: the method is one of ten fixed values
// and the path is either a pattern the SERVER registered or the fixed
// "unmatched" marker. Cardinality is therefore a function of the route table,
// not of the traffic, which matters because the hook fires from the access-log
// defer — outside every app auth gate — and a series once minted is permanent
// for the process lifetime here AND in every observer scraping it. See
// RouteMetricLabels for each label's derivation and for the one deliberate
// divergence from the access line.
//
// fn takes the same RequestMetric as WithRecordMetric, so switching a consumer
// from the unbounded hook to this one changes the option name and nothing else,
// and one recording function can be passed to either.
//
// Like the other metric hooks it fires from a deferred call (a panicking
// handler is still recorded), is excluded on paths skipped via WithSkipPaths or
// WithSkipFunc, and is isolated by a recover guard (a panicking hook skips this
// request's metric rather than killing the connection). The three metric
// options are mutually exclusive; whichever is applied last wins. A nil fn is
// ignored (the package's skip-nil option convention), so a trailing
// WithRecordRouteMetric(nil) neither enables the hook nor clears a prior hook.
//
// The path policy options (WithPathFunc, WithTemplatePathsUnder,
// WithMaxLoggedPath) do NOT reach these labels: they govern the recorded path
// of the LOG LINE, while this hook's path is the matched route. A path policy
// and this hook are complementary, not alternatives.
func WithRecordRouteMetric(fn func(RequestMetric)) LogOption {
	return func(c *logConfig) {
		if fn == nil {
			return
		}
		c.recordRoute = fn
		c.recordMetric = nil
		c.recordMetricReq = nil
	}
}

// metricLabelUnmatched is the PATH label recorded for a request that matched no
// route. Every unrouted request collapses onto this one value deliberately: it
// has no route to name and its real path is caller-chosen, so a single series
// absorbs every scanner probe instead of minting one per probe. The METHOD label
// is not collapsed with it — routeMetricMethod bounds the method with no help
// from the route table, so there is nothing left for the collapse to protect.
const metricLabelUnmatched = "unmatched"

// metricLabelOther is the single bucket every non-standard method collapses
// into. A fixed bucket rather than the token itself because r.Method is
// caller-chosen text: http.ServeMux hands ANY method to a method-agnostic
// pattern — that is how such a pattern answers 405 — and measured over a real
// socket, a request line naming "M!#$%&'*+-.^_`|~" reaches a handler, as does
// one naming 300 bytes of "M". Recording that verbatim would let one
// unauthenticated caller mint series without bound. The metric says only that
// a non-standard method arrived; the access line still carries which.
const metricLabelOther = "other"

// routeMetricMethod maps a request method onto the closed set of method labels:
// the method itself when it is one of the nine standard methods, otherwise
// metricLabelOther. Ten values for any input, which is the bound — no property
// of the route table, the app's patterns, or the caller can widen it.
//
// The set is RFC 9110 §9.3's eight methods plus PATCH (RFC 5789). Everything
// else buckets, including a method that is registered but not standard
// (PROPFIND, RFC 4918), an unregistered vendor method (PURGE), and a lowercase
// spelling of a standard one: methods are case-sensitive (RFC 9110 §9.1), "get"
// is not GET, and folding it in would hand a caller a second spelling of an
// existing series.
func routeMetricMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodDelete, http.MethodConnect, http.MethodOptions,
		http.MethodTrace, http.MethodPatch:
		return m
	}
	return metricLabelOther
}

// routeMetricPath returns the path label: the template of the pattern
// http.ServeMux matched, with the method prefix ServeMux includes stripped
// (the method is its own label), or metricLabelUnmatched when nothing matched.
func routeMetricPath(r *http.Request) string {
	if r.Pattern == "" {
		return metricLabelUnmatched
	}
	if _, template, ok := strings.Cut(r.Pattern, " "); ok {
		return template
	}
	return r.Pattern
}

// RouteMetricLabels derives the bounded (method, path) label pair for a
// per-request HTTP metric. It is the pure primitive behind
// WithRecordRouteMetric, exported for any hook that already holds the request
// and wants the standard pair inside it — but the OPTION is the recommended
// path, because a helper only protects the consumers that remember to call it.
//
// The method label is r.Method when it is one of the nine standard methods —
// GET, HEAD, POST, PUT, DELETE, CONNECT, OPTIONS, TRACE (RFC 9110 §9.3) and
// PATCH (RFC 5789) — and the fixed "other" bucket for everything else. Ten
// values, by construction, whatever arrives on the request line.
//
// The path label is the route http.ServeMux matched: r.Pattern with the method
// prefix stripped ("GET /beat/{id}" records "/beat/{id}", so every beat id
// records as one series and an unknown id mints nothing), the pattern itself
// when it names no method ("/beat/{id}", a "/" catch-all), and the fixed
// "unmatched" marker when nothing matched (r.Pattern == "", e.g. net/http's own
// 404, or its 405 for a method that missed a method-bearing route). A
// host-qualified pattern ("GET example.com/beat/{id}") keeps its host — still a
// string the server registered, still bounded.
//
// Every value on both labels is therefore either a pattern the SERVER
// registered or one of eleven fixed strings this package can return (the nine
// standard method names, the "other" bucket, the "unmatched" marker), so the
// series ceiling is ten times one more than the route table and no property of
// the traffic can widen it. That matters because the hook fires from the
// access-log defer, outside every app auth gate, and a series once minted is
// permanent for the process lifetime here AND in every observer scraping it.
//
// The method comes from the REQUEST, bounded by the closed set, rather than from
// the matched pattern. Deriving it from the pattern is also bounded, but it
// makes http.ServeMux's HEAD-to-GET routing invisible: a HEAD probe against a
// GET-only pattern would record method="GET" while the access line for the same
// request records HEAD, so the log and the metric disagree about the commonest
// non-GET probe there is. Reading the closed set instead keeps them in agreement
// for all nine standard methods and still owes nothing to the route table.
//
// ONE divergence from the access line remains, deliberately: for a NON-standard
// method the line records the token verbatim (bounded to 24 bytes, see
// boundLoggedMethod's marker) while this records "other". A log line is read to
// diagnose one request and needs the real value; a metric series is read in
// aggregate and needs a bounded name. When a metric shows "other" traffic, the
// access line for the same request_id says what the method was.
//
// Caveat, inherited from the hooks: middleware between RequestLogger and the mux
// that replaces the request (r.WithContext and friends return a clone) leaves
// r.Pattern empty here, because the mux populated the clone. That reads as
// "unmatched" for every request — check it before believing a flat metric.
func RouteMetricLabels(r *http.Request) (method, path string) {
	return routeMetricMethod(r.Method), routeMetricPath(r)
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
	// The suppression test comes first, so a suppressed record costs no
	// path transform, no client-ip resolution, no level policy, and no metric
	// hook. The fact it reads was latched when the handler wrote 101 (or
	// hijacked), not now: this defer only runs when the handler returns, which
	// for a live WebSocket is when the socket closes.
	if c.skipUpgrades && rec.switchedProtocol() {
		return
	}
	d := time.Since(start)
	status := rec.Status()
	if c.pathFunc != nil {
		path = c.safeLoggedPath(r, id)
	}
	// Bounded here rather than inside each policy, so the raw default, a
	// template, a caller's transform, and the fail-closed placeholders all pass
	// through the same cap — and a policy added later cannot forget it.
	path = boundLoggedPath(path, c.maxLoggedPath)
	args := []any{
		"method", boundLoggedMethod(r.Method),
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
	if c.recordMetric != nil || c.recordMetricReq != nil || c.recordRoute != nil {
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
				"method", boundLoggedMethod(r.Method),
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
				"method", boundLoggedMethod(r.Method),
				"path", path,
				"status", status,
			)
			lvl = slog.LevelInfo
		}
	}()
	return c.logLevel(r, status)
}

// safeRecordMetric fires the caller-supplied metric hook (WithRecordRouteMetric,
// WithRecordMetricRequest or WithRecordMetric — mutual exclusion means at most
// one is set) in isolation. A panic in the hook is logged as a hook failure and
// swallowed — the metric for this request is skipped — so it cannot escape the
// outer Logging defer (which runs outside Recoverer) and turn a completed
// request into a net/http connection-closing panic.
func (c *logConfig) safeRecordMetric(r *http.Request, path string, status int, d time.Duration, id string) {
	defer func() {
		if v := recover(); v != nil {
			c.logger.Error("webhttp: metric hook failed",
				"panic", v,
				"stack", string(debug.Stack()),
				"request_id", id,
				"method", boundLoggedMethod(r.Method),
				"path", path,
				"status", status,
				"duration_ms", d.Milliseconds(),
			)
		}
	}()
	switch {
	case c.recordRoute != nil:
		// The labels are derived HERE rather than by the app, which is the
		// point of the option: the bound is a property of the wiring.
		method, route := RouteMetricLabels(r)
		c.recordRoute(RequestMetric{Method: method, Path: route, Status: status, Latency: d})
	case c.recordMetricReq != nil:
		c.recordMetricReq(r, status, d)
	default:
		c.recordMetric(RequestMetric{Method: boundLoggedMethod(r.Method), Path: path, Status: status, Latency: d})
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
// With WithPathFunc the recorded path is fn's return instead of r.URL.Path —
// the token-redaction middle ground between logging a credential-bearing path
// raw and skipping its access record entirely (fail-closed placeholder when
// the transform fails; see the option).
//
// The two attacker-controlled attributes are bounded by default, whichever
// policy produced them: the path to 512 bytes cut on a rune boundary plus a
// truncation marker (WithMaxLoggedPath re-sets the cap), and the method to 24
// bytes, over which it records a fixed "(overlong)" placeholder rather than a
// misleading prefix. Both bounds apply to the LOG only — routing, the status,
// and the response are unchanged.
//
// It records the status via a StatusRecorder that stays transparent to
// http.ResponseController, so wrapped handlers can still Flush and Hijack. An
// inbound HeaderRequestID is reused when it satisfies ValidRequestID; otherwise
// a new id is minted with NewRequestID.
//
// A request matched by WithSkipPaths or WithSkipFunc still gets an id minted,
// echoed, and threaded, but is served through the raw writer with no recorder,
// no access-log line, and no metric hook. WithSkipUpgrades removes the record of
// a request that actually SWITCHED PROTOCOLS (a completed WebSocket handshake),
// decided from the response instead of predicted from the request, so every
// refusal on the same route keeps its line; it can only remove a record, never
// restore one a skip rule dropped.
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
	if c.maxLoggedPath <= 0 {
		c.maxLoggedPath = defaultMaxLoggedPath
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
