package webhttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// okHandler is a trivial next handler that records how many times it ran and
// answers 200, so a test can tell an admitted request from a throttled one.
func okHandler(hits *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*hits++
		w.WriteHeader(http.StatusOK)
	})
}

// TestTokenBucketAllowLocked pins the refill/consume math against an injected
// clock: the first call fills to burst, tokens deplete one per admitted call,
// an empty bucket denies, elapsed time refills at refillPerSec, and the pool is
// capped at burst so idle time cannot bank unbounded tokens.
func TestTokenBucketAllowLocked(t *testing.T) {
	b := &tokenBucket{burst: 2, refillPerSec: 1}
	t0 := time.Unix(1_700_000_000, 0)

	// First call latches burst=2 and consumes one; the second empties it.
	if !b.allowLocked(t0) {
		t.Fatal("call 1 should be allowed (bucket fills to burst)")
	}
	if !b.allowLocked(t0) {
		t.Fatal("call 2 should be allowed (one token left)")
	}
	if b.allowLocked(t0) {
		t.Fatal("call 3 should be denied (bucket empty, no time elapsed)")
	}

	// 1s later at 1 token/s refills exactly one token: one admit, then empty.
	if !b.allowLocked(t0.Add(time.Second)) {
		t.Fatal("call after 1s should be allowed (refilled one token)")
	}
	if b.allowLocked(t0.Add(time.Second)) {
		t.Fatal("no further token should be available in the same instant")
	}

	// A long idle gap must cap at burst, not bank 100 tokens.
	for i := range 2 {
		if !b.allowLocked(t0.Add(100 * time.Second)) {
			t.Fatalf("call %d after long idle should be allowed (capped to burst)", i+1)
		}
	}
	if b.allowLocked(t0.Add(100 * time.Second)) {
		t.Fatal("bucket must cap at burst=2, not accumulate idle time unbounded")
	}
}

// TestRateLimiterAllowsBurstThenLimits fires more requests than the burst with
// a refill slow enough that none accrues during the test, so exactly burst
// requests are admitted and the rest get 429.
func TestRateLimiterAllowsBurstThenLimits(t *testing.T) {
	hits := 0
	// interval 100s => one token every 100s, far longer than the test.
	h := RateLimiter(2, 100*time.Second)(okHandler(&hits))

	codes := make([]int, 0, 5)
	for range 5 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
		codes = append(codes, rec.Code)
	}

	want := []int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests, http.StatusTooManyRequests, http.StatusTooManyRequests}
	for i, w := range want {
		if codes[i] != w {
			t.Errorf("request %d: status = %d, want %d (sequence %v)", i+1, codes[i], w, codes)
		}
	}
	if hits != 2 {
		t.Errorf("next handler ran %d times, want 2 (only admitted requests reach it)", hits)
	}
}

// TestRateLimiter429Envelope checks the throttled response is the standard
// WriteError JSON envelope and that WithRateLimitError overrides code+message.
func TestRateLimiter429Envelope(t *testing.T) {
	hits := 0
	h := RateLimiter(1, 100*time.Second, WithRateLimitError("session_rate", "too many sessions"))(okHandler(&hits))

	// Drain the single token, then trip the limit.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", nil))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var env ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not the JSON error envelope: %v (body=%q)", err, rec.Body.String())
	}
	if env.Code != "session_rate" || env.Error != "too many sessions" {
		t.Errorf("envelope = %+v, want code=session_rate error=%q", env, "too many sessions")
	}
}

// TestRateLimiter429DefaultRenderingUnchanged pins the zero-option default now
// that the rendering is a hook: with no WithRateLimitResponder the 429 is still
// the JSON WriteError envelope with the default code and message, on a JSON
// content type, alongside the Retry-After hint.
func TestRateLimiter429DefaultRenderingUnchanged(t *testing.T) {
	hits := 0
	h := RateLimiter(1, 100*time.Second)(okHandler(&hits))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", nil))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (the default rendering)", ct)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("Retry-After is empty, want a whole-second hint on the default rendering")
	}
	var env ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not the JSON error envelope: %v (body=%q)", err, rec.Body.String())
	}
	if env.Code != "rate_limited" || env.Error != "rate limit exceeded" {
		t.Errorf("envelope = %+v, want code=rate_limited error=%q", env, "rate limit exceeded")
	}
}

