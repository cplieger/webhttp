package webhttp_test

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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

func TestRecoverer_nilPanicHookKeepsPriorHook(t *testing.T) {
	var hookCalls int
	hook := func(any, []byte) { hookCalls++ }
	panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	h := webhttp.Recoverer(
		webhttp.WithRecoverLogger(discardLogger()),
		webhttp.WithPanicHook(hook),
		webhttp.WithPanicHook(nil),
	)(panicky)

	rr := serve(h, http.MethodGet, "/x", nil)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
	if hookCalls != 1 {
		t.Errorf("hook called %d times, want 1 (nil hook option must be ignored)", hookCalls)
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

func TestRecoverer_committedResponseNotDoubleWritten(t *testing.T) {
	logCap := &captureHandler{}
	// A handler that commits a 200 with a partial body and then panics. The
	// recoverer must not write a second status or body onto the already-committed
	// response (which would corrupt the body and, under an outer Logging, mislog
	// the status), but it must still log the panic.
	panicky := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("boom after commit")
	})
	h := webhttp.Recoverer(webhttp.WithRecoverLogger(slog.New(logCap)))(panicky)

	// Observe the response through a StatusRecorder so the recorded status can be
	// asserted; Recoverer detects its Wrote() accessor and reuses it rather than
	// double-wrapping.
	rec := webhttp.NewStatusRecorder(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodGet, "/x", nil)

	// The panic must not escape Recoverer.
	func() {
		defer func() {
			if v := recover(); v != nil {
				t.Fatalf("panic escaped Recoverer: %v", v)
			}
		}()
		h.ServeHTTP(rec, req)
	}()

	if rec.Status() != http.StatusOK {
		t.Errorf("recorded status = %d, want 200 (a committed response must not be overwritten)", rec.Status())
	}
	inner, ok := rec.Unwrap().(*httptest.ResponseRecorder)
	if !ok {
		t.Fatalf("Unwrap() = %T, want *httptest.ResponseRecorder", rec.Unwrap())
	}
	if inner.Code != http.StatusOK {
		t.Errorf("underlying status = %d, want 200 (no second WriteHeader)", inner.Code)
	}
	if body := inner.Body.String(); body != "partial" {
		t.Errorf("body = %q, want %q (no 500 body appended to a committed response)", body, "partial")
	}

	// The panic is still logged even though the 500 body was skipped.
	recs := logCap.snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d log records, want exactly 1 (the recovered panic)", len(recs))
	}
	if recs[0].Level != slog.LevelError {
		t.Errorf("log level = %v, want Error", recs[0].Level)
	}
	if m := attrsOf(recs[0]); m["panic"] != "boom after commit" {
		t.Errorf("panic attr = %v, want 'boom after commit'", m["panic"])
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
		"X-Content-Type-Options":     "nosniff", // set by default (a handler could still override it)
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
		// (a) No trusted proxies: the socket peer is the only trustworthy source,
		// returned even when an X-Forwarded-For header is present. A client
		// cannot spoof its way past the default.
		{"no trusted set returns peer, XFF ignored", "203.0.113.5:1234", "1.2.3.4", "", nil, "203.0.113.5"},
		// (b) Trusted single-hop proxy: the client sent a spoofed leftmost entry
		// and the proxy appended the real peer on the right. Walking right-to-left,
		// the first untrusted entry (the appended real client) wins and the spoof
		// is ignored.
		{"trusted peer returns real right client not spoofed left", "10.0.0.1:9999", "1.2.3.4, 198.51.100.9", "", trusted("10.0.0.0/8"), "198.51.100.9"},
		// Whitespace around entries is trimmed; the appended trusted hop on the
		// right is skipped and the real client returned.
		{"trusted peer trims XFF whitespace and skips trusted hop", "10.0.0.1:9999", "  1.2.3.4 , 10.0.0.1", "", trusted("10.0.0.0/8"), "1.2.3.4"},
		// (c) Multi-hop trusted chain: the two rightmost entries are our own
		// proxies and are skipped; the leftmost untrusted entry is the client.
		{"trusted multi-hop skips trusted proxies to client", "10.0.0.1:9999", "1.2.3.4, 10.0.0.2, 10.0.0.3", "", trusted("10.0.0.0/8"), "1.2.3.4"},
		// A malformed entry at the right boundary stops the walk: nothing further
		// left can be trusted, so the peer is returned.
		{"trusted peer, malformed XFF boundary, returns peer", "10.0.0.1:9999", "garbage", "", trusted("10.0.0.0/8"), "10.0.0.1"},
		// Trusted peer but no forwarded header: the peer is the client.
		{"trusted peer, no forwarded header, uses peer", "10.0.0.1:9999", "", "", trusted("10.0.0.0/8"), "10.0.0.1"},
		// X-Real-IP is no longer consulted (removed as a client-settable spoof
		// vector): a trusted peer with only X-Real-IP set still returns the peer.
		{"trusted peer ignores removed X-Real-IP", "10.0.0.1:9999", "", "5.6.7.8", trusted("10.0.0.0/8"), "10.0.0.1"},
		// (d) Untrusted direct peer: forwarded headers are attacker-controlled and
		// ignored, including a spoofed XFF and X-Real-IP.
		{"untrusted peer ignores XFF and X-Real-IP", "203.0.113.5:1234", "1.2.3.4", "9.9.9.9", trusted("10.0.0.0/8"), "203.0.113.5"},
		// (e) IPv6 trusted peer parsed from a bracketed host:port; the untrusted
		// XFF entry is the client.
		{"IPv6 trusted peer honors XFF", "[::1]:8080", "2001:db8::1", "", trusted("::1/128"), "2001:db8::1"},
		// (e) Malformed RemoteAddr with no port is used verbatim; an unparseable
		// peer can never be trusted, so XFF is ignored with or without a set.
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

// TestClientIP_multiLineXFF proves ClientIP treats multiple X-Forwarded-For
// header LINES as the single comma-joined value RFC 7230 defines, rather than
// reading only the first line (the old Header.Get behavior). A client sends a
// spoofed first XFF line; a trusted proxy that appends the peer it observed as
// a SEPARATE header line (instead of comma-appending) must not let the spoofed
// first line win. The right-to-left walk must still select the real appended
// entry, closing the spoof gap.
func TestClientIP_multiLineXFF(t *testing.T) {
	_, trusted, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatalf("bad test CIDR: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:9999" // immediate peer is a trusted proxy
	// Two separate X-Forwarded-For header lines: the client's spoofed value
	// first, then the real client the trusted proxy observed and appended.
	req.Header.Add("X-Forwarded-For", "1.2.3.4")      // spoofed by the client
	req.Header.Add("X-Forwarded-For", "198.51.100.9") // real peer, added by proxy
	if got := webhttp.ClientIP(req, trusted); got != "198.51.100.9" {
		t.Errorf("ClientIP() with multi-line XFF = %q, want 198.51.100.9 (rightmost untrusted entry, not the spoofed first line)", got)
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

func TestRouteTimeout_fastHandler503WithoutContentTypeIsRelabeled(t *testing.T) {
	// A handler that finishes in time but emits a 503 with NO Content-Type is
	// indistinguishable from a genuine timeout envelope, so the wrapper relabels
	// it application/json (+ nosniff). This documents and locks in that behavior;
	// a handler that wants to avoid it must set its own Content-Type (see the
	// sibling test that emits text/html).
	h := webhttp.RouteTimeout(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("nope"))
	}), 5*time.Second, "too slow")

	rr := serve(h, http.MethodGet, "/", nil)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (an unlabeled 503 is relabeled)", ct)
	}
	if ns := rr.Header().Get("X-Content-Type-Options"); ns != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", ns)
	}
	// Only the headers are added; the handler's own body bytes are unchanged.
	if rr.Body.String() != "nope" {
		t.Errorf("body = %q, want the handler's own body %q", rr.Body.String(), "nope")
	}
}

