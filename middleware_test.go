package webhttp_test

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/webhttp"
)

// recordingMW returns a middleware that appends "<name>-in" before calling next
// and "<name>-out" after, so a Chain's wrapping order is observable.
func recordingMW(log *[]string, name string) webhttp.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*log = append(*log, name+"-in")
			next.ServeHTTP(w, r)
			*log = append(*log, name+"-out")
		})
	}
}

func TestChain_firstListedIsOutermost(t *testing.T) {
	var log []string
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		log = append(log, "handler")
	})

	chained := webhttp.Chain(handler,
		recordingMW(&log, "A"),
		recordingMW(&log, "B"),
		recordingMW(&log, "C"),
	)
	serve(chained, http.MethodGet, "/", nil)

	// A is listed first, so it wraps outermost: first in, last out.
	want := []string{"A-in", "B-in", "C-in", "handler", "C-out", "B-out", "A-out"}
	if !slices.Equal(log, want) {
		t.Errorf("chain order = %v, want %v", log, want)
	}
}

func TestChain_skipsNilMiddleware(t *testing.T) {
	var log []string
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		log = append(log, "handler")
	})

	chained := webhttp.Chain(handler, nil, recordingMW(&log, "A"), nil)
	serve(chained, http.MethodGet, "/", nil)

	want := []string{"A-in", "handler", "A-out"}
	if !slices.Equal(log, want) {
		t.Errorf("chain order = %v, want %v (nil middleware must be skipped)", log, want)
	}
}

func TestChain_emptyRunsHandler(t *testing.T) {
	var called bool
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	serve(webhttp.Chain(handler), http.MethodGet, "/", nil)
	if !called {
		t.Error("Chain with no middlewares did not run the handler")
	}
}

func TestRecoverer_panicWrites500AndLogs(t *testing.T) {
	logCap := &captureHandler{}
	panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	h := webhttp.Recoverer(webhttp.WithRecoverLogger(slog.New(logCap)))(panicky)

	rr := serve(h, http.MethodGet, "/x", nil)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body webhttp.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Code != "internal_error" || body.Error != "internal server error" {
		t.Errorf("body = %+v, want code=internal_error error='internal server error'", body)
	}

	recs := logCap.snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d log records, want exactly 1", len(recs))
	}
	if recs[0].Level != slog.LevelError {
		t.Errorf("log level = %v, want Error", recs[0].Level)
	}
	m := attrsOf(recs[0])
	if m["panic"] != "boom" {
		t.Errorf("panic attr = %v, want boom", m["panic"])
	}
	if s, ok := m["stack"].(string); !ok || s == "" {
		t.Errorf("stack attr = %v, want a non-empty string", m["stack"])
	}
}

func TestRecoverer_repanicsErrAbortHandler(t *testing.T) {
	logCap := &captureHandler{}
	aborter := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})
	h := webhttp.Recoverer(webhttp.WithRecoverLogger(slog.New(logCap)))(aborter)

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		serve(h, http.MethodGet, "/x", nil)
	}()

	if recovered != http.ErrAbortHandler {
		t.Errorf("recovered %v, want http.ErrAbortHandler (it must be re-panicked)", recovered)
	}
	if n := len(logCap.snapshot()); n != 0 {
		t.Errorf("ErrAbortHandler was logged %d times, want 0 (silent abort)", n)
	}
}

func TestRecoverer_panicHookFires(t *testing.T) {
	var (
		hookCalls int
		gotVal    any
		gotStack  []byte
	)
	hook := func(v any, stack []byte) {
		hookCalls++
		gotVal, gotStack = v, stack
	}
	panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("kaboom") })
	h := webhttp.Recoverer(
		webhttp.WithRecoverLogger(discardLogger()),
		webhttp.WithPanicHook(hook),
	)(panicky)

	rr := serve(h, http.MethodGet, "/x", nil)

	if hookCalls != 1 {
		t.Fatalf("hook called %d times, want 1", hookCalls)
	}
	if gotVal != "kaboom" {
		t.Errorf("hook value = %v, want kaboom", gotVal)
	}
	if len(gotStack) == 0 {
		t.Error("hook received an empty stack")
	}
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (hook does not suppress the response)", rr.Code)
	}
}

