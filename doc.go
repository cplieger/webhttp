// Package webhttp provides resilient server-side HTTP plumbing for building
// small services on top of net/http.
//
// It bundles the pieces almost every server ends up hand-rolling:
//
//   - request-id injection plus one-line access logging (RequestLogger), whose
//     two attacker-controlled attributes are bounded by default — the path to
//     512 bytes on a rune boundary (WithMaxLoggedPath re-sets the cap), the
//     method to 24 bytes, beyond which it records a placeholder rather than a
//     misleading prefix — so a megabyte URL cannot buy a megabyte log line,
//     plus a metric hook whose (method, path) labels the LIBRARY derives and
//     bounds, the method onto a closed ten-value set and the path onto the
//     matched route (WithRecordRouteMetric, over the RouteMetricLabels
//     primitive), so an app cannot forget the cardinality bound,
//   - a status recorder that stays transparent to http.ResponseController and
//     also implements http.Flusher/http.Hijacker/io.ReaderFrom passthroughs, so
//     both ResponseController-based and direct-type-assertion callers (plus the
//     sendfile fast path) keep working (StatusRecorder),
//   - a composable middleware set: an ordering combinator (Chain), a panic
//     recoverer (Recoverer), baseline response security headers
//     (SecurityHeaders), access logging as middleware (Logging), a JSON
//     per-route timeout (RouteTimeout), a shared token-bucket rate
//     limiter (RateLimiter, with the SessionCreateRateLimit preset for
//     heavy-child-spawning create endpoints and the FailedAuthRateLimit preset
//     for a route guarded by one static credential), and a fixed
//     Cache-Control: no-store setter for dynamic surfaces (NoStore, whose
//     placement and override ordering stay app-owned), plus a spoof-aware
//     client-IP resolver that
//     reads X-Forwarded-For only from trusted proxy hops (ClientIP),
//   - an exact-match Host allowlist against DNS rebinding: an immutable
//     parsed policy (ParseHostList, HostPolicy) applied as middleware or
//     queried per request, built on a strict authority canonicalizer that
//     rejects malformed Host values rather than repairing them
//     (CanonicalHost), with an opt-in carve-out for loopback health checks
//     and in-container clients (WithLoopbackExempt),
//   - an embedded-static file handler with construction-time content-hash
//     ETags and precomputed gzip, with per-path cache policy left to the app
//     (StaticHandler, WithStaticCacheControl),
//   - a CSP inline-script hash extractor for pinning script-src to the exact
//     embedded page bytes instead of 'unsafe-inline' (InlineScriptHashes;
//     the policy string itself stays app-owned, passed via WithCSP),
//   - JSON response and error helpers (WriteJSON, WriteJSONStatus, Ok,
//     WriteError), whose envelope keeps the machine-readable code (ErrorCode)
//     typed apart from the human message and refuses to emit a code that is
//     not a token,
//   - request-prelude helpers for body limiting, method gating, and JSON
//     decoding (LimitBody, RequireMethod, MethodNotAllowed, DecodeBody),
//   - the request path http.ServeMux will actually route, with the verdict on
//     whether it will route the given spelling or answer a 307 no registered
//     pattern can intercept — the redirect a machine sender that does not
//     follow redirects reads as success (CanonicalRequestPath),
//   - a constant-time verifier for a single operator-configured static
//     credential — an API key, bearer token, or basic-auth field — hashing
//     the secret once at construction and comparing SHA-256 digests so no
//     per-call timing varies with the secret, failing closed on an empty
//     configured secret (NewStaticTokenVerifier),
//   - a configured-bind exposure classifier for boot-time policy — is this
//     listen address loopback-only, exposed beyond loopback, or not
//     host:port at all — with the malformed-input decision left to the app
//     (ClassifyBind, ClassifyBindHost, BindClass),
//   - an HTTP readiness gate for load balancers (Ready, ReadinessHandler),
//   - a graceful server bootstrap (NewServer, Run) with net/http's own
//     connection-level lines routed into slog at a caller-chosen level
//     (WithSlogErrorLog), a grace expiry identifiable by its origin rather
//     than by a bare deadline error (ErrShutdownGraceExpired), a bounded
//     teardown wait that cannot misreport a completion as a timeout
//     (AwaitDone), and a cause-aware predicate for telling a routine
//     cancellation apart from a coincident fault (CausedByCancellation).
//
// The middleware share the standard func(http.Handler) http.Handler shape (the
// Middleware alias) and compose with Chain, whose first-listed entry is the
// outermost wrapper. A typical stack is
// Chain(mux, Logging(), Recoverer(), SecurityHeaders()): logging outermost so a
// recovered panic is logged as its 500 rather than a misleading 200.
//
// Because Middleware is that shape and nothing more, a stdlib middleware drops
// into the same Chain with no adapter. net/http's own CSRF defence is the one
// worth naming, since a Host allowlist is only half the cross-origin story and
// this library deliberately does not wrap it:
//
//	cop := http.NewCrossOriginProtection()
//	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//		webhttp.WriteError(w, r, http.StatusForbidden, "cross_origin_denied", "cross-origin request denied")
//	}))
//	h := webhttp.Chain(mux, Logging(), Recoverer(), hostPolicy.Middleware(), cop.Handler, SecurityHeaders())
//
// cop.Handler already satisfies Middleware, and SetDenyHandler is what makes the
// refusal speak this package's error envelope — measured: a cross-site POST
// answers 403 with {"error":…,"code":"cross_origin_denied","request_id":…} and
// carries SecurityHeaders' headers, because the deny handler runs inside the
// chain. Keep the Host allowlist OUTSIDE it: after DNS rebinding the Origin and
// Host agree, so a same-origin check alone admits the request (CWE-346). A
// webhttp wrapper for this would be permanent exported surface for a capability
// a caller already has, which is the same reason there is no option for an
// http.Server field NewServer leaves alone.
//
// webhttp is the inbound-server counterpart to httpx
// (github.com/cplieger/httpx), which is the outbound-client toolkit: httpx
// makes resilient requests going OUT, webhttp handles the requests coming IN.
// The two are complementary and share no code.
//
// The package has zero dependencies beyond the standard library. It ships the
// mechanism only; each consuming application layers its own route table, error
// taxonomy, and named helpers on top.
//
// The sse subpackage (github.com/cplieger/webhttp/v2/sse) adds a broadcast hub
// for Server-Sent Events: replay ring with Last-Event-ID resume, topic
// filtering, keepalives, client caps, and a shutdown drain gate. It is the
// streaming counterpart to the request/response helpers here (RouteTimeout
// deliberately cannot wrap a stream; the sse handler owns its own deadlines).
package webhttp
