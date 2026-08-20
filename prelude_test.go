package webhttp_test

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/webhttp/v2"
)

type payload struct {
	Name string `json:"name"`
}

func TestRequireMethod_match(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	if !webhttp.RequireMethod(rr, req, http.MethodPost) {
		t.Error("RequireMethod = false, want true on match")
	}
	if rr.Body.Len() != 0 {
		t.Errorf("wrote a body on match: %s", rr.Body.String())
	}
}

func TestRequireMethod_mismatch(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if webhttp.RequireMethod(rr, req, http.MethodPost) {
		t.Error("RequireMethod = true, want false on mismatch")
	}
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("code = %d, want 405", rr.Code)
	}
	// RFC 9110 requires a 405 to carry an Allow header naming the permitted method.
	if allow := rr.Header().Get("Allow"); allow != http.MethodPost {
		t.Errorf("Allow header = %q, want %q", allow, http.MethodPost)
	}
	var got webhttp.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Code != "method_not_allowed" {
		t.Errorf("code = %q, want method_not_allowed", got.Code)
	}
}

// RequireMethod is built on MethodNotAllowed, which renders a LIST. This pins
// the single-method response byte for byte — header value, status, and raw body
// — so the multi-method rendering can never change what today's single-method
// callers (subflux's RequirePOST/RequireGET, vibekit's requirePOST) emit.
func TestRequireMethod_singleMethodResponseIsByteIdentical(t *testing.T) {
	const wantBody = `{"error":"method not allowed","code":"method_not_allowed"}` + "\n"
	cases := []struct {
		name     string
		required string
	}{
		{"post", http.MethodPost},
		{"get", http.MethodGet},
		{"delete", http.MethodDelete},
		// A method token is case-sensitive (RFC 9110 9.1): the header echoes
		// exactly what the caller passed, never a canonicalized spelling.
		{"lowercase verbatim", "post"},
		// Garbage in: an empty required method still emits the header, with the
		// empty value the spec defines as "no methods allowed".
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodOptions, "/", nil)
			if webhttp.RequireMethod(rr, req, tc.required) {
				t.Fatalf("RequireMethod = true, want false (OPTIONS vs %q)", tc.required)
			}
			allow, ok := rr.Header()["Allow"]
			if !ok {
				t.Fatal("no Allow header on the 405 (RFC 9110 makes it mandatory)")
			}
			if len(allow) != 1 || allow[0] != tc.required {
				t.Errorf("Allow = %q, want exactly [%q]", allow, tc.required)
			}
			if rr.Code != http.StatusMethodNotAllowed {
				t.Errorf("code = %d, want 405", rr.Code)
			}
			if got := rr.Body.String(); got != wantBody {
				t.Errorf("body = %q, want %q", got, wantBody)
			}
			if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if nosniff := rr.Header().Get("X-Content-Type-Options"); nosniff != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", nosniff)
			}
		})
	}
}

// MethodNotAllowed is the rejection half a multi-method route needs: the 405
// names every permitted method, which a single-method guard cannot express.
func TestMethodNotAllowed_multiMethodAllow(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/beat/x", nil)
	webhttp.MethodNotAllowed(rr, req, http.MethodGet, http.MethodPost)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("code = %d, want 405", rr.Code)
	}
	if allow := rr.Header().Get("Allow"); allow != "GET, POST" {
		t.Errorf("Allow = %q, want %q", allow, "GET, POST")
	}
	var got webhttp.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Code != "method_not_allowed" || got.Error != "method not allowed" {
		t.Errorf("body = %+v, want the standard method_not_allowed envelope", got)
	}
}

