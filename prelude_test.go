package webhttp_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cplieger/webhttp"
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
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"a"}{"name":"b"}`))
	var p payload
	err := webhttp.DecodeJSONInto(rr, req, &p, webhttp.MaxJSONBody)
	if !errors.Is(err, webhttp.ErrTrailingData) {
		t.Fatalf("err = %v, want ErrTrailingData", err)
	}
}

// The oversize case surfaces as a *http.MaxBytesError so a caller can map it to
// 413 (as vibekit does) while a malformed body maps to 400.
func TestDecodeJSONInto_oversizeIsMaxBytesError(t *testing.T) {
	// A VALID JSON body that exceeds the cap: the decoder reads past maxBytes
	// while parsing, so the MaxBytesReader trips. (A non-JSON body would fail
	// with a syntax error at byte 0, before the cap ever matters.)
	body := `{"name":"` + strings.Repeat("x", 64) + `"}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	var p payload
	err := webhttp.DecodeJSONInto(rr, req, &p, 16)
	var maxErr *http.MaxBytesError
	if !errors.As(err, &maxErr) {
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
