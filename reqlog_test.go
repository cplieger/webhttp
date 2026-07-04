package webhttp_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/webhttp"
)

func TestValidRequestID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"single", "a", true},
		{"len 64", strings.Repeat("a", 64), true},
		{"len 65", strings.Repeat("a", 65), false},
		{"typical hex", "0123456789abcdef0123456789abcdef", true},
		{"underscore and hyphen", "req_id-123", true},
		{"all classes", "Aa0_-", true},
		{"space", "bad id", false},
		{"dot", "bad.id", false},
		{"slash", "a/b", false},
		{"newline", "a\nb", false},
		{"tab", "a\tb", false},
		{"colon", "a:b", false},
		{"unicode", "café", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := webhttp.ValidRequestID(tc.in); got != tc.want {
				t.Errorf("ValidRequestID(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestNewRequestID_isValidAndHex(t *testing.T) {
	id := webhttp.NewRequestID()
	if !webhttp.ValidRequestID(id) {
		t.Errorf("NewRequestID() = %q, which is not a valid request id", id)
	}
	if len(id) != 32 {
		t.Errorf("NewRequestID() length = %d, want 32 hex chars", len(id))
	}
}

func TestNewRequestID_unique(t *testing.T) {
	a, b := webhttp.NewRequestID(), webhttp.NewRequestID()
	if a == b {
		t.Errorf("two successive ids are equal: %q", a)
	}
}

func TestNewRequestID_timestampFallbackIsValidCharset(t *testing.T) {
	// NewRequestID falls back to this layout when the random source fails; the
	// invariant is that the fallback stays within the ValidRequestID charset
	// (no dot, no colon).
	ts := time.Now().UTC().Format("20060102T150405")
	if !webhttp.ValidRequestID(ts) {
		t.Errorf("fallback timestamp %q is not a valid request id", ts)
	}
	if strings.ContainsAny(ts, ".:") {
		t.Errorf("fallback timestamp %q contains a charset-invalid byte", ts)
	}
}

func TestRequestID_contextRoundTrip(t *testing.T) {
	ctx := webhttp.WithRequestID(t.Context(), "abc")
	if got := webhttp.RequestIDFromContext(ctx); got != "abc" {
		t.Errorf("RequestIDFromContext = %q, want %q", got, "abc")
	}
}

func TestRequestIDFromContext_absent(t *testing.T) {
	if got := webhttp.RequestIDFromContext(t.Context()); got != "" {
		t.Errorf("RequestIDFromContext = %q, want empty", got)
	}
}

// serve runs one request through h and returns the response recorder.
func serve(h http.Handler, method, target string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestRequestLogger_mintsIDWhenAbsent(t *testing.T) {
	var seen string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = webhttp.RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := webhttp.RequestLogger(next, webhttp.WithLogger(discardLogger()))

	rr := serve(h, http.MethodGet, "/x", nil)

	echoed := rr.Header().Get(webhttp.HeaderRequestID)
	if !webhttp.ValidRequestID(echoed) {
		t.Errorf("echoed id %q is not valid", echoed)
	}
	if seen != echoed {
		t.Errorf("context id %q != echoed header %q", seen, echoed)
	}
}

func TestRequestLogger_reusesValidInboundID(t *testing.T) {
	var seen string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = webhttp.RequestIDFromContext(r.Context())
	})
	h := webhttp.RequestLogger(next, webhttp.WithLogger(discardLogger()))

	hdr := http.Header{}
	hdr.Set(webhttp.HeaderRequestID, "inbound-123")
	rr := serve(h, http.MethodGet, "/x", hdr)

	if got := rr.Header().Get(webhttp.HeaderRequestID); got != "inbound-123" {
		t.Errorf("echoed id = %q, want inbound-123", got)
	}
	if seen != "inbound-123" {
		t.Errorf("context id = %q, want inbound-123", seen)
	}
}

func TestRequestLogger_replacesInvalidInboundID(t *testing.T) {
	var seen string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = webhttp.RequestIDFromContext(r.Context())
	})
	h := webhttp.RequestLogger(next, webhttp.WithLogger(discardLogger()))

	hdr := http.Header{}
	hdr.Set(webhttp.HeaderRequestID, "bad id!!")
	rr := serve(h, http.MethodGet, "/x", hdr)

	echoed := rr.Header().Get(webhttp.HeaderRequestID)
	if echoed == "bad id!!" {
		t.Error("invalid inbound id was reused instead of replaced")
	}
	if !webhttp.ValidRequestID(echoed) {
		t.Errorf("replacement id %q is not valid", echoed)
	}
	if seen != echoed {
		t.Errorf("context id %q != echoed header %q", seen, echoed)
	}
}

func TestRequestLogger_emitsOneInfoLine(t *testing.T) {
	logCap := &captureHandler{}
	h := webhttp.RequestLogger(statusHandler(http.StatusCreated), webhttp.WithLogger(slog.New(logCap)))

	serve(h, http.MethodPost, "/api/thing", nil)

	recs := logCap.snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d log records, want exactly 1", len(recs))
	}
	r := recs[0]
	if r.Message != "http" {
		t.Errorf("log message = %q, want %q", r.Message, "http")
	}
	if r.Level != slog.LevelInfo {
		t.Errorf("log level = %v, want Info", r.Level)
	}
	m := attrsOf(r)
	if m["method"] != http.MethodPost {
		t.Errorf("method attr = %v, want POST", m["method"])
	}
	if m["path"] != "/api/thing" {
		t.Errorf("path attr = %v, want /api/thing", m["path"])
	}
	if m["status"] != int64(http.StatusCreated) {
		t.Errorf("status attr = %v, want %d", m["status"], http.StatusCreated)
	}
	if id, ok := m["request_id"].(string); !ok || !webhttp.ValidRequestID(id) {
		t.Errorf("request_id attr = %v, want a valid id", m["request_id"])
	}
	if d, ok := m["duration_ms"].(int64); !ok || d < 0 {
		t.Errorf("duration_ms attr = %v, want a non-negative int64", m["duration_ms"])
	}
}

