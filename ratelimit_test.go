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
