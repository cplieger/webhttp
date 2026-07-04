package webhttp_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cplieger/webhttp"
)

func TestReady_zeroValueIsNotReady(t *testing.T) {
	var r webhttp.Ready
	if r.Ready() {
		t.Error("zero-value Ready should report not ready")
	}
}

func TestReady_setToggles(t *testing.T) {
	var r webhttp.Ready
	r.Set(true)
	if !r.Ready() {
		t.Error("want ready after Set(true)")
	}
	r.Set(false)
	if r.Ready() {
		t.Error("want not ready after Set(false)")
	}
}

func TestReadinessHandler_ready(t *testing.T) {
	r := &webhttp.Ready{}
	r.Set(true)
	rr := httptest.NewRecorder()
	webhttp.ReadinessHandler(r).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rr.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["status"] != "ok" {
		t.Errorf("status = %q, want ok", got["status"])
	}
}

func TestReadinessHandler_unready(t *testing.T) {
	r := &webhttp.Ready{} // zero value: not ready
	rr := httptest.NewRecorder()
	webhttp.ReadinessHandler(r).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", rr.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["status"] != "unready" {
		t.Errorf("status = %q, want unready", got["status"])
	}
	if got["reason"] != "starting up or shutting down" {
		t.Errorf("reason = %q, want %q", got["reason"], "starting up or shutting down")
	}
}

// readyFunc adapts a function to the ReadinessChecker interface, proving a
// consumer can supply its own composite readiness implementation.
type readyFunc func() bool

func (f readyFunc) Ready() bool { return f() }

func TestReadinessHandler_customChecker(t *testing.T) {
	rr := httptest.NewRecorder()
	webhttp.ReadinessHandler(readyFunc(func() bool { return true })).
		ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("code = %d, want 200 for a ready custom checker", rr.Code)
	}
}
