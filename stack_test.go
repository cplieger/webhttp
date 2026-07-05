package webhttp_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cplieger/webhttp"
)

// quietLogger returns a slog.Logger that discards output, so the Recoverer's
// panic Error line and the access Info line don't clutter test output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestDefaultStack_recoveredPanicLogsAs500 is the load-bearing ordering test
// (P1 completion criterion): a downstream panic must be logged as 500, not 200.
// That only holds when Logging is OUTSIDE Recoverer, which is the order
// DefaultStack composes. If the order were reversed, the deferred access line
// would run during the panic unwind and record the recorder's default 200.
func TestDefaultStack_recoveredPanicLogsAs500(t *testing.T) {
	var loggedCalls int
	var loggedStatus int
	metric := func(_, _ string, status int, _ time.Duration) {
		loggedCalls++
		loggedStatus = status
	}

	panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})
	h := webhttp.DefaultStack(panicky,
		webhttp.WithLoggingOptions(webhttp.WithLogger(quietLogger()), webhttp.WithRecordMetric(metric)),
		webhttp.WithRecovererOptions(webhttp.WithRecoverLogger(quietLogger())),
	)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("response status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if loggedCalls != 1 {
		t.Fatalf("access-log/metric fired %d times, want 1", loggedCalls)
	}
	if loggedStatus != http.StatusInternalServerError {
		t.Errorf("access-log status = %d, want %d (Logging must sit OUTSIDE Recoverer)",
			loggedStatus, http.StatusInternalServerError)
	}
}

// TestDefaultStack_appliesAllThreeLayers confirms every layer is present on the
// normal path: SecurityHeaders sets its baseline, and Logging mints+echoes the
// request id.
func TestDefaultStack_appliesAllThreeLayers(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h := webhttp.DefaultStack(ok, webhttp.WithLoggingOptions(webhttp.WithLogger(quietLogger())))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/y", nil))

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want %q (SecurityHeaders layer missing)", got, "nosniff")
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want %q", got, "DENY")
	}
	if rec.Header().Get(webhttp.HeaderRequestID) == "" {
		t.Errorf("%s not echoed (Logging layer missing)", webhttp.HeaderRequestID)
	}
}

// TestDefaultStack_securityOptionsRouted proves the per-layer option routers
// reach the intended middleware: a CSP passed via WithSecurityHeadersOptions
// lands on the response.
func TestDefaultStack_securityOptionsRouted(t *testing.T) {
	const policy = "default-src 'self'"
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h := webhttp.DefaultStack(ok,
		webhttp.WithLoggingOptions(webhttp.WithLogger(quietLogger())),
		webhttp.WithSecurityHeadersOptions(webhttp.WithCSP(policy)),
	)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/z", nil))

	if got := rec.Header().Get("Content-Security-Policy"); got != policy {
		t.Errorf("Content-Security-Policy = %q, want %q (WithSecurityHeadersOptions not routed)", got, policy)
	}
}

// TestDefaultStack_nilOptionSkipped confirms a nil StackOption is ignored rather
// than panicking.
func TestDefaultStack_nilOptionSkipped(t *testing.T) {
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h := webhttp.DefaultStack(ok, nil, webhttp.WithLoggingOptions(webhttp.WithLogger(quietLogger())))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/n", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}
