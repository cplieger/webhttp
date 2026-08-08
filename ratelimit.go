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
	when      func(*http.Request) bool
	responder ErrorResponder
	code      string
	msg       string
}

// WithRateLimitError sets the error code and message written in the 429 JSON
// envelope (via WriteError). Defaults to "rate_limited" / "rate limit
// exceeded".
func WithRateLimitError(code, msg string) RateLimitOption {
	return func(c *rateLimitConfig) {
		c.code, c.msg = code, msg
	}
}

// WithRateLimitResponder sets the ErrorResponder that writes the 429 body when a
// request is denied. It defaults to WriteError - the JSON envelope; supply one
// to render the 429 on a different content type, for example an XML endpoint
// returning its own error document. The responder owns writing the status and
// headers (the Retry-After hint is already set on w when it runs). A nil
// responder is ignored, keeping the default.
//
// It composes with WithRateLimitError rather than replacing it: the code and
// message that option sets are the ones handed to the responder, so an app keeps
// one error taxonomy across both renderings.
func WithRateLimitResponder(fn ErrorResponder) RateLimitOption {
	return func(c *rateLimitConfig) {
		if fn != nil {
			c.responder = fn
		}
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
// exceeded") (code and message overridable with WithRateLimitError, the
// rendering with WithRateLimitResponder) and does not reach the next handler.
// The 429 carries a conservative Retry-After hint: the whole seconds for the
// CURRENT token deficit to refill at the configured
// rate — at most ceil(interval) when the bucket was just emptied, less when it
// is already partially refilled — clamped to at least 1s.
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
	if c.responder == nil {
		c.responder = WriteError
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
			if ok, retryAfter := b.allow(); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				c.responder(w, r, http.StatusTooManyRequests, c.code, c.msg)
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
// token. It reads the clock under the lock and delegates the pure
// refill/consume math to allowLocked. On a deny it also computes the
// Retry-After hint from the same locked state, so the decision and the hint
// are one consistent snapshot (retryAfter is 0 on an admit).
func (b *tokenBucket) allow() (ok bool, retryAfterSecs int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.allowLocked(time.Now()) {
		return true, 0
	}
	return false, b.retryAfterSecondsLocked()
}

// allowLocked is the clock-injectable core of allow: it refills the bucket for
// the time elapsed since the last call, caps the pool at burst, consumes one
// token, and reports whether a token was available. The caller must hold b.mu.
// Taking now as a parameter keeps the refill/consume math deterministically
// testable without sleeping.
//
// A now earlier than the last observed time counts as zero elapsed and
// re-anchors the timeline at now (exactly x/time/rate's advance semantics): the
// pool can never go negative through a backwards reading, and refill resumes
// immediately on the new timeline instead of stalling until the clock
// re-passes the old anchor. The production call site reads the clock under the
// lock, so successive monotonic readings cannot go backwards; the guard makes
// the math total over its input domain anyway.
func (b *tokenBucket) allowLocked(now time.Time) bool {
	if b.last.IsZero() {
		b.tokens = b.burst
	} else {
		if now.Before(b.last) {
			b.last = now
		}
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

// retryAfterSecondsLocked returns the whole-second Retry-After hint for a
// denied request: the time for the CURRENT deficit (the fraction of a token
// still missing) to accrue at the refill rate, rounded up and clamped to at
// least 1s. Scaling to the actual deficit rather than a full token is the
// x/time/rate durationFromTokens approach: a bucket denied at 0.9 tokens hints
// the seconds to accrue 0.1, not a full interval. The hint stays conservative
// for the requesting client (rounded up, never below 1s); under contention on
// the shared bucket it is best-effort either way. The caller must hold b.mu
// so the deficit is read from the same state that produced the deny.
func (b *tokenBucket) retryAfterSecondsLocked() int {
	deficit := 1 - b.tokens
	if deficit <= 0 {
		return 1
	}
	secs := int(math.Ceil(deficit / b.refillPerSec))
	if secs < 1 {
		return 1
	}
	return secs
}

// Session-create preset tuning: a small burst absorbs a user opening several
// tabs at once; the steady one-per-second refill throttles sustained create
// churn. One home for the numbers so the web-terminal family's servers cannot
// drift apart.
const (
	sessionCreateBurst    = 6
	sessionCreateInterval = time.Second // time to accrue one create token
)

// SessionCreateRateLimit returns middleware gating POST requests to path (an
// exact match, e.g. "/api/sessions") behind the standard session-create token
// bucket: burst 6, one token accrued per second, and a 429 envelope of code
// "rate_limited" / message "session creation rate exceeded". Non-POST methods
// and other paths pass through without consuming a token, so a handler that
// multiplexes list (GET) and close (DELETE) on the same path stays
// unthrottled.
//
// This is a preset over RateLimiter for the create-a-heavy-child endpoint
// shape, where each admitted request forks an expensive process (a PTY, an
// agent subprocess) and what needs bounding is aggregate create churn. The
// tuning lives here once; an app needing different numbers or a different
// predicate composes RateLimiter directly.
func SessionCreateRateLimit(path string) Middleware {
	return RateLimiter(sessionCreateBurst, sessionCreateInterval,
		WithRateLimitWhen(func(r *http.Request) bool {
			return r.Method == http.MethodPost && r.URL.Path == path
		}),
		WithRateLimitError("rate_limited", "session creation rate exceeded"),
	)
}

// Failed-auth preset tuning: ten immediate attempts absorb an operator
// retrying a rotated credential by hand, and the 6s refill turns a guessing
// run at wire speed into ten attempts a minute. One home for the numbers so
// the services guarding the same shape cannot drift apart.
const (
	failedAuthBurst    = 10
	failedAuthInterval = 6 * time.Second // time to accrue one attempt token
)

const (
	// failedAuthCode is the 429 envelope code every failed-auth throttle
	// answers with. It is fixed rather than a parameter: the code is what an
	// operator's log queries and alert rules key on across services, and the
	// three consumers that hand-wrote this preset had already converged on this
	// exact string while their messages deliberately differ.
	failedAuthCode = "too_many_auth_failures"
	// failedAuthDefaultMsg is what an empty msg falls back to: true of any
	// credential, and deliberately silent about which one was presented.
	failedAuthDefaultMsg = "too many failed authentication attempts"
)

// FailedAuthRateLimit returns middleware that throttles FAILED-credential
// requests through the standard failed-auth token bucket: burst 10, one token
// accrued every 6 seconds, and a 429 envelope of code
// "too_many_auth_failures" carrying msg.
//
// when is the caller's failed-credential predicate, and only a request it
// reports true for draws a token. A valid credential is therefore never
// throttled, not even mid-flood, so the tuning does not have to leave room for
// the app's own legitimate senders however many there are. A nil when limits
// EVERY request the middleware sees, which is the wiring for a caller that has
// already filtered the failed-auth class itself (see the second example below).
//
// msg is the caller's human message, because the credential differs per service
// — a beat token, a bearer, an apikey — and naming it is what makes the refusal
// legible to whoever configured it. An empty msg falls back to
// "too many failed authentication attempts".
//
// What this bounds is the one-access-line-per-attempt log flood and the digest
// cost of an attempt, not the guessing rate alone: a network-exposed listener
// otherwise turns a wire-speed guessing flood into a wire-speed stream of 401s,
// one access line each. Per-client fairness is out of scope for the same reason
// RateLimiter's bucket is aggregate — a failed credential carries no trusted
// client identity to key on, and a knob here would be one more thing to
// configure wrong on a gate that must simply hold.
//
// Both wirings its consumers use stay possible, and the difference is
// deliberate:
//
//	// Predicate through the limiter (pg-autodump, seadex-scout): the
//	// middleware sees every request and the predicate decides.
//	limiter := webhttp.FailedAuthRateLimit(func(r *http.Request) bool {
//		return r.Method == http.MethodPost && r.URL.Path == "/dump" &&
//			!presentsValidBearer(verify, r)
//	}, "too many failed bearer attempts")
//
//	// Predicate outside the limiter (knell): the caller filters first and
//	// hands the limiter only the failed-auth class, so an attempt costs one
//	// fewer credential digest and the caller can attribute the 429 to its own
//	// pre-route refusal counter.
//	limited := webhttp.FailedAuthRateLimit(nil, "too many failed beat token attempts")(next)
//
// It is a preset over RateLimiter for the one-static-credential-on-one-route
// shape; an app needing other numbers or a different envelope composes
// RateLimiter directly.
func FailedAuthRateLimit(when func(*http.Request) bool, msg string) Middleware {
	if msg == "" {
		msg = failedAuthDefaultMsg
	}
	// No nil branch on when: WithRateLimitWhen ignores a nil predicate, so a
	// nil here leaves RateLimiter's default (draw a token from every request)
	// in place, which is exactly the documented meaning. Re-checking it would
	// be a second reading of the same rule, free to disagree with the first.
	return RateLimiter(failedAuthBurst, failedAuthInterval,
		WithRateLimitWhen(when),
		WithRateLimitError(failedAuthCode, msg),
	)
}
