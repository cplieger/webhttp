// Package webhttp is server-side HTTP plumbing on top of net/http: request-id
// and bounded access logging (RequestLogger, Logging), a transparent
// StatusRecorder, composable middleware (Chain, Recoverer, SecurityHeaders,
// RouteTimeout, RateLimiter and its presets, NoStore), a spoof-aware ClientIP,
// a Host allowlist against DNS rebinding (HostPolicy, CanonicalHost), an
// embedded StaticHandler, JSON envelope helpers (WriteJSON, WriteError),
// request preludes (LimitBody, RequireMethod, DecodeBody), a constant-time
// NewStaticTokenVerifier, ClassifyBind, a readiness gate, and a graceful
// server (NewServer, Run).
//
// Middleware is func(http.Handler) http.Handler and Chain lists the outermost
// first, so Chain(mux, Logging(), Recoverer(), SecurityHeaders()) logs a
// recovered panic as its 500, and a stdlib middleware such as
// http.CrossOriginProtection's Handler composes with no adapter. The package
// depends on nothing beyond the standard library.
package webhttp