// TestRateLimiterCustomResponder covers WithRateLimitResponder: the responder is
// invoked with the 429 status and the configured code and message, its body is
// what reaches the client (no JSON envelope beside it), the Retry-After hint is
// already set when it runs, and an ADMITTED request never calls it.
func TestRateLimiterCustomResponder(t *testing.T) {
	var calls int
	var gotStatus int
	var gotCode ErrorCode
	var gotMsg, gotRetryAfter string
	responder := func(w http.ResponseWriter, _ *http.Request, status int, code ErrorCode, msg string) {
		calls++
		gotStatus, gotCode, gotMsg = status, code, msg
		gotRetryAfter = w.Header().Get("Retry-After")
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`<error code="` + string(code) + `">` + msg + `</error>`))
	}
	hits := 0
	h := RateLimiter(1, 100*time.Second, WithRateLimitResponder(responder))(okHandler(&hits))

	// The admitted request must not reach the responder.
	admitted := httptest.NewRecorder()
	h.ServeHTTP(admitted, httptest.NewRequest(http.MethodPost, "/x", nil))
	if admitted.Code != http.StatusOK || calls != 0 {
		t.Fatalf("admitted request: status = %d, responder calls = %d, want 200 and 0", admitted.Code, calls)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))

	if calls != 1 {
		t.Fatalf("responder called %d times on a throttled request, want 1", calls)
	}
	if gotStatus != http.StatusTooManyRequests {
		t.Errorf("responder status = %d, want 429", gotStatus)
	}
	if gotCode != "rate_limited" || gotMsg != "rate limit exceeded" {
		t.Errorf("responder args = (%q, %q), want (rate_limited, rate limit exceeded)", gotCode, gotMsg)
	}
	if gotRetryAfter == "" {
		t.Error("responder saw no Retry-After header, want the hint already set")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/xml" {
		t.Errorf("Content-Type = %q, want application/xml (custom responder)", ct)
	}
	if got, want := rec.Body.String(), `<error code="rate_limited">rate limit exceeded</error>`; got != want {
		t.Errorf("body = %q, want %q (the responder's body is what reaches the client)", got, want)
	}
	if hits != 1 {
		t.Errorf("next handler ran %d times, want 1 (a throttled request never reaches it)", hits)
	}
}

// TestRateLimiterResponderComposesWithRateLimitError pins that the two options
// stack rather than displace each other: the code and message set by
// WithRateLimitError are the ones handed to a custom responder, so an app keeps
// one error taxonomy across both renderings.
func TestRateLimiterResponderComposesWithRateLimitError(t *testing.T) {
	var gotCode ErrorCode
	var gotMsg string
	responder := func(w http.ResponseWriter, _ *http.Request, status int, code ErrorCode, msg string) {
		gotCode, gotMsg = code, msg
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(string(code) + ": " + msg))
	}
	hits := 0
	h := RateLimiter(1, 100*time.Second,
		WithRateLimitError("session_rate", "too many sessions"),
		WithRateLimitResponder(responder),
	)(okHandler(&hits))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", nil))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))

	if gotCode != "session_rate" || gotMsg != "too many sessions" {
		t.Errorf("responder args = (%q, %q), want (session_rate, too many sessions)", gotCode, gotMsg)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
	if got, want := rec.Body.String(), "session_rate: too many sessions"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestRateLimiterNilResponderKeepsJSONDefault mirrors Recoverer's nil-option
// contract: a nil responder is ignored, so the JSON WriteError default stands.
func TestRateLimiterNilResponderKeepsJSONDefault(t *testing.T) {
	hits := 0
	h := RateLimiter(1, 100*time.Second, WithRateLimitResponder(nil))(okHandler(&hits))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", nil))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (nil responder keeps the default)", ct)
	}
	var env ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not the JSON error envelope: %v (body=%q)", err, rec.Body.String())
	}
	if env.Code != "rate_limited" || env.Error != "rate limit exceeded" {
		t.Errorf("envelope = %+v, want the default rate_limited envelope", env)
	}
}

// TestRateLimiter429SetsRetryAfter pins the conservative whole-second
// Retry-After header on throttled 429s: a fractional interval rounds up (ceil),
// and a sub-second interval clamps to the 1s floor.
func TestRateLimiter429SetsRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval time.Duration
		want     string
	}{
		{"fractional interval rounds up", 2500 * time.Millisecond, "3"},
		{"sub-second interval clamps to one second", 10 * time.Millisecond, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits := 0
			h := RateLimiter(1, tc.interval)(okHandler(&hits))

			// Drain the single token, then trip the limit.
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", nil))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))

			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", rec.Code)
			}
			if got := rec.Header().Get("Retry-After"); got != tc.want {
				t.Errorf("Retry-After = %q, want %q", got, tc.want)
			}
			if hits != 1 {
				t.Errorf("next handler ran %d times, want 1 (throttled request must not reach it)", hits)
			}
		})
	}
}

