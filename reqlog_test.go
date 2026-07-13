package webhttp_test

import (
	"log/slog"
	"net"
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
		{"crlf injection", "abc\r\nX-Evil: 1", false},
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

func TestRequestLogger_skipPathExcludedFromMetricHook(t *testing.T) {
	var calls int
	hook := func(_, _ string, _ int, _ time.Duration) { calls++ }
	// A skip path is excluded from BOTH the access log and the metric hook: a
	// stream's open-to-close duration plus a synthetic status is misleading.
	h := webhttp.RequestLogger(statusHandler(http.StatusTeapot),
		webhttp.WithLogger(discardLogger()),
		webhttp.WithSkipPaths("/stream"),
		webhttp.WithRecordMetric(hook))

	serve(h, http.MethodGet, "/stream", nil)

	if calls != 0 {
		t.Errorf("metric hook called %d times for a skip path, want 0", calls)
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

func TestRequestLogger_panicStillEmitsAccessLine(t *testing.T) {
	logCap := &captureHandler{}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})
	h := webhttp.RequestLogger(next, webhttp.WithLogger(slog.New(logCap)))

	// RequestLogger does not recover; the panic propagates out of ServeHTTP.
	// Recover it here so the test can assert the deferred access line still ran.
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("handler panic did not propagate through RequestLogger")
			}
		}()
		serve(h, http.MethodGet, "/boom", nil)
	}()

	recs := logCap.snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d log records after panic, want exactly 1", len(recs))
	}
	if recs[0].Message != "http" {
		t.Errorf("log message = %q, want %q", recs[0].Message, "http")
	}
	if m := attrsOf(recs[0]); m["status"] != int64(http.StatusOK) {
		t.Errorf("panic access line status = %v, want 200 (recorded default)", m["status"])
	}
}

func TestRequestLogger_skipFuncSuppressesLogAndMetricButEchoesID(t *testing.T) {
	logCap := &captureHandler{}
	var metricCalls int
	// A path parameter (/ws/{id}) that an exact WithSkipPaths match cannot cover.
	h := webhttp.RequestLogger(okHandler(),
		webhttp.WithLogger(slog.New(logCap)),
		webhttp.WithSkipFunc(func(r *http.Request) bool {
			return strings.HasPrefix(r.URL.Path, "/ws/")
		}),
		webhttp.WithRecordMetric(func(_, _ string, _ int, _ time.Duration) { metricCalls++ }))

	rr := serve(h, http.MethodGet, "/ws/room-42", nil)

	if n := len(logCap.snapshot()); n != 0 {
		t.Errorf("skip-func path emitted %d log lines, want 0", n)
	}
	if metricCalls != 0 {
		t.Errorf("skip-func path called the metric hook %d times, want 0", metricCalls)
	}
	if !webhttp.ValidRequestID(rr.Header().Get(webhttp.HeaderRequestID)) {
		t.Error("skip-func path did not echo a valid request id")
	}
}

func TestRequestLogger_skipFuncFalseStillLogs(t *testing.T) {
	logCap := &captureHandler{}
	h := webhttp.RequestLogger(okHandler(),
		webhttp.WithLogger(slog.New(logCap)),
		webhttp.WithSkipFunc(func(*http.Request) bool { return false }))

	serve(h, http.MethodGet, "/normal", nil)

	if n := len(logCap.snapshot()); n != 1 {
		t.Errorf("skip-func returning false emitted %d log lines, want 1", n)
	}
}

func TestRequestLogger_rejectsCRLFInjectionInboundID(t *testing.T) {
	var seen string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = webhttp.RequestIDFromContext(r.Context())
	})
	h := webhttp.RequestLogger(next, webhttp.WithLogger(discardLogger()))

	hdr := http.Header{}
	// A header-splitting / log-forging inbound id must be rejected, not echoed.
	hdr.Set(webhttp.HeaderRequestID, "abc\r\nX-Evil: 1")
	rr := serve(h, http.MethodGet, "/x", hdr)

	echoed := rr.Header().Get(webhttp.HeaderRequestID)
	if strings.ContainsAny(echoed, "\r\n") {
		t.Errorf("echoed id %q contains CR/LF; injection content was not rejected", echoed)
	}
	if !webhttp.ValidRequestID(echoed) {
		t.Errorf("echoed id %q is not a freshly minted valid id", echoed)
	}
	if seen != echoed {
		t.Errorf("context id %q != echoed header %q", seen, echoed)
	}
}

// mustCIDR parses a CIDR for a test trusted-proxy set, failing the test on a
// malformed literal.
func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", s, err)
	}
	return n
}