func TestRouteTimeout_nonPositiveDurationReturnsHandlerUnwrapped(t *testing.T) {
	// A non-positive timeout disables the wrapper: the original handler is
	// returned unchanged, so its response passes through untouched (no JSON
	// relabel, no 503, no buffering).
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("direct"))
	})
	for _, d := range []time.Duration{0, -time.Second} {
		h := webhttp.RouteTimeout(base, d, "unused")
		rr := serve(h, http.MethodGet, "/", nil)
		if rr.Code != http.StatusOK {
			t.Errorf("d=%v: status = %d, want 200", d, rr.Code)
		}
		if ct := rr.Header().Get("Content-Type"); ct != "text/plain" {
			t.Errorf("d=%v: Content-Type = %q, want text/plain (handler returned unwrapped)", d, ct)
		}
		if rr.Body.String() != "direct" {
			t.Errorf("d=%v: body = %q, want direct", d, rr.Body.String())
		}
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

// WriteError is the canonical ErrorResponder and the Recoverer default; this
// assertion locks that the default's signature matches the exported type.
var _ webhttp.ErrorResponder = webhttp.WriteError

func TestRecoverer_customResponderRendersNonJSON(t *testing.T) {
	logCap := &captureHandler{}
	// A non-JSON endpoint supplies an ErrorResponder that writes its own content
	// type and body; Recoverer must use it for the 500 in place of the JSON default.
	responder := func(w http.ResponseWriter, _ *http.Request, status int, code, msg string) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`<error code="` + code + `">` + msg + `</error>`))
	}
	panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	h := webhttp.Recoverer(
		webhttp.WithRecoverLogger(slog.New(logCap)),
		webhttp.WithRecoverResponder(responder),
	)(panicky)

	rr := serve(h, http.MethodGet, "/x", nil)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/xml" {
		t.Errorf("Content-Type = %q, want application/xml (custom responder)", ct)
	}
	if got, want := rr.Body.String(), `<error code="internal_error">internal server error</error>`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	// The panic is still logged regardless of the responder.
	if recs := logCap.snapshot(); len(recs) != 1 || recs[0].Level != slog.LevelError {
		t.Errorf("got %d records (want exactly 1 at Error) for the recovered panic", len(recs))
	}
}