// TestRateLimiterWithRateLimitWhenPassesThrough verifies the predicate gate:
// only matching requests draw from the bucket; non-matching ones always pass,
// even after the bucket is empty.
func TestRateLimiterWithRateLimitWhenPassesThrough(t *testing.T) {
	hits := 0
	limited := RateLimiter(1, 100*time.Second, WithRateLimitWhen(func(r *http.Request) bool {
		return r.Method == http.MethodPost
	}))(okHandler(&hits))

	// Empty the bucket with a POST, confirm a second POST is throttled.
	limited.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/s", nil))
	postRec := httptest.NewRecorder()
	limited.ServeHTTP(postRec, httptest.NewRequest(http.MethodPost, "/s", nil))
	if postRec.Code != http.StatusTooManyRequests {
		t.Fatalf("second POST status = %d, want 429", postRec.Code)
	}

	// GETs never match the predicate, so they pass unthrottled even now.
	for i := range 3 {
		getRec := httptest.NewRecorder()
		limited.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/s", nil))
		if getRec.Code != http.StatusOK {
			t.Errorf("GET %d status = %d, want 200 (predicate exempts it)", i+1, getRec.Code)
		}
	}
}

// TestRateLimiterNonPositiveDisables confirms a non-positive burst or interval
// returns the handler unwrapped: every request passes, none is throttled.
func TestRateLimiterNonPositiveDisables(t *testing.T) {
	for _, tc := range []struct {
		name     string
		burst    int
		interval time.Duration
	}{
		{"zero burst", 0, time.Second},
		{"negative burst", -1, time.Second},
		{"zero interval", 5, 0},
		{"negative interval", 5, -time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits := 0
			h := RateLimiter(tc.burst, tc.interval)(okHandler(&hits))
			for range 10 {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200 (limiting disabled)", rec.Code)
				}
			}
			if hits != 10 {
				t.Errorf("next handler ran %d times, want 10 (no throttling)", hits)
			}
		})
	}
}

func TestRateLimiter_concurrentAdmitsAtMostBurst(t *testing.T) {
	const burst = 50
	// The interval is far longer than the test window, so no extra token
	// accrues: the bucket holds exactly burst tokens for the whole run.
	h := RateLimiter(burst, 3*time.Hour)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	const goroutines = 200
	var admitted atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
			if rec.Code == http.StatusOK {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()

	// Under correct locking exactly burst requests win a token regardless of
	// concurrency; a dropped or wrongly-scoped lock lets two goroutines both
	// observe tokens>=1 and both decrement (check-then-act race), over-admitting.
	if got := admitted.Load(); got != burst {
		t.Errorf("admitted %d requests concurrently, want exactly burst=%d "+
			"(a dropped lock over-admits via a check-then-act race)", got, burst)
	}
}

// TestSessionCreateRateLimit pins the preset's contract: exactly the shared
// burst of POSTs to the configured path is admitted (the 7th gets the preset's
// 429 envelope with a Retry-After of the 1s refill), while non-POST methods on
// the path and POSTs to other paths never draw from the bucket.
func TestSessionCreateRateLimit(t *testing.T) {
	hits := 0
	h := SessionCreateRateLimit("/api/sessions")(okHandler(&hits))

	// The preset's burst is 6: six immediate POSTs are admitted, the seventh
	// is throttled. The 1s refill is long enough that no token accrues
	// mid-loop on any plausible test host.
	for i := range 6 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sessions", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %d status = %d, want 200 (inside burst)", i+1, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sessions", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("POST 7 status = %d, want 429 (burst exhausted)", rec.Code)
	}
	var env ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("429 body is not the JSON error envelope: %v (body=%q)", err, rec.Body.String())
	}
	if env.Code != "rate_limited" || env.Error != "session creation rate exceeded" {
		t.Errorf("envelope = %+v, want code=rate_limited error=%q", env, "session creation rate exceeded")
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want %q (1s refill)", got, "1")
	}

	// With the bucket empty, requests the predicate exempts still pass: GET
	// and DELETE on the path (list/close), and POST to any other path.
	exempt := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/sessions"},
		{http.MethodDelete, "/api/sessions/abc"},
		{http.MethodPost, "/api/other"},
	}
	for _, tc := range exempt {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s status = %d, want 200 (predicate exempts it even when the bucket is empty)", tc.method, tc.path, rec.Code)
		}
	}
}

