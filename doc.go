// Package webhttp provides resilient server-side HTTP plumbing for building
// small services on top of net/http.
//
// It bundles the pieces almost every server ends up hand-rolling:
//
//   - request-id injection plus one-line access logging (RequestLogger),
//   - a status recorder that stays transparent to http.ResponseController and
//     also implements http.Flusher/http.Hijacker/io.ReaderFrom passthroughs, so
//     both ResponseController-based and direct-type-assertion callers (plus the
//     sendfile fast path) keep working (StatusRecorder),
//   - a composable middleware set: an ordering combinator (Chain) plus its
//     batteries-included correct-order convenience (DefaultStack), a panic
//     recoverer (Recoverer), baseline response security headers
//     (SecurityHeaders), access logging as middleware (Logging), a JSON
//     per-route timeout (RouteTimeout), and a shared token-bucket rate
//     limiter (RateLimiter), plus a spoof-aware client-IP resolver that
//     reads X-Forwarded-For only from trusted proxy hops (ClientIP),
//   - JSON response and error helpers (WriteJSON, WriteJSONStatus, Ok,
//     WriteError),
//   - request-prelude helpers for body limiting, method gating, and JSON
//     decoding (LimitBody, RequireMethod, DecodeBody),
//   - an HTTP readiness gate for load balancers (Ready, ReadinessHandler),
//   - a graceful server bootstrap (NewServer, Run).
//
// The middleware share the standard func(http.Handler) http.Handler shape (the
// Middleware alias) and compose with Chain, whose first-listed entry is the
// outermost wrapper. A typical stack is
// Chain(mux, Logging(), Recoverer(), SecurityHeaders()): logging outermost so a
// recovered panic is logged as its 500 rather than a misleading 200.
//
// webhttp is the inbound-server counterpart to httpx
// (github.com/cplieger/httpx), which is the outbound-client toolkit: httpx
// makes resilient requests going OUT, webhttp handles the requests coming IN.
// The two are complementary and share no code.
//
// The package has zero dependencies beyond the standard library. It ships the
// mechanism only; each consuming application layers its own route table, error
// taxonomy, and named helpers on top.
package webhttp