func TestRequestLogger_skipPathOmitsLogLineButEchoesID(t *testing.T) {
	logCap := &captureHandler{}
	h := webhttp.RequestLogger(okHandler(),
		webhttp.WithLogger(slog.New(logCap)),
		webhttp.WithSkipPaths("/stream"))

	rr := serve(h, http.MethodGet, "/stream", nil)

	if n := len(logCap.snapshot()); n != 0 {
		t.Errorf("skip path emitted %d log lines, want 0", n)
	}
	if !webhttp.ValidRequestID(rr.Header().Get(webhttp.HeaderRequestID)) {
		t.Error("skip path did not echo a valid request id")
	}
}

func TestRequestLogger_nonSkipPathStillLogs(t *testing.T) {
	logCap := &captureHandler{}
	h := webhttp.RequestLogger(okHandler(),
		webhttp.WithLogger(slog.New(logCap)),
		webhttp.WithSkipPaths("/stream"))

	serve(h, http.MethodGet, "/other", nil)

	if n := len(logCap.snapshot()); n != 1 {
		t.Errorf("non-skip path emitted %d log lines, want 1", n)
	}
}

func TestRequestLogger_metricHookOnLoggedPath(t *testing.T) {
	var (
		calls              int
		gotMethod, gotPath string
		gotStatus          int
		gotDuration        time.Duration
	)
	hook := func(method, path string, status int, d time.Duration) {
		calls++
		gotMethod, gotPath, gotStatus, gotDuration = method, path, status, d
	}
	h := webhttp.RequestLogger(statusHandler(http.StatusAccepted),
		webhttp.WithLogger(discardLogger()),
		webhttp.WithRecordMetric(hook))

	serve(h, http.MethodPut, "/metric", nil)

	if calls != 1 {
		t.Fatalf("hook called %d times, want 1", calls)
	}
	if gotMethod != http.MethodPut || gotPath != "/metric" {
		t.Errorf("hook got (%q, %q), want (PUT, /metric)", gotMethod, gotPath)
	}
	if gotStatus != http.StatusAccepted {
		t.Errorf("hook status = %d, want %d", gotStatus, http.StatusAccepted)
	}
	if gotDuration < 0 {
		t.Errorf("hook duration = %v, want non-negative", gotDuration)
	}
}

func TestRequestLogger_metricHookOnSkipPathReportsOK(t *testing.T) {
	var (
		calls     int
		gotStatus int
	)
	hook := func(_, _ string, status int, _ time.Duration) {
		calls++
		gotStatus = status
	}
	// The handler writes 418, but the skip path is served through the raw
	// writer with no recorder, so the metric reports 200 by design.
	h := webhttp.RequestLogger(statusHandler(http.StatusTeapot),
		webhttp.WithLogger(discardLogger()),
		webhttp.WithSkipPaths("/stream"),
		webhttp.WithRecordMetric(hook))

	serve(h, http.MethodGet, "/stream", nil)

	if calls != 1 {
		t.Fatalf("hook called %d times for skip path, want 1", calls)
	}
	if gotStatus != http.StatusOK {
		t.Errorf("skip-path metric status = %d, want 200 (raw writer, no recorder)", gotStatus)
	}
}

func TestRequestLogger_recorderCapturesHandlerStatus(t *testing.T) {
	logCap := &captureHandler{}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	})
	h := webhttp.RequestLogger(next, webhttp.WithLogger(slog.New(logCap)))

	rr := serve(h, http.MethodGet, "/x", nil)

	if rr.Code != http.StatusForbidden {
		t.Errorf("response code = %d, want %d", rr.Code, http.StatusForbidden)
	}
	recs := logCap.snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d log records, want 1", len(recs))
	}
	if m := attrsOf(recs[0]); m["status"] != int64(http.StatusForbidden) {
		t.Errorf("logged status = %v, want %d", m["status"], http.StatusForbidden)
	}
}

func TestRequestLogger_defaultLoggerWhenUnset(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(discardLogger())
	t.Cleanup(func() { slog.SetDefault(prev) })

	// No WithLogger option: exercises the slog.Default() fallback.
	h := webhttp.RequestLogger(okHandler())
	rr := serve(h, http.MethodGet, "/x", nil)
	if !webhttp.ValidRequestID(rr.Header().Get(webhttp.HeaderRequestID)) {
		t.Error("default-logger path did not echo a valid request id")
	}
}

func TestRequestLogger_nilOptionIgnored(t *testing.T) {
	// A nil LogOption must be skipped rather than panic.
	h := webhttp.RequestLogger(okHandler(), nil, webhttp.WithLogger(discardLogger()))
	rr := serve(h, http.MethodGet, "/x", nil)
	if !webhttp.ValidRequestID(rr.Header().Get(webhttp.HeaderRequestID)) {
		t.Error("did not echo a valid request id")
	}
}
