package webhttp_test

import (
	"bytes"
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
	var got webhttp.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Code != "method_not_allowed" {
		t.Errorf("code = %q, want method_not_allowed", got.Code)
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

func TestLimitedWriter(t *testing.T) {
	cases := []struct {
		name    string
		n       int64
		writes  []string
		wantOut string
		wantN   []int
	}{
		{"under budget", 100, []string{"hello"}, "hello", []int{5}},
		{"exact budget", 5, []string{"hello"}, "hello", []int{5}},
		{"over budget single write", 3, []string{"hello"}, "hel", []int{5}},
		{"zero budget drops all", 0, []string{"hello"}, "", []int{5}},
		{"negative budget drops all", -1, []string{"hello"}, "", []int{5}},
		{"multi write accumulates to cap", 4, []string{"ab", "cd", "ef"}, "abcd", []int{2, 2, 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			lw := &webhttp.LimitedWriter{W: &buf, N: tc.n}
			for i, s := range tc.writes {
				n, err := lw.Write([]byte(s))
				if err != nil {
					t.Fatalf("write %d: unexpected error %v", i, err)
				}
				if n != tc.wantN[i] {
					t.Errorf("write %d returned n=%d, want %d", i, n, tc.wantN[i])
				}
			}
			if buf.String() != tc.wantOut {
				t.Errorf("output = %q, want %q", buf.String(), tc.wantOut)
			}
		})
	}
}

var errBoom = errors.New("boom")

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errBoom }

func TestLimitedWriter_underlyingErrorPropagates(t *testing.T) {
	lw := &webhttp.LimitedWriter{W: errWriter{}, N: 100}
	n, err := lw.Write([]byte("hello"))
	if !errors.Is(err, errBoom) {
		t.Errorf("err = %v, want errBoom", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0 on underlying error", n)
	}
}