func TestRecoverer_noPanicPassesThrough(t *testing.T) {
	logCap := &captureHandler{}
	var hookCalls int
	h := webhttp.Recoverer(
		webhttp.WithRecoverLogger(slog.New(logCap)),
		webhttp.WithPanicHook(func(any, []byte) { hookCalls++ }),
	)(statusHandler(http.StatusTeapot))

	rr := serve(h, http.MethodGet, "/x", nil)

	if rr.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d (passed through)", rr.Code, http.StatusTeapot)
	}
	if n := len(logCap.snapshot()); n != 0 {
		t.Errorf("logged %d lines without a panic, want 0", n)
	}
	if hookCalls != 0 {
		t.Errorf("hook fired %d times without a panic, want 0", hookCalls)
	}
}

func TestRecoverer_nilOptionIgnoredAndDefaultLogger(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(discardLogger())
	t.Cleanup(func() { slog.SetDefault(prev) })

	// A nil option must be skipped; with no WithRecoverLogger the slog.Default()
	// fallback is exercised.
	h := webhttp.Recoverer(nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("x")
	}))
	rr := serve(h, http.MethodGet, "/x", nil)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
}

func TestRecoverer_insideLoggingLogsStatus500(t *testing.T) {
	logCap := &captureHandler{}
	panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })

	// Logging outermost, Recoverer inside (the documented arrangement): the
	// recovered 500 is recorded before RequestLogger's deferred access line runs,
	// so the request logs as 500 rather than a misleading 200. The recoverer uses
	// a discard logger, so logCap captures only the access line.
	h := webhttp.Chain(panicky,
		webhttp.Logging(webhttp.WithLogger(slog.New(logCap))),
		webhttp.Recoverer(webhttp.WithRecoverLogger(discardLogger())),
	)

	rr := serve(h, http.MethodGet, "/x", nil)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
	recs := logCap.snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d log records, want exactly 1 (the access line)", len(recs))
	}
	if recs[0].Message != "http" {
		t.Fatalf("captured message = %q, want the access line %q", recs[0].Message, "http")
	}
	if m := attrsOf(recs[0]); m["status"] != int64(http.StatusInternalServerError) {
		t.Errorf("access line status = %v, want 500 (Recoverer inside Logging)", m["status"])
	}

	// The error body carries the request id threaded by RequestLogger.
	echoed := rr.Header().Get(webhttp.HeaderRequestID)
	var body webhttp.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.RequestID == "" || body.RequestID != echoed {
		t.Errorf("error body request_id = %q, want the echoed id %q", body.RequestID, echoed)
	}
}

func TestSecurityHeaders_defaults(t *testing.T) {
	h := webhttp.SecurityHeaders()(okHandler())
	rr := serve(h, http.MethodGet, "/", nil)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (next handler must run)", rr.Code)
	}
	defaults := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	}
	for k, want := range defaults {
		if got := rr.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	for _, k := range []string{
		"Content-Security-Policy",
		"Permissions-Policy",
		"Cross-Origin-Opener-Policy",
		"Strict-Transport-Security",
	} {
		if got := rr.Header().Get(k); got != "" {
			t.Errorf("%s = %q, want unset by default", k, got)
		}
	}
}

