package webhttp_test

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cplieger/webhttp"
)

func TestStatusRecorder_defaultStatusIs200(t *testing.T) {
	rec := webhttp.NewStatusRecorder(httptest.NewRecorder())
	if got := rec.Status(); got != http.StatusOK {
		t.Errorf("Status() = %d, want %d", got, http.StatusOK)
	}
}

func TestStatusRecorder_implicitStatusOnWrite(t *testing.T) {
	under := httptest.NewRecorder()
	rec := webhttp.NewStatusRecorder(under)
	n, err := rec.Write([]byte("hi"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 2 {
		t.Errorf("Write returned n=%d, want 2", n)
	}
	if got := rec.Status(); got != http.StatusOK {
		t.Errorf("Status() = %d, want %d after implicit write", got, http.StatusOK)
	}
	if under.Body.String() != "hi" {
		t.Errorf("underlying body = %q, want %q", under.Body.String(), "hi")
	}
}

func TestStatusRecorder_recordsFirstCodeOnly(t *testing.T) {
	under := httptest.NewRecorder()
	rec := webhttp.NewStatusRecorder(under)
	rec.WriteHeader(http.StatusTeapot)
	rec.WriteHeader(http.StatusInternalServerError)
	if got := rec.Status(); got != http.StatusTeapot {
		t.Errorf("Status() = %d, want %d (first code wins)", got, http.StatusTeapot)
	}
	if under.Code != http.StatusTeapot {
		t.Errorf("underlying Code = %d, want %d", under.Code, http.StatusTeapot)
	}
}

func TestStatusRecorder_recordsExplicitCode(t *testing.T) {
	under := httptest.NewRecorder()
	rec := webhttp.NewStatusRecorder(under)
	rec.WriteHeader(http.StatusNoContent)
	if got := rec.Status(); got != http.StatusNoContent {
		t.Errorf("Status() = %d, want %d", got, http.StatusNoContent)
	}
}

func TestStatusRecorder_unwrapReturnsWrappedWriter(t *testing.T) {
	under := httptest.NewRecorder()
	rec := webhttp.NewStatusRecorder(under)
	if rec.Unwrap() != under {
		t.Error("Unwrap() did not return the wrapped writer")
	}
}

func TestStatusRecorder_flushReachesHTTPTestRecorder(t *testing.T) {
	under := httptest.NewRecorder()
	rec := webhttp.NewStatusRecorder(under)
	if err := http.NewResponseController(rec).Flush(); err != nil {
		t.Fatalf("Flush through recorder: %v", err)
	}
	if !under.Flushed {
		t.Error("underlying httptest recorder was not flushed through Unwrap")
	}
}

// flushOnlyWriter exposes Flush so the Unwrap chain can be tested with a writer
// whose flusher is reached only through StatusRecorder.Unwrap.
type flushOnlyWriter struct {
	http.ResponseWriter
	flushed bool
}

func (f *flushOnlyWriter) Flush() { f.flushed = true }

func TestStatusRecorder_flushReachesCustomFlusherThroughUnwrap(t *testing.T) {
	fw := &flushOnlyWriter{ResponseWriter: httptest.NewRecorder()}
	rec := webhttp.NewStatusRecorder(fw)
	if err := http.NewResponseController(rec).Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !fw.flushed {
		t.Error("custom Flush was not reached through the Unwrap chain")
	}
}

// hijackOnlyWriter exposes Hijack so the Unwrap chain can be tested for the
// hijack path SSE/WebSocket handlers rely on.
type hijackOnlyWriter struct {
	http.ResponseWriter
	hijacked bool
}

func (h *hijackOnlyWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, nil
}

func TestStatusRecorder_hijackReachesCustomHijackerThroughUnwrap(t *testing.T) {
	hw := &hijackOnlyWriter{ResponseWriter: httptest.NewRecorder()}
	rec := webhttp.NewStatusRecorder(hw)
	if _, _, err := http.NewResponseController(rec).Hijack(); err != nil {
		t.Fatalf("Hijack: %v", err)
	}
	if !hw.hijacked {
		t.Error("custom Hijack was not reached through the Unwrap chain")
	}
}