func TestRecoverer_nilResponderKeepsJSONDefault(t *testing.T) {
	// A nil responder is ignored, so the JSON WriteError default stands.
	panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	h := webhttp.Recoverer(
		webhttp.WithRecoverLogger(discardLogger()),
		webhttp.WithRecoverResponder(nil),
	)(panicky)

	rr := serve(h, http.MethodGet, "/x", nil)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (nil responder keeps the default)", ct)
	}
	var body webhttp.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Code != "internal_error" || body.Error != "internal server error" {
		t.Errorf("body = %+v, want the default internal_error envelope", body)
	}
}

func TestRecoverer_customResponderNotCalledOnCommittedResponse(t *testing.T) {
	// The commit gate applies to a custom responder too: a handler that commits
	// then panics must not have the responder write a second status or body.
	var responderCalls int
	responder := func(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
		responderCalls++
		webhttp.WriteError(w, r, status, code, msg)
	}
	panicky := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("boom after commit")
	})
	h := webhttp.Recoverer(
		webhttp.WithRecoverLogger(discardLogger()),
		webhttp.WithRecoverResponder(responder),
	)(panicky)

	rr := serve(h, http.MethodGet, "/x", nil)

	if responderCalls != 0 {
		t.Errorf("responder called %d times on a committed response, want 0", responderCalls)
	}
	if rr.Body.String() != "partial" {
		t.Errorf("body = %q, want %q (committed response left untouched)", rr.Body.String(), "partial")
	}
}

func TestRecoverer_panicHookPanicIsIsolated(t *testing.T) {
	logCap := &captureHandler{}
	// The caller's panic hook itself panics. That secondary failure must be
	// isolated: logged with the original request id, and recovery continues so
	// the client still receives the 500 rather than an aborted connection.
	panicHook := func(any, []byte) { panic("hook boom") }
	panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })

	// Logging outermost so the deferred access line's recorded status can be
	// asserted; both the recoverer and the logger write into logCap, so records
	// are filtered by message.
	h := webhttp.Chain(panicky,
		webhttp.Logging(webhttp.WithLogger(slog.New(logCap))),
		webhttp.Recoverer(
			webhttp.WithRecoverLogger(slog.New(logCap)),
			webhttp.WithPanicHook(panicHook),
		),
	)

	rr := serve(h, http.MethodGet, "/x", nil)

	// The client still receives the JSON 500 despite the hook panicking.
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (a hook panic must not abort the 500)", rr.Code)
	}
	var body webhttp.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v (body=%q)", err, rr.Body.String())
	}
	if body.Code != "internal_error" || body.Error != "internal server error" {
		t.Errorf("body = %+v, want the default internal_error envelope", body)
	}

	// The secondary hook failure is logged with the original request id, and the
	// outer access line records the 500 the client received.
	var accessStatus any
	var hookFailID, recoveredID string
	sawHookFail := false
	for _, rec := range logCap.snapshot() {
		m := attrsOf(rec)
		switch rec.Message {
		case "http":
			accessStatus = m["status"]
		case "webhttp: panic hook failed":
			sawHookFail = true
			hookFailID, _ = m["request_id"].(string)
			if m["panic"] != "hook boom" {
				t.Errorf("hook-failure panic attr = %v, want 'hook boom'", m["panic"])
			}
		case "webhttp: recovered from panic":
			recoveredID, _ = m["request_id"].(string)
		}
	}
	if !sawHookFail {
		t.Fatal("no 'webhttp: panic hook failed' log record; the hook panic was not isolated")
	}
	if accessStatus != int64(http.StatusInternalServerError) {
		t.Errorf("access line status = %v, want 500 (Recoverer inside Logging)", accessStatus)
	}
	echoed := rr.Header().Get(webhttp.HeaderRequestID)
	if hookFailID == "" || hookFailID != recoveredID || hookFailID != echoed {
		t.Errorf("hook-failure request_id = %q, want the recovered/echoed id (recovered=%q echoed=%q)",
			hookFailID, recoveredID, echoed)
	}
}