func TestSecurityHeaders_optionsOverride(t *testing.T) {
	h := webhttp.SecurityHeaders(
		webhttp.WithCSP("default-src 'self'"),
		webhttp.WithFrameOptions("SAMEORIGIN"),
		webhttp.WithReferrerPolicy("no-referrer"),
		webhttp.WithPermissionsPolicy("geolocation=(), camera=()"),
		webhttp.WithCOOP("same-origin"),
	)(okHandler())
	rr := serve(h, http.MethodGet, "/", nil)

	want := map[string]string{
		"X-Content-Type-Options":     "nosniff", // always on, not overridable
		"Content-Security-Policy":    "default-src 'self'",
		"X-Frame-Options":            "SAMEORIGIN",
		"Referrer-Policy":            "no-referrer",
		"Permissions-Policy":         "geolocation=(), camera=()",
		"Cross-Origin-Opener-Policy": "same-origin",
	}
	for k, v := range want {
		if got := rr.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestSecurityHeaders_emptyValueOmitsHeader(t *testing.T) {
	h := webhttp.SecurityHeaders(
		webhttp.WithFrameOptions(""),
		webhttp.WithReferrerPolicy(""),
	)(okHandler())
	rr := serve(h, http.MethodGet, "/", nil)

	if got := rr.Header().Get("X-Frame-Options"); got != "" {
		t.Errorf("X-Frame-Options = %q, want omitted when cleared", got)
	}
	if got := rr.Header().Get("Referrer-Policy"); got != "" {
		t.Errorf("Referrer-Policy = %q, want omitted when cleared", got)
	}
	// The fixed nosniff default is unaffected.
	if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestSecurityHeaders_hsts(t *testing.T) {
	cases := []struct {
		name string
		opts []webhttp.SecurityOption
		want string
	}{
		{"off by default", nil, ""},
		{
			"max-age only",
			[]webhttp.SecurityOption{webhttp.WithHSTS(24*time.Hour, false, false)},
			"max-age=86400",
		},
		{
			"includeSubDomains",
			[]webhttp.SecurityOption{webhttp.WithHSTS(24*time.Hour, true, false)},
			"max-age=86400; includeSubDomains",
		},
		{
			"includeSubDomains and preload",
			[]webhttp.SecurityOption{webhttp.WithHSTS(365*24*time.Hour, true, true)},
			"max-age=31536000; includeSubDomains; preload",
		},
		{
			"preload without subdomains",
			[]webhttp.SecurityOption{webhttp.WithHSTS(time.Hour, false, true)},
			"max-age=3600; preload",
		},
		{
			"negative max-age clamped to zero",
			[]webhttp.SecurityOption{webhttp.WithHSTS(-1*time.Hour, false, false)},
			"max-age=0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := webhttp.SecurityHeaders(tc.opts...)(okHandler())
			rr := serve(h, http.MethodGet, "/", nil)
			if got := rr.Header().Get("Strict-Transport-Security"); got != tc.want {
				t.Errorf("Strict-Transport-Security = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSecurityHeaders_nilOptionIgnored(t *testing.T) {
	h := webhttp.SecurityHeaders(nil)(okHandler())
	rr := serve(h, http.MethodGet, "/", nil)
	if got := rr.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY (nil option must be skipped)", got)
	}
}

func TestLogging_composesInChain(t *testing.T) {
	logCap := &captureHandler{}
	h := webhttp.Chain(okHandler(), webhttp.Logging(webhttp.WithLogger(slog.New(logCap))))

	rr := serve(h, http.MethodGet, "/composed", nil)

	if !webhttp.ValidRequestID(rr.Header().Get(webhttp.HeaderRequestID)) {
		t.Error("Logging in a Chain did not echo a valid request id")
	}
	recs := logCap.snapshot()
	if len(recs) != 1 || recs[0].Message != "http" {
		t.Fatalf("got %d records (msg of first: %q), want 1 access line", len(recs),
			firstMessage(recs))
	}
	if m := attrsOf(recs[0]); m["path"] != "/composed" {
		t.Errorf("path attr = %v, want /composed", m["path"])
	}
}

func TestLogging_passesOptionsThrough(t *testing.T) {
	logCap := &captureHandler{}
	h := webhttp.Chain(okHandler(), webhttp.Logging(
		webhttp.WithLogger(slog.New(logCap)),
		webhttp.WithSkipPaths("/skip"),
	))

	serve(h, http.MethodGet, "/skip", nil)

	if n := len(logCap.snapshot()); n != 0 {
		t.Errorf("skip path emitted %d lines through Logging(), want 0 (option threaded)", n)
	}
}

func TestClientIP(t *testing.T) {
	trusted := func(cidrs ...string) []*net.IPNet {
		out := make([]*net.IPNet, 0, len(cidrs))
		for _, c := range cidrs {
			_, n, err := net.ParseCIDR(c)
			if err != nil {
				t.Fatalf("bad test CIDR %q: %v", c, err)
			}
			out = append(out, n)
		}
		return out
	}

	cases := []struct {
		name       string
		remoteAddr string
		xff        string
		xRealIP    string
		trusted    []*net.IPNet
		want       string
	}{
		{"no trusted set returns peer, XFF ignored", "203.0.113.5:1234", "1.2.3.4", "", nil, "203.0.113.5"},
		{"trusted peer honors leftmost XFF", "10.0.0.1:9999", "1.2.3.4, 10.0.0.1", "", trusted("10.0.0.0/8"), "1.2.3.4"},
		{"trusted peer trims XFF whitespace", "10.0.0.1:9999", "  1.2.3.4 , 10.0.0.1", "", trusted("10.0.0.0/8"), "1.2.3.4"},
		{"trusted peer falls back to X-Real-IP", "10.0.0.1:9999", "", "5.6.7.8", trusted("10.0.0.0/8"), "5.6.7.8"},
		{"trusted peer, malformed XFF, uses X-Real-IP", "10.0.0.1:9999", "garbage", "9.9.9.9", trusted("10.0.0.0/8"), "9.9.9.9"},
		{"trusted peer, no forwarded header, uses peer", "10.0.0.1:9999", "", "", trusted("10.0.0.0/8"), "10.0.0.1"},
		{"untrusted peer ignores XFF", "203.0.113.5:1234", "1.2.3.4", "9.9.9.9", trusted("10.0.0.0/8"), "203.0.113.5"},
		{"IPv6 trusted peer honors XFF", "[::1]:8080", "2001:db8::1", "", trusted("::1/128"), "2001:db8::1"},
		{"malformed RemoteAddr, no trusted set", "not-an-addr", "1.2.3.4", "", nil, "not-an-addr"},
		{"malformed RemoteAddr with trusted set", "weird", "1.2.3.4", "", trusted("10.0.0.0/8"), "weird"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.xRealIP != "" {
				req.Header.Set("X-Real-IP", tc.xRealIP)
			}
			if got := webhttp.ClientIP(req, tc.trusted...); got != tc.want {
				t.Errorf("ClientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRouteTimeout_fastHandlerPasses(t *testing.T) {
	h := webhttp.RouteTimeout(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	}), 5*time.Second, "too slow")

	rr := serve(h, http.MethodGet, "/", nil)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	if rr.Body.String() != "hello" {
		t.Errorf("body = %q, want hello", rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain (fast handler not relabeled)", ct)
	}
}

func TestRouteTimeout_slowHandlerTimesOut(t *testing.T) {
	slow := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // unblocks when TimeoutHandler cancels the request
	})
	h := webhttp.RouteTimeout(slow, 20*time.Millisecond, "server too slow")

	rr := serve(h, http.MethodGet, "/", nil)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if ns := rr.Header().Get("X-Content-Type-Options"); ns != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", ns)
	}
	var body webhttp.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("timeout body is not JSON: %v (body=%q)", err, rr.Body.String())
	}
	if body.Code != "timeout" || body.Error != "server too slow" {
		t.Errorf("body = %+v, want code=timeout error='server too slow'", body)
	}
}

func TestRouteTimeout_emptyMsgDefaults(t *testing.T) {
	slow := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	h := webhttp.RouteTimeout(slow, 10*time.Millisecond, "")

	rr := serve(h, http.MethodGet, "/", nil)

	var body webhttp.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error != "request timed out" {
		t.Errorf("default timeout message = %q, want 'request timed out'", body.Error)
	}
	if body.Code != "timeout" {
		t.Errorf("code = %q, want timeout", body.Code)
	}
}

func TestRouteTimeout_fastHandler503KeepsItsContentType(t *testing.T) {
	// A handler that finishes in time and emits its own 503 with a Content-Type
	// must not be relabeled as JSON: the wrapper only touches an unlabeled 503.
	h := webhttp.RouteTimeout(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("<html>down</html>"))
	}), 5*time.Second, "too slow")

	rr := serve(h, http.MethodGet, "/", nil)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/html" {
		t.Errorf("Content-Type = %q, want text/html (handler's own 503 preserved)", ct)
	}
	if rr.Body.String() != "<html>down</html>" {
		t.Errorf("body = %q, want the handler's own body", rr.Body.String())
	}
}

// firstMessage returns the message of the first record, or "" when there are
// none, for clearer failure output.
func firstMessage(recs []slog.Record) string {
	if len(recs) == 0 {
		return ""
	}
	return recs[0].Message
}