// TestTokenBucketAllowLockedClockSkewGuard pins the out-of-order-now guard
// (x/time/rate advance semantics): a now earlier than the last observed time
// counts as zero elapsed and re-anchors the timeline at now — the pool never
// goes negative through a backwards reading, and refill resumes immediately on
// the new timeline. Production reads the clock under the lock (monotonic), so
// this is the totality guard for the injectable core.
func TestTokenBucketAllowLockedClockSkewGuard(t *testing.T) {
	b := &tokenBucket{burst: 2, refillPerSec: 1}
	t0 := time.Unix(1_700_000_000, 0)

	// Latch full and drain both tokens at t0.
	if !b.allowLocked(t0) {
		t.Fatal("first of the burst should be admitted at t0")
	}
	if !b.allowLocked(t0) {
		t.Fatal("second of the burst should be admitted at t0")
	}

	// A backwards clock reading must be a plain deny: without the guard the
	// negative elapsed would push tokens to -10 and stall recovery for 10
	// extra seconds.
	if b.allowLocked(t0.Add(-10 * time.Second)) {
		t.Fatal("backwards now must not admit")
	}
	if b.tokens < 0 {
		t.Fatalf("backwards now drove tokens negative: %v", b.tokens)
	}

	// The timeline is re-anchored at t0-10s: one second later on the NEW
	// timeline exactly one token has accrued — refill resumes immediately
	// rather than stalling until the clock re-passes the old anchor.
	if !b.allowLocked(t0.Add(-9 * time.Second)) {
		t.Fatal("1s after the re-anchor one token should have accrued")
	}
	if b.allowLocked(t0.Add(-9 * time.Second)) {
		t.Fatal("only one token can have accrued in 1s on the re-anchored timeline")
	}
}

// TestTokenBucketRetryAfterScalesToDeficit pins the deficit-scaled Retry-After
// hint (the x/time/rate durationFromTokens approach): a freshly emptied bucket
// hints the full interval, a partially refilled one hints only the remaining
// deficit, and the whole-second floor holds.
func TestTokenBucketRetryAfterScalesToDeficit(t *testing.T) {
	// interval 2.5s per token (refillPerSec = 0.4).
	b := &tokenBucket{burst: 1, refillPerSec: 0.4}
	t0 := time.Unix(1_700_000_000, 0)

	if !b.allowLocked(t0) {
		t.Fatal("first call latches the full burst and admits")
	}

	// Empty bucket, zero elapsed: deficit 1 token => ceil(2.5s) = 3.
	if b.allowLocked(t0) {
		t.Fatal("bucket should be empty")
	}
	if got := b.retryAfterSecondsLocked(); got != 3 {
		t.Errorf("full-deficit hint = %d, want 3 (ceil of the whole interval)", got)
	}

	// 1.5s later the bucket holds 0.6 tokens: deficit 0.4 => 1s, not 3.
	if b.allowLocked(t0.Add(1500 * time.Millisecond)) {
		t.Fatal("0.6 tokens must not admit")
	}
	if got := b.retryAfterSecondsLocked(); got != 1 {
		t.Errorf("partial-deficit hint = %d, want 1 (only the remaining 0.4 tokens)", got)
	}
}

// TestFailedAuthRateLimitTuning pins the preset's tuning as the three consumers
// hand-wrote it: burst 10 (ten immediate attempts admitted, the eleventh
// throttled), a 6s refill (which the deficit-scaled Retry-After reports as 6 on
// a freshly emptied bucket), and the fixed "too_many_auth_failures" envelope
// code carrying the caller's message.
func TestFailedAuthRateLimitTuning(t *testing.T) {
	hits := 0
	h := FailedAuthRateLimit(nil, "too many failed bearer attempts")(okHandler(&hits))

	for i := range 10 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/dump", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d status = %d, want 200 (inside the burst of 10)", i+1, rec.Code)
		}
	}
	if hits != 10 {
		t.Fatalf("next handler ran %d times, want 10", hits)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/dump", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt 11 status = %d, want 429 (burst exhausted)", rec.Code)
	}
	if hits != 10 {
		t.Errorf("next handler ran %d times, want 10 (a throttled attempt must not reach it)", hits)
	}
	// One whole token missing at 1/6 tokens per second rounds up to 6s, so the
	// hint is the observable proof of the refill interval.
	if got := rec.Header().Get("Retry-After"); got != "6" {
		t.Errorf("Retry-After = %q, want %q (6s refill)", got, "6")
	}
	var env ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("429 body is not the JSON error envelope: %v (body=%q)", err, rec.Body.String())
	}
	if env.Code != "too_many_auth_failures" {
		t.Errorf("envelope code = %q, want %q (the fixed cross-service code)", env.Code, "too_many_auth_failures")
	}
	if env.Error != "too many failed bearer attempts" {
		t.Errorf("envelope message = %q, want the caller's msg verbatim", env.Error)
	}
}