func TestRecoverer_responderPanicBeforeCommitFallsBackToJSON500(t *testing.T) {
	logCap := &captureHandler{}
	// A custom responder that panics BEFORE writing any header or body
	// (pre-commit). The response is still uncommitted, so the recovery must fall
	// back to the default JSON 500 and the client still receives a 500.
	responder := func(http.ResponseWriter, *http.Request, int, string, string) {
		panic("responder boom")
	}
	panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })

	h := webhttp.Chain(panicky,
		webhttp.Logging(webhttp.WithLogger(slog.New(logCap))),
		webhttp.Recoverer(
			webhttp.WithRecoverLogger(slog.New(logCap)),
			webhttp.WithRecoverResponder(responder),
		),
	)

	rr := serve(h, http.MethodGet, "/x", nil)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (pre-commit responder panic falls back to JSON 500)", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (fallback default responder)", ct)
	}
	var body webhttp.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v (body=%q)", err, rr.Body.String())
	}
	if body.Code != "internal_error" || body.Error != "internal server error" {
		t.Errorf("body = %+v, want the default internal_error envelope", body)
	}

	// The secondary responder failure is logged with the original request id, and
	// the outer access line records the 500 the client received.
	var accessStatus any
	var responderFailID, recoveredID string
	sawResponderFail := false
	for _, rec := range logCap.snapshot() {
		m := attrsOf(rec)
		switch rec.Message {
		case "http":
			accessStatus = m["status"]
		case "webhttp: recover responder failed":
			sawResponderFail = true
			responderFailID, _ = m["request_id"].(string)
			if m["panic"] != "responder boom" {
				t.Errorf("responder-failure panic attr = %v, want 'responder boom'", m["panic"])
			}
		case "webhttp: recovered from panic":
			recoveredID, _ = m["request_id"].(string)
		}
	}
	if !sawResponderFail {
		t.Fatal("no 'webhttp: recover responder failed' log record; the responder panic was not isolated")
	}
	if accessStatus != int64(http.StatusInternalServerError) {
		t.Errorf("access line status = %v, want 500 (Recoverer inside Logging)", accessStatus)
	}
	echoed := rr.Header().Get(webhttp.HeaderRequestID)
	if responderFailID == "" || responderFailID != recoveredID || responderFailID != echoed {
		t.Errorf("responder-failure request_id = %q, want the recovered/echoed id (recovered=%q echoed=%q)",
			responderFailID, recoveredID, echoed)
	}
}

func TestRouteTimeout_envelopeCarriesContextRequestID(t *testing.T) {
	slow := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	h := webhttp.RouteTimeout(slow, 20*time.Millisecond, "server too slow")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(webhttp.WithRequestID(req.Context(), "corr-42"))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	var body webhttp.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("timeout body is not JSON: %v (body=%q)", err, rr.Body.String())
	}
	if body.RequestID != "corr-42" {
		t.Errorf("timeout envelope request_id = %q, want %q", body.RequestID, "corr-42")
	}
	if body.Code != "timeout" || body.Error != "server too slow" {
		t.Errorf("body = %+v, want code=timeout error='server too slow'", body)
	}
}

func TestRouteTimeout_envelopeOmitsRequestIDWithoutContext(t *testing.T) {
	// Without an id in the request context (no RequestLogger upstream) the
	// envelope omits the field entirely (omitempty wire shape), matching
	// WriteError's behavior for an id-less request.
	slow := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	h := webhttp.RouteTimeout(slow, 10*time.Millisecond, "")

	rr := serve(h, http.MethodGet, "/", nil)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "request_id") {
		t.Errorf("id-less timeout envelope contains request_id: %q", rr.Body.String())
	}
}

func TestRouteTimeout_underRequestLoggerCorrelatesHeaderAndBody(t *testing.T) {
	// The universal-correlation proof: composed under RequestLogger, the minted
	// id on the X-Request-ID response header and the request_id in the timeout
	// envelope are the same id.
	slow := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	h := webhttp.RequestLogger(
		webhttp.RouteTimeout(slow, 20*time.Millisecond, "server too slow"),
		webhttp.WithLogger(discardLogger()))

	rr := serve(h, http.MethodGet, "/", nil)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	id := rr.Header().Get(webhttp.HeaderRequestID)
	if !webhttp.ValidRequestID(id) {
		t.Fatalf("echoed header id %q is not valid", id)
	}
	var body webhttp.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("timeout body is not JSON: %v (body=%q)", err, rr.Body.String())
	}
	if body.RequestID != id {
		t.Errorf("envelope request_id = %q, header id = %q; want them equal", body.RequestID, id)
	}
}
