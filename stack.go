package webhttp

import "net/http"

// stackConfig accumulates the per-layer option groups DefaultStack forwards to
// its composed middleware.
type stackConfig struct {
	logging   []LogOption
	recoverer []RecoverOption
	security  []SecurityOption
}

// StackOption configures DefaultStack by supplying options to one of its
// composed layers. The three routers (WithLoggingOptions, WithRecovererOptions,
// WithSecurityHeadersOptions) carry each layer's own option family through
// unchanged; a nil StackOption is skipped.
type StackOption func(*stackConfig)

// WithLoggingOptions forwards LogOption values to DefaultStack's Logging layer
// (see RequestLogger for the option list, e.g. WithLogger, WithSkipPaths,
// WithRecordMetric).
func WithLoggingOptions(opts ...LogOption) StackOption {
	return func(c *stackConfig) { c.logging = append(c.logging, opts...) }
}

// WithRecovererOptions forwards RecoverOption values to DefaultStack's Recoverer
// layer (e.g. WithRecoverLogger, WithPanicHook).
func WithRecovererOptions(opts ...RecoverOption) StackOption {
	return func(c *stackConfig) { c.recoverer = append(c.recoverer, opts...) }
}

// WithSecurityHeadersOptions forwards SecurityOption values to DefaultStack's
// SecurityHeaders layer (e.g. WithCSP, WithHSTS, WithFrameOptions).
func WithSecurityHeadersOptions(opts ...SecurityOption) StackOption {
	return func(c *stackConfig) { c.security = append(c.security, opts...) }
}

// DefaultStack wraps h with the observability-safe middleware stack every
// server should have, composed in the one correct order so the common path is
// correct-by-construction. It is exactly:
//
//	Chain(h,
//		Logging(loggingOpts...),         // outermost
//		Recoverer(recoverOpts...),       // inside Logging
//		SecurityHeaders(securityOpts...), // innermost
//	)
//
// The order is load-bearing. Logging sits OUTSIDE Recoverer, so a recovered
// panic is written as its 500 before RequestLogger's deferred access line runs
// and the request logs as 500; the reversed order would log a misleading 200
// (see Recoverer). DefaultStack exists so callers do not have to remember that
// ordering.
//
// It is a convenience over the primitives, not a replacement for them. Chain,
// Logging, Recoverer, and SecurityHeaders remain the composable API for any
// stack that needs a different shape — extra middleware, a different order, a
// subset, or a per-route timeout (RouteTimeout). Configure the composed layers
// with WithLoggingOptions, WithRecovererOptions, and WithSecurityHeadersOptions;
// with no options each layer uses its own defaults (slog.Default() logging,
// nosniff + X-Frame-Options: DENY + Referrer-Policy baseline headers, no CSP).
func DefaultStack(h http.Handler, opts ...StackOption) http.Handler {
	var c stackConfig
	for _, o := range opts {
		if o != nil {
			o(&c)
		}
	}
	return Chain(h,
		Logging(c.logging...),
		Recoverer(c.recoverer...),
		SecurityHeaders(c.security...),
	)
}