// serveWithPeer drives h with a request whose RemoteAddr and optional
// X-Forwarded-For are set, so the client-IP resolution can be exercised.
func serveWithPeer(h http.Handler, remoteAddr, xff string) {
	req := httptest.NewRequest(http.MethodGet, "/api/thing", nil)
	req.RemoteAddr = remoteAddr
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
}

// Without WithClientIP the access line carries no client_ip attribute (the
// default output is unchanged).
func TestRequestLogger_noClientIPAttrByDefault(t *testing.T) {
	logCap := &captureHandler{}
	h := webhttp.RequestLogger(okHandler(), webhttp.WithLogger(slog.New(logCap)))

	serveWithPeer(h, "192.0.2.1:1234", "203.0.113.5")

	recs := logCap.snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d log records, want 1", len(recs))
	}
	if _, ok := attrsOf(recs[0])["client_ip"]; ok {
		t.Error("client_ip attr present without WithClientIP; want absent")
	}
}

// With WithClientIP and no trusted ranges, the socket peer host is logged and
// an X-Forwarded-For is IGNORED (the spoof-proof default).
func TestRequestLogger_withClientIPLogsPeerByDefault(t *testing.T) {
	logCap := &captureHandler{}
	h := webhttp.RequestLogger(okHandler(),
		webhttp.WithLogger(slog.New(logCap)),
		webhttp.WithClientIP())

	// A spoofed XFF must not be honored when no proxy range is trusted.
	serveWithPeer(h, "192.0.2.1:1234", "203.0.113.5")

	m := attrsOf(logCap.snapshot()[0])
	if got := m["client_ip"]; got != "192.0.2.1" {
		t.Errorf("client_ip = %v, want the socket peer 192.0.2.1 (XFF ignored)", got)
	}
}

// With WithClientIP and the peer inside a trusted proxy range, the real client
// is resolved from X-Forwarded-For (right-to-left, skipping trusted hops).
func TestRequestLogger_withClientIPResolvesTrustedXFF(t *testing.T) {
	logCap := &captureHandler{}
	h := webhttp.RequestLogger(okHandler(),
		webhttp.WithLogger(slog.New(logCap)),
		webhttp.WithClientIP(mustCIDR(t, "192.0.2.0/24")))

	// Peer 192.0.2.1 is a trusted proxy; it appended the client it saw.
	serveWithPeer(h, "192.0.2.1:1234", "203.0.113.5")

	m := attrsOf(logCap.snapshot()[0])
	if got := m["client_ip"]; got != "203.0.113.5" {
		t.Errorf("client_ip = %v, want the forwarded client 203.0.113.5", got)
	}
}

// WithClientIPFunc logs the result of the caller-supplied resolver verbatim
// (for a dynamic/hot-reloaded trusted set), instead of the fixed-set ClientIP.
func TestRequestLogger_withClientIPFunc(t *testing.T) {
	logCap := &captureHandler{}
	h := webhttp.RequestLogger(okHandler(),
		webhttp.WithLogger(slog.New(logCap)),
		webhttp.WithClientIPFunc(func(*http.Request) string { return "resolved-by-func" }))

	serveWithPeer(h, "192.0.2.1:1234", "203.0.113.5")

	if got := attrsOf(logCap.snapshot()[0])["client_ip"]; got != "resolved-by-func" {
		t.Errorf("client_ip = %v, want the func result %q", got, "resolved-by-func")
	}
}

// WithClientIP and WithClientIPFunc are mutually exclusive; the last one applied
// wins (the earlier one's state is cleared).
func TestRequestLogger_clientIPOptionsMutuallyExclusive(t *testing.T) {
	// Func applied last → func wins.
	cap1 := &captureHandler{}
	h1 := webhttp.RequestLogger(okHandler(),
		webhttp.WithLogger(slog.New(cap1)),
		webhttp.WithClientIP(mustCIDR(t, "192.0.2.0/24")),
		webhttp.WithClientIPFunc(func(*http.Request) string { return "func-wins" }))
	serveWithPeer(h1, "192.0.2.1:1234", "203.0.113.5")
	if got := attrsOf(cap1.snapshot()[0])["client_ip"]; got != "func-wins" {
		t.Errorf("client_ip = %v, want func-wins", got)
	}

	// WithClientIP applied last → trusted-set path wins (func cleared).
	cap2 := &captureHandler{}
	h2 := webhttp.RequestLogger(okHandler(),
		webhttp.WithLogger(slog.New(cap2)),
		webhttp.WithClientIPFunc(func(*http.Request) string { return "func-loses" }),
		webhttp.WithClientIP(mustCIDR(t, "192.0.2.0/24")))
	serveWithPeer(h2, "192.0.2.1:1234", "203.0.113.5")
	if got := attrsOf(cap2.snapshot()[0])["client_ip"]; got != "203.0.113.5" {
		t.Errorf("client_ip = %v, want the trusted-XFF client 203.0.113.5", got)
	}
}

