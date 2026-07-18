package webhttp_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cplieger/webhttp"
)

func TestJSONHeaders(t *testing.T) {
	rr := httptest.NewRecorder()
	webhttp.JSONHeaders(rr)
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if xo := rr.Header().Get("X-Content-Type-Options"); xo != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", xo)
	}

	t.Run("defers to an upstream nosniff writer", func(t *testing.T) {
		rr := httptest.NewRecorder()
		// SecurityHeaders (or any upstream middleware) already set the guard;
		// JSONHeaders must leave that single writer in charge, not re-set it.
		rr.Header().Set("X-Content-Type-Options", "nosniff")
		webhttp.JSONHeaders(rr)
		if got := rr.Header().Values("X-Content-Type-Options"); len(got) != 1 || got[0] != "nosniff" {
			t.Errorf("X-Content-Type-Options values = %q, want exactly [nosniff]", got)
		}
	})
}

func TestWriteJSON(t *testing.T) {
	rr := httptest.NewRecorder()
	webhttp.WriteJSON(rr, map[string]int{"n": 5})
	if rr.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got map[string]int
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["n"] != 5 {
		t.Errorf("body n = %d, want 5", got["n"])
	}
}

func TestWriteJSONStatus(t *testing.T) {
	rr := httptest.NewRecorder()
	webhttp.WriteJSONStatus(rr, http.StatusCreated, map[string]string{"k": "v"})
	if rr.Code != http.StatusCreated {
		t.Errorf("code = %d, want 201", rr.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["k"] != "v" {
		t.Errorf("body k = %q, want v", got["k"])
	}
}

func TestWriteJSONStatus_encodeFailureCommitsStatusAndWarns(t *testing.T) {
	logCap := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logCap))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rr := httptest.NewRecorder()
	// A channel cannot be JSON-encoded, so Encode fails after the status is
	// already committed.
	webhttp.WriteJSONStatus(rr, http.StatusBadGateway, make(chan int))

	if rr.Code != http.StatusBadGateway {
		t.Errorf("status not committed before encode failure: got %d, want %d", rr.Code, http.StatusBadGateway)
	}
	recs := logCap.snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d log records, want 1 warn", len(recs))
	}
	if recs[0].Level != slog.LevelWarn {
		t.Errorf("log level = %v, want Warn", recs[0].Level)
	}
	if recs[0].Message != "webhttp: json encode failed after status committed" {
		t.Errorf("log message = %q", recs[0].Message)
	}
	m := attrsOf(recs[0])
	if m["code"] != int64(http.StatusBadGateway) {
		t.Errorf("code attr = %v, want %d", m["code"], http.StatusBadGateway)
	}
	if m["error"] == nil {
		t.Error("missing error attr on encode-failure warn")
	}
}

func TestOk(t *testing.T) {
	rr := httptest.NewRecorder()
	webhttp.Ok(rr)
	if rr.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", rr.Code)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"ok":true}` {
		t.Errorf("body = %q, want %q", got, `{"ok":true}`)
	}
}

func TestWriteError_withRequestID(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(webhttp.WithRequestID(req.Context(), "rid-9"))

	webhttp.WriteError(rr, req, http.StatusForbidden, "forbidden", "no access")

	if rr.Code != http.StatusForbidden {
		t.Errorf("code = %d, want 403", rr.Code)
	}
	var got webhttp.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Error != "no access" || got.Code != "forbidden" || got.RequestID != "rid-9" {
		t.Errorf("body = %+v, want {no access forbidden rid-9}", got)
	}
}

func TestWriteError_nilRequestIsSafe(t *testing.T) {
	rr := httptest.NewRecorder()
	// Must not panic when r is nil.
	webhttp.WriteError(rr, nil, http.StatusInternalServerError, "internal", "boom")

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("code = %d, want 500", rr.Code)
	}
	var got webhttp.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.RequestID != "" {
		t.Errorf("RequestID = %q, want empty for nil request", got.RequestID)
	}
	if got.Error != "boom" || got.Code != "internal" {
		t.Errorf("body = %+v", got)
	}
}

func TestWriteError_omitsEmptyOptionalFields(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil) // no request id in context
	webhttp.WriteError(rr, req, http.StatusBadRequest, "", "just a message")

	body := rr.Body.String()
	if strings.Contains(body, "\"code\"") {
		t.Errorf("empty code not omitted: %s", body)
	}
	if strings.Contains(body, "request_id") {
		t.Errorf("empty request_id not omitted: %s", body)
	}
}