// An Allow header is mandatory on a 405 even when nothing is permitted: RFC
// 9110 10.2.1 defines the empty field value as "the resource allows no
// methods". Omitting the header entirely would violate the MUST.
func TestMethodNotAllowed_noMethodsStillSetsEmptyAllow(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/disabled", nil)
	webhttp.MethodNotAllowed(rr, req)

	values, ok := rr.Header()["Allow"]
	if !ok {
		t.Fatal("Allow header absent; RFC 9110 requires it on every 405")
	}
	if len(values) != 1 || values[0] != "" {
		t.Errorf("Allow = %q, want exactly one empty value", values)
	}
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("code = %d, want 405", rr.Code)
	}
}

func TestSetAllow(t *testing.T) {
	cases := []struct {
		name    string
		allowed []string
		want    string
	}{
		{"single", []string{http.MethodPost}, "POST"},
		{"pair joined with comma space", []string{http.MethodGet, http.MethodPost}, "GET, POST"},
		{"caller order preserved", []string{http.MethodPost, http.MethodGet}, "POST, GET"},
		{"three", []string{http.MethodGet, http.MethodPut, http.MethodDelete}, "GET, PUT, DELETE"},
		// A sender must not generate an empty list element (RFC 9110 5.6.1.1),
		// so a blank from a config-built set is dropped rather than rendered.
		{"empty entries dropped", []string{http.MethodGet, "", http.MethodPost}, "GET, POST"},
		{"only empty entries", []string{"", ""}, ""},
		// The field names a set, so a doubled entry collapses.
		{"exact duplicates collapsed", []string{http.MethodGet, http.MethodGet, http.MethodPost}, "GET, POST"},
		// Case-sensitive (RFC 9110 9.1): distinct spellings are distinct tokens.
		{"case is not folded", []string{"GET", "get"}, "GET, get"},
		{"none", nil, ""},
		// HEAD is never implied by GET; the route advertises what it serves.
		{"head not implied", []string{http.MethodGet}, "GET"},
		{"head when explicit", []string{http.MethodGet, http.MethodHead}, "GET, HEAD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			webhttp.SetAllow(rr, tc.allowed...)
			values, ok := rr.Header()["Allow"]
			if !ok {
				t.Fatal("SetAllow did not set the Allow header")
			}
			if len(values) != 1 || values[0] != tc.want {
				t.Errorf("Allow = %q, want exactly [%q]", values, tc.want)
			}
		})
	}
}

// SetAllow replaces, never appends: a route that re-advertises its set (a
// middleware and the handler both naming it) must not emit two Allow fields.
func TestSetAllow_replacesAnyExistingValue(t *testing.T) {
	rr := httptest.NewRecorder()
	rr.Header().Set("Allow", "PATCH")
	webhttp.SetAllow(rr, http.MethodGet, http.MethodPost)
	if values := rr.Header()["Allow"]; len(values) != 1 || values[0] != "GET, POST" {
		t.Errorf("Allow = %q, want exactly [%q]", values, "GET, POST")
	}
}

func TestDecodeBody_rejectsTrailingData(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"second json value", `{"a":1}{"b":2}`},
		{"trailing junk", `{"a":1} junk`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			var p payload
			if webhttp.DecodeBody(rr, req, &p, "exactly one value") {
				t.Error("DecodeBody = true, want false for trailing data after the first value")
			}
			if rr.Code != http.StatusBadRequest {
				t.Errorf("code = %d, want 400", rr.Code)
			}
			var got webhttp.ErrorResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.Code != "bad_request" || got.Error != "exactly one value" {
				t.Errorf("body = %+v, want code=bad_request error='exactly one value'", got)
			}
		})
	}
}

func TestDecodeBody_success(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"neo"}`))
	var p payload
	if !webhttp.DecodeBody(rr, req, &p, "bad body") {
		t.Fatal("DecodeBody = false, want true")
	}
	if p.Name != "neo" {
		t.Errorf("Name = %q, want neo", p.Name)
	}
}

func TestDecodeBody_invalidJSON(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{not json`))
	var p payload
	if webhttp.DecodeBody(rr, req, &p, "bad body") {
		t.Error("DecodeBody = true, want false on invalid JSON")
	}
	if rr.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", rr.Code)
	}
	var got webhttp.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Code != "bad_request" || got.Error != "bad body" {
		t.Errorf("body = %+v, want code=bad_request error='bad body'", got)
	}
}