// WithClientIPFunc(nil) is a no-op (matching the package's nil-option
// convention), so no client_ip attribute is emitted.
func TestRequestLogger_withClientIPFuncNilIsNoOp(t *testing.T) {
	logCap := &captureHandler{}
	h := webhttp.RequestLogger(okHandler(),
		webhttp.WithLogger(slog.New(logCap)),
		webhttp.WithClientIPFunc(nil))

	serveWithPeer(h, "192.0.2.1:1234", "203.0.113.5")

	recs := logCap.snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d log records, want 1", len(recs))
	}
	if _, ok := attrsOf(recs[0])["client_ip"]; ok {
		t.Error("client_ip attr present after WithClientIPFunc(nil); want absent")
	}
}

// A nil WithClientIPFunc applied after WithClientIP does not clear the prior
// trusted-set resolver: the nil callback is ignored, not last-wins.
func TestRequestLogger_withClientIPFuncNilKeepsPriorTrustedSet(t *testing.T) {
	logCap := &captureHandler{}
	h := webhttp.RequestLogger(okHandler(),
		webhttp.WithLogger(slog.New(logCap)),
		webhttp.WithClientIP(mustCIDR(t, "192.0.2.0/24")),
		webhttp.WithClientIPFunc(nil))

	serveWithPeer(h, "192.0.2.1:1234", "203.0.113.5")

	if got := attrsOf(logCap.snapshot()[0])["client_ip"]; got != "203.0.113.5" {
		t.Errorf("client_ip = %v, want the trusted-XFF client 203.0.113.5 (nil func ignored)", got)
	}
}

// A panicking WithClientIPFunc resolver must not escape the outer Logging defer
// (which sits outside Recoverer): the request still completes, the access line
// is still emitted, and only the client_ip attribute is omitted.
func TestRequestLogger_panickingClientIPResolverStillEmitsAccessLine(t *testing.T) {
	logCap := &captureHandler{}
	h := webhttp.RequestLogger(okHandler(),
		webhttp.WithLogger(slog.New(logCap)),
		webhttp.WithClientIPFunc(func(*http.Request) string { panic("resolver boom") }))

	// A panic in the resolver must be contained, not propagated out of ServeHTTP.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("client-IP resolver panic escaped RequestLogger: %v", r)
			}
		}()
		serveWithPeer(h, "192.0.2.1:1234", "203.0.113.5")
	}()

	recs := logCap.snapshot()
	// Expect the resolver-failure log AND the access line.
	var access *slog.Record
	var sawFailure bool
	for i := range recs {
		switch recs[i].Message {
		case "http":
			access = &recs[i]
		case "webhttp: client_ip resolver failed":
			sawFailure = true
		}
	}
	if !sawFailure {
		t.Error("expected a 'client_ip resolver failed' log record, got none")
	}
	if access == nil {
		t.Fatal("access line was not emitted after resolver panic")
	}
	if _, ok := attrsOf(*access)["client_ip"]; ok {
		t.Error("client_ip attr present after resolver panic; want omitted")
	}
	if m := attrsOf(*access); m["status"] != int64(http.StatusOK) {
		t.Errorf("access line status = %v, want 200", m["status"])
	}
}

// A panicking WithRecordMetric hook must not escape the outer Logging defer: the
// request still completes and the access line is still emitted (the metric for
// this request is simply skipped).
func TestRequestLogger_panickingMetricHookStillEmitsAccessLine(t *testing.T) {
	logCap := &captureHandler{}
	h := webhttp.RequestLogger(statusHandler(http.StatusAccepted),
		webhttp.WithLogger(slog.New(logCap)),
		webhttp.WithRecordMetric(func(_, _ string, _ int, _ time.Duration) { panic("metric boom") }))

	// A panic in the metric hook must be contained, not propagated out of ServeHTTP.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("metric hook panic escaped RequestLogger: %v", r)
			}
		}()
		serve(h, http.MethodPut, "/metric", nil)
	}()

	recs := logCap.snapshot()
	var access *slog.Record
	var sawFailure bool
	for i := range recs {
		switch recs[i].Message {
		case "http":
			access = &recs[i]
		case "webhttp: metric hook failed":
			sawFailure = true
		}
	}
	if !sawFailure {
		t.Error("expected a 'metric hook failed' log record, got none")
	}
	if access == nil {
		t.Fatal("access line was not emitted after metric hook panic")
	}
	if m := attrsOf(*access); m["status"] != int64(http.StatusAccepted) {
		t.Errorf("access line status = %v, want %d", m["status"], http.StatusAccepted)
	}
}
