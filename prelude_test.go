package webhttp_test

import (
	"encoding/json"
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