// TestFailedAuthRateLimitNilPredicateThrottlesEverything pins the nil-when
// wiring (knell's): with no predicate every request the middleware sees draws a
// token, whatever its method or path, because the caller has already filtered
// the failed-auth class before handing the request over.
func TestFailedAuthRateLimitNilPredicateThrottlesEverything(t *testing.T) {
	hits := 0
	h := FailedAuthRateLimit(nil, "")(okHandler(&hits))

	// Ten assorted requests drain the shared bucket: none is exempt.
	drain := []struct{ method, path string }{
		{http.MethodPost, "/beat/a"},
		{http.MethodGet, "/beat/a"},
		{http.MethodPost, "/beat/b"},
		{http.MethodDelete, "/beat/b"},
		{http.MethodPut, "/other"},
		{http.MethodPost, "/"},
		{http.MethodGet, "/metrics"},
		{http.MethodHead, "/beat/c"},
		{http.MethodPatch, "/beat/c"},
		{http.MethodOptions, "/beat/d"},
	}
	for i, tc := range drain {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s (request %d) status = %d, want 200 (inside the burst)", tc.method, tc.path, i+1, rec.Code)
		}
	}
	if hits != 10 {
		t.Fatalf("next handler ran %d times, want 10", hits)
	}

	// With the bucket empty nothing is exempt either.
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/beat/a"},
		{http.MethodGet, "/metrics"},
		{http.MethodDelete, "/anything"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("%s %s status = %d, want 429 (a nil predicate exempts nothing)", tc.method, tc.path, rec.Code)
		}
	}
}

// TestFailedAuthRateLimitPredicateThrottlesOnlyMatching pins the
// predicate-through-the-limiter wiring (pg-autodump's and seadex-scout's): only
// a request the predicate calls a failed credential draws a token, so a valid
// credential is never throttled even with the bucket empty.
func TestFailedAuthRateLimitPredicateThrottlesOnlyMatching(t *testing.T) {
	hits := 0
	failedAuth := func(r *http.Request) bool {
		return r.Method == http.MethodPost && r.URL.Path == "/dump" &&
			r.Header.Get("Authorization") != "Bearer good"
	}
	h := FailedAuthRateLimit(failedAuth, "too many failed bearer attempts")(okHandler(&hits))

	// Drain the bucket with bad-bearer POSTs to the guarded route.
	for i := range 10 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/dump", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("bad-bearer attempt %d status = %d, want 200 (inside the burst)", i+1, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/dump", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("bad-bearer attempt 11 status = %d, want 429", rec.Code)
	}

	// Everything the predicate rejects still passes: a valid bearer on the
	// guarded route (the property that lets the tuning ignore real senders),
	// another method, and another path.
	valid := httptest.NewRequest(http.MethodPost, "/dump", nil)
	valid.Header.Set("Authorization", "Bearer good")
	for _, tc := range []struct {
		name string
		r    *http.Request
	}{
		{"valid bearer on the guarded route", valid},
		{"other method", httptest.NewRequest(http.MethodGet, "/dump", nil)},
		{"other path", httptest.NewRequest(http.MethodPost, "/healthz", nil)},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, tc.r)
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200 (the predicate exempts it even when the bucket is empty)", tc.name, rec.Code)
		}
	}
}

// TestFailedAuthRateLimitMessage pins the msg contract: a caller's message is
// used verbatim and an empty one falls back to the fixed default, while the
// envelope code stays the same either way.
func TestFailedAuthRateLimitMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  string
		want string
	}{
		{"caller message used verbatim", "too many failed apikey attempts", "too many failed apikey attempts"},
		{"empty falls back to the default", "", "too many failed authentication attempts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits := 0
			h := FailedAuthRateLimit(nil, tc.msg)(okHandler(&hits))
			// Empty the burst, then read the refusal.
			for range failedAuthBurst {
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", nil))
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", rec.Code)
			}
			var env ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("429 body is not the JSON error envelope: %v (body=%q)", err, rec.Body.String())
			}
			if env.Error != tc.want {
				t.Errorf("envelope message = %q, want %q", env.Error, tc.want)
			}
			if env.Code != "too_many_auth_failures" {
				t.Errorf("envelope code = %q, want %q", env.Code, "too_many_auth_failures")
			}
		})
	}
}