func TestDecodeBody_tooLarge(t *testing.T) {
	big := `{"name":"` + strings.Repeat("x", int(webhttp.MaxJSONBody)) + `"}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(big))
	var p payload
	if webhttp.DecodeBody(rr, req, &p, "too big") {
		t.Error("DecodeBody = true, want false for oversize body")
	}
	if rr.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", rr.Code)
	}
}

func TestDecodeBodyOptional_valid(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"trinity"}`))
	var p payload
	webhttp.DecodeBodyOptional(rr, req, &p)
	if p.Name != "trinity" {
		t.Errorf("Name = %q, want trinity", p.Name)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("optional decode wrote a response: %s", rr.Body.String())
	}
}

func TestDecodeBodyOptional_invalidIgnored(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`garbage`))
	var p payload
	webhttp.DecodeBodyOptional(rr, req, &p) // must not panic or write
	if p.Name != "" {
		t.Errorf("Name = %q, want zero value", p.Name)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("optional decode wrote a response: %s", rr.Body.String())
	}
}

func TestLimitBody_readPastLimitErrors(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("0123456789"))
	webhttp.LimitBody(rr, req, 4)
	if _, err := io.ReadAll(req.Body); err == nil {
		t.Error("reading past the limit did not error")
	}
}

func TestLimitBody_withinLimitReadsFully(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("abc"))
	webhttp.LimitBody(rr, req, 16)
	b, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(b) != "abc" {
		t.Errorf("read %q, want abc", b)
	}
}

// DecodeJSONInto is the mechanism behind DecodeBody, exposed for apps with their
// own error taxonomy / cap: it writes NOTHING and returns a typed error.
func TestDecodeJSONInto_success(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"morpheus"}`))
	var p payload
	if err := webhttp.DecodeJSONInto(rr, req, &p, webhttp.MaxJSONBody); err != nil {
		t.Fatalf("DecodeJSONInto err = %v, want nil", err)
	}
	if p.Name != "morpheus" {
		t.Errorf("Name = %q, want morpheus", p.Name)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("DecodeJSONInto wrote a response: %s", rr.Body.String())
	}
}

func TestDecodeJSONInto_malformedReturnsError(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{not json`))
	var p payload
	err := webhttp.DecodeJSONInto(rr, req, &p, webhttp.MaxJSONBody)
	if err == nil {
		t.Fatal("DecodeJSONInto err = nil, want a decode error")
	}
	if rr.Body.Len() != 0 {
		t.Errorf("DecodeJSONInto wrote a response on error: %s", rr.Body.String())
	}
}

