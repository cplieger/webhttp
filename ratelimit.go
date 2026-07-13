package webhttp

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RateLimitOption configures RateLimiter.
type RateLimitOption func(*rateLimitConfig)

// rateLimitConfig holds resolved RateLimiter configuration.
type rateLimitConfig struct {
	when func(*http.Request) bool
	code string
	msg  string
}

// WithRateLimitError sets the error code and message written in the 429 JSON
// envelope (via WriteError). Defaults to "rate_limited" / "rate limit
// exceeded".
func WithRateLimitError(code, msg string) RateLimitOption {
	return func(c *rateLimitConfig) {
		c.code, c.msg = code, msg
	}
}

// WithRateLimitWhen restricts limiting to requests for which pred returns true;
// every other request passes through without consuming a token. Use it to gate
// a single method+path on a handler that multiplexes several — for example,
// throttle only POST and leave GET/DELETE on the same path unthrottled. A nil
// predicate is ignored, so the default (limit every request) stands.
func WithRateLimitWhen(pred func(*http.Request) bool) RateLimitOption {
	return func(c *rateLimitConfig) {
		if pred != nil {
			c.when = pred
		}
	}
}

// RateLimiter returns middleware that throttles requests through a single
// shared token bucket (standard library only, no external dependency). burst is
// the maximum number of tokens and the initial fill; interval is the time to
// accrue one token (the refill cadence). Each admitted request consumes one
// token; a request that arrives with the bucket empty is answered with a 429
// via WriteError(w, r, http.StatusTooManyRequests, "rate_limited", "rate limit
// exceeded") (code and message overridable with WithRateLimitError) and does
// not reach the next handler. The 429 carries a conservative Retry-After hint:
// the whole seconds to accrue one token, i.e. ceil(interval), clamped to at
// least 1s.
//
// The bucket is process-wide for the middleware instance — shared across all
// requests and all clients — so it bounds the AGGREGATE rate of the wrapped
// route. That is the right tool for capping an expensive shared resource (for
// example, spawning a heavy child process per request), not for per-client
// fairness. Per-client limiting is intentionally out of scope; a caller behind
// a trusted proxy that needs it can key its own buckets on ClientIP.
//
// A non-positive burst or a non-positive interval disables limiting: the
// middleware returns the next handler unwrapped (mirroring RouteTimeout's
// non-positive "off" contract), so a config-driven zero cleanly means "no
// limit".
//
// Apply it to the specific handler you want to throttle, and pair it with
// WithRateLimitWhen to gate only the expensive method+path when the handler
// serves several:
//
//	sessions := webhttp.RateLimiter(6, time.Second,
//		webhttp.WithRateLimitWhen(func(r *http.Request) bool {
//			return r.Method == http.MethodPost
//		}),
//	)(sessionsHandler)
func RateLimiter(burst int, interval time.Duration, opts ...RateLimitOption) Middleware {
	if burst <= 0 || interval <= 0 {
		// "Off": return the handler untouched, so a zero from config means
		// "no limit" without the caller special-casing it.
		return func(next http.Handler) http.Handler { return next }
	}
	c := &rateLimitConfig{code: "rate_limited", msg: "rate limit exceeded"}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	// Convert at the seam, leaving the tokenBucket internals in refillPerSec
	// terms: interval > 0 and finite (an int64 duration) and burst >= 1, so
	// refillPerSec is always finite and positive and float64(burst) >= 1 — no
	// guard or clamp is needed.
	refillPerSec := 1 / interval.Seconds()
	b := &tokenBucket{burst: float64(burst), refillPerSec: refillPerSec}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.when != nil && !c.when(r) {
				next.ServeHTTP(w, r)
				return
			}
			if !b.allow() {
				w.Header().Set("Retry-After", strconv.Itoa(b.retryAfterSeconds()))
				WriteError(w, r, http.StatusTooManyRequests, c.code, c.msg)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// tokenBucket is a minimal mutex-guarded token bucket with no external
// dependency: a float pool that refills continuously at refillPerSec and is
// capped at burst.
type tokenBucket struct {
	last         time.Time
	tokens       float64
	burst        float64
	refillPerSec float64
	mu           sync.Mutex
}

// allow refills the bucket for the elapsed wall-clock time and consumes one
// token, returning false when none is available. It reads the clock under the
// lock and delegates the pure refill/consume math to allowLocked.
func (b *tokenBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.allowLocked(time.Now())
}

// allowLocked is the clock-injectable core of allow: it refills the bucket for
// the time elapsed since the last call, caps the pool at burst, consumes one
// token, and reports whether a token was available. The caller must hold b.mu.
// Taking now as a parameter keeps the refill/consume math deterministically
// testable without sleeping.
func (b *tokenBucket) allowLocked(now time.Time) bool {
	if b.last.IsZero() {
		b.tokens = b.burst
	} else {
		b.tokens += now.Sub(b.last).Seconds() * b.refillPerSec
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// retryAfterSeconds returns a conservative whole-second Retry-After hint: the time to
// accrue a single token at the refill rate, clamped to at least 1s.
func (b *tokenBucket) retryAfterSeconds() int {
	secs := int(math.Ceil(1 / b.refillPerSec))
	if secs < 1 {
		return 1
	}
	return secs
}
