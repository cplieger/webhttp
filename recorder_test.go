package webhttp_test

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/webhttp/v4"
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
		t.Error("underlying httptest recorder was not flushed through the recorder")
	}
}

// flushOnlyWriter is an http.Flusher with no FlushError, the older shape a
// recorder's flush must still reach.
type flushOnlyWriter struct {
	http.ResponseWriter
	flushed bool
}

func (f *flushOnlyWriter) Flush() { f.flushed = true }

func TestStatusRecorder_flushReachesCustomFlusherThroughResponseController(t *testing.T) {
	fw := &flushOnlyWriter{ResponseWriter: httptest.NewRecorder()}
	rec := webhttp.NewStatusRecorder(fw)
	if err := http.NewResponseController(rec).Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !fw.flushed {
		t.Error("custom Flush was not reached through the recorder")
	}
}

// hijackOnlyWriter exposes Hijack, the path WebSocket handlers rely on, so a
// hijack through the recorder can be observed.
type hijackOnlyWriter struct {
	http.ResponseWriter
	hijacked bool
}

func (h *hijackOnlyWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, nil
}

func TestStatusRecorder_hijackReachesCustomHijackerThroughResponseController(t *testing.T) {
	hw := &hijackOnlyWriter{ResponseWriter: httptest.NewRecorder()}
	rec := webhttp.NewStatusRecorder(hw)
	if _, _, err := http.NewResponseController(rec).Hijack(); err != nil {
		t.Fatalf("Hijack: %v", err)
	}
	if !hw.hijacked {
		t.Error("custom Hijack was not reached through the recorder")
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

// TestStatusRecorder_flushMarksCommitted asserts a successful Flush commits the
// response so Wrote() reports true. net/http writes an implicit 200 on flush, so
// Recoverer must not write a second status/body onto an already-flushed response.
func TestStatusRecorder_flushMarksCommitted(t *testing.T) {
	rec := webhttp.NewStatusRecorder(httptest.NewRecorder())
	if rec.Wrote() {
		t.Fatal("Wrote() = true before any write")
	}
	rec.Flush()
	if !rec.Wrote() {
		t.Error("Wrote() = false after a successful Flush; Recoverer would double-write onto a committed response")
	}
}

// TestStatusRecorder_hijackMarksCommitted asserts a successful Hijack commits the
// response so Wrote() reports true. Once the connection is hijacked the
// ResponseWriter can no longer emit a status/body, so Recoverer must skip its 500.
func TestStatusRecorder_hijackMarksCommitted(t *testing.T) {
	hw := &hijackOnlyWriter{ResponseWriter: httptest.NewRecorder()}
	rec := webhttp.NewStatusRecorder(hw)
	if rec.Wrote() {
		t.Fatal("Wrote() = true before any write")
	}
	if _, _, err := rec.Hijack(); err != nil {
		t.Fatalf("Hijack: %v", err)
	}
	if !rec.Wrote() {
		t.Error("Wrote() = false after a successful Hijack; Recoverer would double-write onto a hijacked connection")
	}
}

// --- Negative-path regressions ---------------------------------------------
// The success-path tests above prove Wrote() flips to true on a successful
// Flush or Hijack. These prove the opposite branch: a FAILED or unsupported
// Flush, Hijack, or ReadFrom must NOT commit the recorder, so Recoverer keeps
// emitting its 500 on a following panic. A future change that marked the
// recorder committed unconditionally would silently suppress that 500, and this
// suite would catch it.

// errRead is the sentinel returned by errReader and failingReaderFromWriter, so
// a zero-byte ReadFrom failure is identifiable through errors.Is.
var errRead = errors.New("read failed")

// errReader fails on its first Read, so a copy over it writes zero bytes and
// returns errRead.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errRead }

// failedHijackWriter's Hijack always fails, modeling an HTTP/2 stream (or any
// writer) that cannot be hijacked. It proves a FAILED hijack neither commits
// the recorder nor swallows the underlying error.
type failedHijackWriter struct {
	http.ResponseWriter
}

func (*failedHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, http.ErrNotSupported
}

// failingReaderFromWriter is an underlying writer whose ReadFrom always fails
// with zero bytes written, exercising the io.ReaderFrom fast path of
// StatusRecorder.ReadFrom on its failure branch.
type failingReaderFromWriter struct {
	http.ResponseWriter
}

func (*failingReaderFromWriter) ReadFrom(io.Reader) (int64, error) {
	return 0, errRead
}

// flushErrorWriter models net/http's writer on a connection whose flush fails
// (a passed write deadline returns os.ErrDeadlineExceeded): net/http has
// already committed its implicit 200 when the error comes back.
type flushErrorWriter struct {
	http.ResponseWriter
	err error
}

func (f *flushErrorWriter) FlushError() error { return f.err }

func TestStatusRecorder_flushErrorReportsUnderlyingResult(t *testing.T) {
	tests := []struct {
		name      string
		under     http.ResponseWriter
		wantErr   error
		wantWrote bool
	}{
		{"successful flush commits", httptest.NewRecorder(), nil, true},
		{"failed flush returns its error committed", &flushErrorWriter{httptest.NewRecorder(), os.ErrDeadlineExceeded}, os.ErrDeadlineExceeded, true},
		{"no flush support returns ErrNotSupported uncommitted", &plainWriter{httptest.NewRecorder()}, http.ErrNotSupported, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := webhttp.NewStatusRecorder(tt.under)
			err := http.NewResponseController(rec).Flush()
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("ResponseController.Flush err = %v, want %v", err, tt.wantErr)
			}
			if got := rec.Wrote(); got != tt.wantWrote {
				t.Errorf("Wrote() = %v, want %v", got, tt.wantWrote)
			}
		})
	}
}