func TestDecodeJSONInto_trailingDataIsErrTrailingData(t *testing.T) {
	// Any well-formed second JSON value — object, scalar, array, or null —
	// following the first must classify as ErrTrailingData, not a json type
	// error, so a consumer's errors.Is(err, ErrTrailingData) check is reliable.
	cases := []struct {
		name string
		body string
	}{
		{"object", `{"name":"a"}{"name":"b"}`},
		{"scalar", `{"name":"a"}42`},
		{"array", `{"name":"a"}[1]`},
		{"null", `{"name":"a"}null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			var p payload
			err := webhttp.DecodeJSONInto(rr, req, &p, webhttp.MaxJSONBody)
			if !errors.Is(err, webhttp.ErrTrailingData) {
				t.Fatalf("err = %v, want ErrTrailingData", err)
			}
		})
	}
}

// The oversize case surfaces as a *http.MaxBytesError so a caller can map it to
// 413 (as vibekit does) while a malformed body maps to 400.
//
// Green on go1.27rc2 as well as 1.26. If it ever fails under a
// GOEXPERIMENT=jsonv2 build on a Go 1.26 toolchain, that is golang/go#77789
// (fixed in 1.27, not backported), not a real regression — see LimitBody's doc.
func TestDecodeJSONInto_oversizeIsMaxBytesError(t *testing.T) {
	// A VALID JSON body that exceeds the cap: the decoder reads past maxBytes
	// while parsing, so the MaxBytesReader trips. (A non-JSON body would fail
	// with a syntax error at byte 0, before the cap ever matters.)
	body := `{"name":"` + strings.Repeat("x", 64) + `"}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	var p payload
	err := webhttp.DecodeJSONInto(rr, req, &p, 16)
	if _, isTooLarge := errors.AsType[*http.MaxBytesError](err); !isTooLarge {
		t.Fatalf("err = %v (%T), want a *http.MaxBytesError", err, err)
	}
}

// The cap is the caller's, not the fixed MaxJSONBody — a body under the given
// cap decodes even when it would exceed a smaller one.
func TestDecodeJSONInto_customCap(t *testing.T) {
	body := `{"name":"` + strings.Repeat("y", 200) + `"}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	var p payload
	if err := webhttp.DecodeJSONInto(rr, req, &p, 4096); err != nil {
		t.Fatalf("DecodeJSONInto err = %v, want nil under a 4 KiB cap", err)
	}
	if len(p.Name) != 200 {
		t.Errorf("Name len = %d, want 200", len(p.Name))
	}
}

// --- LimitBody: the over-the-wire contract -----------------------------------
//
// LimitBody's two effects have very different reliability, and both are pinned
// here because a consumer's error handling depends on knowing which is which:
//
//   - the read fails with a *http.MaxBytesError: ALWAYS, in every composition.
//   - net/http closes the connection: only when the MaxBytesReader was handed
//     net/http's OWN writer. The signal is an unexported net/http interface, so
//     no wrapper can carry it; LimitBody walks the Unwrap chain to reach past
//     webhttp's own recorders, and cannot reach past a wrapper that does not
//     unwrap (see the opaque-wrapper and RouteTimeout cases below).
//
// The tests use a real server because the close is a transport-level effect an
// httptest.ResponseRecorder cannot show.

// limitProbeResult is what one over-limit request looked like from both ends.
type limitProbeResult struct {
	status      int  // status the client received
	clientClose bool // client saw the response as connection-closing
	tooLarge    bool // the handler's read failed with *http.MaxBytesError
	reusedNext  bool // a following request rode the same connection
}

// runLimitBodyProbe serves one POST of bodyLen bytes through wrap under a
// maxBytes cap and reports what both ends observed. wrap composes the
// middleware (or writer wrapper) under test around the reading handler.
func runLimitBodyProbe(t *testing.T, wrap func(http.Handler) http.Handler, maxBytes int64, bodyLen int) limitProbeResult {
	t.Helper()

	var res limitProbeResult
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost { // the reuse probe below
			w.WriteHeader(http.StatusOK)
			return
		}
		webhttp.LimitBody(w, r, maxBytes)
		_, err := io.ReadAll(r.Body)
		var tooLarge *http.MaxBytesError
		res.tooLarge = errors.As(err, &tooLarge)
		if res.tooLarge && tooLarge.Limit != maxBytes {
			t.Errorf("MaxBytesError.Limit = %d, want the cap %d", tooLarge.Limit, maxBytes)
		}
		if err != nil && !res.tooLarge {
			t.Errorf("read err = %v (%T), want nil or *http.MaxBytesError", err, err)
		}
		if res.tooLarge {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(wrap(h))
	t.Cleanup(srv.Close)
	client := srv.Client()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader(strings.Repeat("z", bodyLen)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	res.status, res.clientClose = resp.StatusCode, resp.Close
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Whether the connection survived is the effect that actually matters (a
	// closed connection is what stops a sender from streaming more), so observe
	// it directly rather than trusting the header alone.
	next, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("build reuse probe: %v", err)
	}
	next = next.WithContext(httptrace.WithClientTrace(next.Context(), &httptrace.ClientTrace{
		GotConn: func(i httptrace.GotConnInfo) { res.reusedNext = i.Reused },
	}))
	resp2, err := client.Do(next)
	if err != nil {
		t.Fatalf("reuse probe: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	return res
}

// A bare handler: the MaxBytesReader gets net/http's own writer, so the
// too-large signal lands and the connection is closed after the reply.
func TestLimitBody_overLimitClosesConnection_bareWriter(t *testing.T) {
	res := runLimitBodyProbe(t, func(h http.Handler) http.Handler { return h }, 8, 128)
	if !res.tooLarge {
		t.Error("read did not fail with *http.MaxBytesError")
	}
	if !res.clientClose {
		t.Error("resp.Close = false, want true: the over-limit read must close the connection on a bare writer")
	}
	if res.reusedNext {
		t.Error("the connection was reused after an over-limit read")
	}
}

// The real-world composition, and the case this whole contract exists for:
// behind Logging (a StatusRecorder) and Recoverer (another one), the close used
// to be silently lost because MaxBytesReader only signals net/http's own
// writer. LimitBody's Unwrap walk restores it.
func TestLimitBody_overLimitClosesConnection_behindWebhttpMiddleware(t *testing.T) {
	stacks := map[string]func(http.Handler) http.Handler{
		"Logging": func(h http.Handler) http.Handler {
			return webhttp.Chain(h, webhttp.Logging(webhttp.WithLogger(discardLogger())))
		},
		"Recoverer": func(h http.Handler) http.Handler {
			return webhttp.Chain(h, webhttp.Recoverer(webhttp.WithRecoverLogger(discardLogger())))
		},
		"SecurityHeaders": func(h http.Handler) http.Handler {
			return webhttp.Chain(h, webhttp.SecurityHeaders())
		},
		"fullStack": func(h http.Handler) http.Handler {
			return webhttp.Chain(h,
				webhttp.Logging(webhttp.WithLogger(discardLogger())),
				webhttp.Recoverer(webhttp.WithRecoverLogger(discardLogger())),
				webhttp.SecurityHeaders(),
			)
		},
		// A chain deeper than any real stack: the walk is a walk, not one hop.
		"nestedUnwrappableWrappers": func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for range 6 {
					w = &unwrappableWriter{ResponseWriter: w}
				}
				h.ServeHTTP(w, r)
			})
		},
	}
	for name, wrap := range stacks {
		t.Run(name, func(t *testing.T) {
			res := runLimitBodyProbe(t, wrap, 8, 128)
			if !res.tooLarge {
				t.Error("read did not fail with *http.MaxBytesError")
			}
			if !res.clientClose {
				t.Error("resp.Close = false, want true: the too-large signal must survive an unwrappable writer chain")
			}
			if res.reusedNext {
				t.Error("the connection was reused after an over-limit read")
			}
			if res.status != http.StatusRequestEntityTooLarge {
				t.Errorf("status = %d, want 413 (the handler's own answer must still reach the client)", res.status)
			}
		})
	}
}

// The honest ceiling, documented on LimitBody and pinned here so it cannot be
// mistaken for a bug: a wrapper that does not implement Unwrap hides net/http's
// writer, and neither LimitBody nor any other third-party code can deliver the
// signal through it. The typed read error still arrives — which is why the
// caller-side errors.As check is the contract and the close is not.
func TestLimitBody_overLimitCannotCloseThroughOpaqueWrapper(t *testing.T) {
	wraps := map[string]func(http.Handler) http.Handler{
		"noUnwrapMethod": func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h.ServeHTTP(&opaqueWriter{ResponseWriter: w}, r)
			})
		},
		// http.TimeoutHandler's buffering writer is not unwrappable either, so
		// RouteTimeout is the one webhttp composition that loses the close.
		"RouteTimeout": func(h http.Handler) http.Handler {
			return webhttp.RouteTimeout(h, time.Minute, "")
		},
	}
	for name, wrap := range wraps {
		t.Run(name, func(t *testing.T) {
			res := runLimitBodyProbe(t, wrap, 8, 128)
			if !res.tooLarge {
				t.Error("read did not fail with *http.MaxBytesError: the typed error must arrive in every composition")
			}
			if res.clientClose {
				t.Error("resp.Close = true: unexpected close — if net/http can now be signalled through an opaque wrapper, LimitBody's documented limitation is stale")
			}
			if res.status != http.StatusRequestEntityTooLarge {
				t.Errorf("status = %d, want 413", res.status)
			}
		})
	}
}

// A body within the cap is untouched: no error, no close, connection reusable.
func TestLimitBody_underLimitLeavesConnectionReusable(t *testing.T) {
	res := runLimitBodyProbe(t, func(h http.Handler) http.Handler {
		return webhttp.Chain(h, webhttp.Logging(webhttp.WithLogger(discardLogger())))
	}, 1024, 128)
	if res.tooLarge {
		t.Error("a body within the cap reported *http.MaxBytesError")
	}
	if res.status != http.StatusOK {
		t.Errorf("status = %d, want 200", res.status)
	}
	if res.clientClose {
		t.Error("resp.Close = true: a request within the cap must not close the connection")
	}
	if !res.reusedNext {
		t.Error("the connection was not reused after a within-cap request")
	}
}

// Unwrapping serves the too-large signal ONLY: the recorder the handler writes
// through is untouched, so Logging still reports the handler's real status.
func TestLimitBody_overLimitStillRecordsStatusForLogging(t *testing.T) {
	logs := &captureHandler{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webhttp.LimitBody(w, r, 8)
		_, err := io.ReadAll(r.Body)
		if _, isTooLarge := errors.AsType[*http.MaxBytesError](err); !isTooLarge {
			t.Errorf("read err = %v, want *http.MaxBytesError", err)
		}
		webhttp.WriteError(w, r, http.StatusRequestEntityTooLarge, "too_large", "request body too large")
	})
	srv := httptest.NewServer(webhttp.Chain(h, webhttp.Logging(webhttp.WithLogger(slog.New(logs)))))
	defer srv.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader(strings.Repeat("z", 128)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	recs := logs.snapshot()
	if len(recs) != 1 {
		t.Fatalf("captured %d access lines, want 1", len(recs))
	}
	if got := attrsOf(recs[0])["status"]; got != int64(http.StatusRequestEntityTooLarge) {
		t.Errorf("logged status = %v, want %d: the StatusRecorder must still observe the handler's writes", got, http.StatusRequestEntityTooLarge)
	}
}

// A wrapper whose Unwrap never terminates (returns itself), and one that returns
// nil, must both leave LimitBody working: the walk is bounded, and a nil inner
// writer keeps the last real one.
func TestLimitBody_degenerateUnwrapChains(t *testing.T) {
	wraps := map[string]func(http.ResponseWriter) http.ResponseWriter{
		"selfUnwrapping": func(w http.ResponseWriter) http.ResponseWriter {
			return &selfUnwrappingWriter{ResponseWriter: w}
		},
		"nilUnwrapping": func(w http.ResponseWriter) http.ResponseWriter {
			return &nilUnwrappingWriter{ResponseWriter: w}
		},
	}
	for name, wrap := range wraps {
		t.Run(name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("z", 128)))
			webhttp.LimitBody(wrap(rr), req, 8) // must return, not spin
			n, err := io.Copy(io.Discard, req.Body)
			if _, isTooLarge := errors.AsType[*http.MaxBytesError](err); !isTooLarge {
				t.Fatalf("read err = %v (%T), want *http.MaxBytesError", err, err)
			}
			if n != 8 {
				t.Errorf("read %d bytes, want the 8-byte cap", n)
			}
		})
	}
}
