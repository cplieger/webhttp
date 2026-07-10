package webhttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	// refillPerSec 0.01 => one token every 100s, far longer than the test.
	h := RateLimiter(2, 0.01)(okHandler(&hits))

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
	h := RateLimiter(1, 0.01, WithRateLimitError("session_rate", "too many sessions"))(okHandler(&hits))

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

// TestRateLimiterWithRateLimitWhenPassesThrough verifies the predicate gate:
// only matching requests draw from the bucket; non-matching ones always pass,
// even after the bucket is empty.
func TestRateLimiterWithRateLimitWhenPassesThrough(t *testing.T) {
	hits := 0
	limited := RateLimiter(1, 0.01, WithRateLimitWhen(func(r *http.Request) bool {
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

// TestRateLimiterNonPositiveDisables confirms a non-positive burst or refill
// returns the handler unwrapped: every request passes, none is throttled.
func TestRateLimiterNonPositiveDisables(t *testing.T) {
	for _, tc := range []struct {
		name              string
		burst, refillRate float64
	}{
		{"zero burst", 0, 1},
		{"negative burst", -1, 1},
		{"zero refill", 5, 0},
		{"negative refill", 5, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits := 0
			h := RateLimiter(tc.burst, tc.refillRate)(okHandler(&hits))
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