// TestStatusRecorder_flushErrorReachesHandlerThroughMiddleware covers the
// recorder where handlers meet it: inside Logging, and inside Recoverer and
// Gzip, whose own flush forwards to the recorder.
func TestStatusRecorder_flushErrorReachesHandlerThroughMiddleware(t *testing.T) {
	stacks := []struct {
		name string
		wrap func(http.Handler) http.Handler
	}{
		{"Logging", webhttp.Logging(webhttp.WithLogger(discardLogger()))},
		{"Logging Recoverer Gzip", func(h http.Handler) http.Handler {
			return webhttp.Chain(h,
				webhttp.Logging(webhttp.WithLogger(discardLogger())),
				webhttp.Recoverer(webhttp.WithRecoverLogger(discardLogger())),
				webhttp.Gzip(),
			)
		}},
	}
	writers := []struct {
		name    string
		under   func() http.ResponseWriter
		wantErr error
	}{
		{"flush succeeds", func() http.ResponseWriter { return httptest.NewRecorder() }, nil},
		{"flush hits the write deadline", func() http.ResponseWriter {
			return &flushErrorWriter{httptest.NewRecorder(), os.ErrDeadlineExceeded}
		}, os.ErrDeadlineExceeded},
		{"writer cannot flush", func() http.ResponseWriter { return &plainWriter{httptest.NewRecorder()} }, http.ErrNotSupported},
	}
	for _, s := range stacks {
		for _, wr := range writers {
			t.Run(s.name+" "+wr.name, func(t *testing.T) {
				var err error
				h := s.wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					err = http.NewResponseController(w).Flush()
				}))
				h.ServeHTTP(wr.under(), httptest.NewRequest(http.MethodGet, "/feed", nil))
				if !errors.Is(err, wr.wantErr) {
					t.Errorf("handler's ResponseController.Flush err = %v, want %v", err, wr.wantErr)
				}
			})
		}
	}
}

func TestStatusRecorder_unsupportedFlushLeavesUncommitted(t *testing.T) {
	// plainWriter implements no optional interface, so a Flush through the
	// recorder finds no flusher and http.NewResponseController returns
	// http.ErrNotSupported. Nothing was written, so the response stays
	// uncommitted.
	rec := webhttp.NewStatusRecorder(&plainWriter{ResponseWriter: httptest.NewRecorder()})
	if rec.Wrote() {
		t.Fatal("Wrote() = true before any operation")
	}
	rec.Flush()
	if rec.Wrote() {
		t.Error("Wrote() = true after an unsupported Flush; no flusher ran, so nothing was committed")
	}
}

// TestRecoverer_skips500AfterFailedFlushOnRealServer runs net/http's own
// writer, which commits its implicit 200 before a flush fails: Recoverer must
// not write a 500 onto it, and the access line must record the 200 that went
// out.
func TestRecoverer_skips500AfterFailedFlushOnRealServer(t *testing.T) {
	logCap := &captureHandler{}
	h := webhttp.Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rc := http.NewResponseController(w)
		if err := rc.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
			t.Errorf("SetWriteDeadline: %v", err)
		}
		if err := rc.Flush(); err == nil {
			t.Error("Flush past the write deadline returned nil, so this test cannot observe a failed flush")
		}
		panic("boom after failed flush")
	}),
		webhttp.Logging(webhttp.WithLogger(slog.New(logCap))),
		webhttp.Recoverer(webhttp.WithRecoverLogger(discardLogger())),
	)
	url, errorLog := gzipServer(t, h)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The connection fails mid-response, so the client error is expected.
	if resp, err := http.DefaultTransport.RoundTrip(req); err == nil {
		_ = resp.Body.Close()
	}

	if logs := errorLog(); strings.Contains(logs, "superfluous") {
		t.Errorf("server error log = %q, want no superfluous WriteHeader from Recoverer", logs)
	}
	recs := logCap.snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d log records, want exactly 1 (the access line)", len(recs))
	}
	if m := attrsOf(recs[0]); m["status"] != int64(http.StatusOK) {
		t.Errorf("access line status = %v, want 200 (the status net/http committed)", m["status"])
	}
}

