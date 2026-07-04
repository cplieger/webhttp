package webhttp_test

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestStatusRecorder_satisfiesFlusherAndHijackerDirectly(t *testing.T) {
	rec := webhttp.NewStatusRecorder(httptest.NewRecorder())
	// A direct type assertion is what libraries like gorilla/websocket use
	// (w.(http.Hijacker)); it must succeed independently of ResponseController.
	if _, ok := any(rec).(http.Flusher); !ok {
		t.Error("*StatusRecorder does not satisfy http.Flusher via a direct type assertion")
	}
	if _, ok := any(rec).(http.Hijacker); !ok {
		t.Error("*StatusRecorder does not satisfy http.Hijacker via a direct type assertion")
	}
	if _, ok := any(rec).(io.ReaderFrom); !ok {
		t.Error("*StatusRecorder does not satisfy io.ReaderFrom via a direct type assertion")
	}
}

func TestStatusRecorder_directFlushReachesUnderlyingFlusher(t *testing.T) {
	fw := &flushOnlyWriter{ResponseWriter: httptest.NewRecorder()}
	rec := webhttp.NewStatusRecorder(fw)
	rec.Flush() // direct call, not via http.NewResponseController
	if !fw.flushed {
		t.Error("direct StatusRecorder.Flush did not reach the underlying flusher")
	}
}

func TestStatusRecorder_directHijackReachesUnderlyingHijacker(t *testing.T) {
	hw := &hijackOnlyWriter{ResponseWriter: httptest.NewRecorder()}
	rec := webhttp.NewStatusRecorder(hw)
	if _, _, err := rec.Hijack(); err != nil { // direct call, not via ResponseController
		t.Fatalf("Hijack: %v", err)
	}
	if !hw.hijacked {
		t.Error("direct StatusRecorder.Hijack did not reach the underlying hijacker")
	}
}

// readerFromWriter records ReadFrom calls to prove StatusRecorder.ReadFrom
// forwards to an underlying io.ReaderFrom (the zero-copy/sendfile fast path).
type readerFromWriter struct {
	http.ResponseWriter
	readFromCalled bool
}

func (rw *readerFromWriter) ReadFrom(src io.Reader) (int64, error) {
	rw.readFromCalled = true
	return io.Copy(rw.ResponseWriter, src)
}

func TestStatusRecorder_readFromUsesUnderlyingReaderFrom(t *testing.T) {
	under := &readerFromWriter{ResponseWriter: httptest.NewRecorder()}
	rec := webhttp.NewStatusRecorder(under)
	n, err := rec.ReadFrom(strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if n != 5 {
		t.Errorf("ReadFrom n = %d, want 5", n)
	}
	if !under.readFromCalled {
		t.Error("ReadFrom did not forward to the underlying io.ReaderFrom")
	}
	if rec.Status() != http.StatusOK {
		t.Errorf("Status() = %d, want 200 (ReadFrom implies a written body)", rec.Status())
	}
}

// plainWriter is an http.ResponseWriter with no ReadFrom method, so
// StatusRecorder.ReadFrom must fall back to io.Copy. Because the embedded field
// is the http.ResponseWriter interface, ReadFrom is not promoted even when the
// dynamic value has one.
type plainWriter struct {
	http.ResponseWriter
}

func TestStatusRecorder_readFromFallsBackToCopy(t *testing.T) {
	under := httptest.NewRecorder()
	rec := webhttp.NewStatusRecorder(&plainWriter{ResponseWriter: under})
	n, err := rec.ReadFrom(strings.NewReader("world"))
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if n != 5 {
		t.Errorf("ReadFrom n = %d, want 5", n)
	}
	if under.Body.String() != "world" {
		t.Errorf("underlying body = %q, want world (io.Copy fallback)", under.Body.String())
	}
}