func TestStatusRecorder_failedHijackLeavesUncommitted(t *testing.T) {
	rec := webhttp.NewStatusRecorder(&failedHijackWriter{ResponseWriter: httptest.NewRecorder()})
	if rec.Wrote() {
		t.Fatal("Wrote() = true before any operation")
	}
	conn, rw, err := rec.Hijack()
	if !errors.Is(err, http.ErrNotSupported) {
		t.Errorf("Hijack err = %v, want the underlying http.ErrNotSupported to propagate", err)
	}
	if conn != nil || rw != nil {
		t.Errorf("Hijack returned conn=%v rw=%v, want both nil on failure", conn, rw)
	}
	if rec.Wrote() {
		t.Error("Wrote() = true after a failed Hijack; a failed hijack must not commit the response")
	}
}

func TestStatusRecorder_failedReadFromCopyPathLeavesUncommitted(t *testing.T) {
	// The underlying httptest recorder is not an io.ReaderFrom, so ReadFrom takes
	// the io.Copy fallback and the failing source writes zero bytes. A zero-byte
	// failure must not commit the response.
	rec := webhttp.NewStatusRecorder(httptest.NewRecorder())
	if rec.Wrote() {
		t.Fatal("Wrote() = true before any operation")
	}
	n, err := rec.ReadFrom(errReader{})
	if n != 0 {
		t.Errorf("ReadFrom n = %d, want 0 on a failed read", n)
	}
	if !errors.Is(err, errRead) {
		t.Errorf("ReadFrom err = %v, want the source error errRead to propagate", err)
	}
	if rec.Wrote() {
		t.Error("Wrote() = true after a zero-byte ReadFrom failure (copy path); must not commit")
	}
}

func TestStatusRecorder_failedReadFromReaderFromPathLeavesUncommitted(t *testing.T) {
	// The underlying writer IS an io.ReaderFrom that fails with zero bytes, so
	// ReadFrom takes the fast path and returns (0, err). It must not commit.
	rec := webhttp.NewStatusRecorder(&failingReaderFromWriter{ResponseWriter: httptest.NewRecorder()})
	if rec.Wrote() {
		t.Fatal("Wrote() = true before any operation")
	}
	n, err := rec.ReadFrom(strings.NewReader("unused"))
	if n != 0 {
		t.Errorf("ReadFrom n = %d, want 0 on a failed read", n)
	}
	if !errors.Is(err, errRead) {
		t.Errorf("ReadFrom err = %v, want the underlying error errRead to propagate", err)
	}
	if rec.Wrote() {
		t.Error("Wrote() = true after a zero-byte ReadFrom failure (ReaderFrom path); must not commit")
	}
}

// TestRecoverer_emits500AfterFailedReadFrom ties the recorder's negative-path
// contract to Recoverer end to end: a handler whose ReadFrom fails with zero
// bytes leaves the response uncommitted (Wrote() == false), so a following
// panic must still produce the 500. Recoverer reads only Wrote(), so one failed
// op through the full middleware proves the wiring the three recorder-level
// negative tests above establish for each op.
func TestRecoverer_emits500AfterFailedReadFrom(t *testing.T) {
	panicky := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rf, ok := w.(io.ReaderFrom)
		if !ok {
			t.Error("Recoverer's writer is not an io.ReaderFrom")
		} else if n, err := rf.ReadFrom(errReader{}); n != 0 || err == nil {
			t.Errorf("failed ReadFrom = (%d, %v), want (0, non-nil)", n, err)
		}
		panic("boom after failed readfrom")
	})
	h := webhttp.Recoverer(webhttp.WithRecoverLogger(discardLogger()))(panicky)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	func() {
		defer func() {
			if v := recover(); v != nil {
				t.Fatalf("panic escaped Recoverer: %v", v)
			}
		}()
		h.ServeHTTP(rec, req)
	}()

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (a failed ReadFrom must not suppress Recoverer's 500)", rec.Code)
	}
}

// TestStatusRecorder_readFromMarksCommitted asserts a successful (n>0) ReadFrom
// commits the response so Wrote() reports true: streaming a body through the
// io.ReaderFrom fast path starts the response just like Write, so Recoverer must
// not append a second status and body onto it.
func TestStatusRecorder_readFromMarksCommitted(t *testing.T) {
	// readerFromWriter implements io.ReaderFrom, so ReadFrom takes the fast path
	// (rf.ReadFrom) rather than the io.Copy fallback.
	under := &readerFromWriter{ResponseWriter: httptest.NewRecorder()}
	rec := webhttp.NewStatusRecorder(under)
	if rec.Wrote() {
		t.Fatal("Wrote() = true before any write")
	}
	n, err := rec.ReadFrom(strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if n != 5 {
		t.Errorf("ReadFrom n = %d, want 5", n)
	}
	if !under.readFromCalled {
		t.Error("ReadFrom did not take the io.ReaderFrom fast path")
	}
	if !rec.Wrote() {
		t.Error("Wrote() = false after a successful ReadFrom; Recoverer would double-write onto a committed body")
	}
	// The body is committed at the implicit 200, so a later WriteHeader must not
	// overwrite the recorded status (first-code-wins, guarded by wroteHeader).
	rec.WriteHeader(http.StatusInternalServerError)
	if got := rec.Status(); got != http.StatusOK {
		t.Errorf("Status() = %d, want 200 (a committed ReadFrom body fixes the status; a later WriteHeader must not overwrite it)", got)
	}
}
