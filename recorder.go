package webhttp

import (
	"bufio"
	"io"
	"net"
	"net/http"
)

// StatusRecorder wraps an http.ResponseWriter to capture the response status
// code. Middleware that needs to observe the final status (access logging,
// metrics) wraps the writer in a StatusRecorder.
//
// It stays transparent to streaming in two complementary ways. The Unwrap
// method lets http.NewResponseController walk to the underlying writer, so
// ResponseController-based callers reach its Flusher, Hijacker, and deadline
// setters. It also implements http.Flusher, http.Hijacker, and io.ReaderFrom
// directly, so a handler or library that type-asserts those interfaces on the
// writer (as gorilla/websocket does with w.(http.Hijacker)) still works, and
// io.Copy/http.ServeContent keep the zero-copy sendfile fast path. Each
// passthrough returns the underlying writer's own result, so, for example,
// Hijack reports http.ErrNotSupported on an HTTP/2 stream.
type StatusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

// NewStatusRecorder wraps w. The recorded status defaults to http.StatusOK
// (200) and stays there until WriteHeader or the first Write records a status.
func NewStatusRecorder(w http.ResponseWriter) *StatusRecorder {
	return &StatusRecorder{ResponseWriter: w, status: http.StatusOK}
}

// WriteHeader records the first explicit status code and forwards every call to
// the wrapped writer. Only the first code is recorded, matching net/http's
// first-code-wins semantics; a later call still forwards (so net/http logs its
// standard "superfluous WriteHeader" warning) but does not change Status.
func (s *StatusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

// Write records an implicit 200 status on the first write when WriteHeader was
// not called (mirroring net/http), then forwards to the wrapped writer.
func (s *StatusRecorder) Write(b []byte) (int, error) {
	s.wroteHeader = true
	return s.ResponseWriter.Write(b)
}

// Status returns the recorded status code, or http.StatusOK if none was set.
func (s *StatusRecorder) Status() int {
	return s.status
}

// Unwrap returns the wrapped http.ResponseWriter. http.NewResponseController
// walks Unwrap to reach a Flusher, Hijacker, or other optional interface
// implemented by the underlying writer, so streaming still works through the
// recorder.
func (s *StatusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

// Flush forwards to the underlying writer via http.ResponseController (which
// walks the Unwrap chain), so a handler that type-asserts http.Flusher directly
// still streams through the recorder. A no-op if the underlying writer cannot flush.
func (s *StatusRecorder) Flush() {
	// Best-effort: an underlying writer that cannot flush has nothing to do, so
	// its error is intentionally discarded.
	_ = http.NewResponseController(s.ResponseWriter).Flush()
}

// Hijack forwards to the underlying writer via http.ResponseController for
// libraries that hijack the connection through a direct http.Hijacker assertion.
// Returns an error (e.g. http.ErrNotSupported) when the underlying writer, such
// as an HTTP/2 stream, cannot be hijacked.
func (s *StatusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(s.ResponseWriter).Hijack()
}

// ReadFrom preserves the zero-copy (sendfile) fast path for io.Copy and
// http.ServeContent when the underlying writer implements io.ReaderFrom. Writing
// the body implies a 200 status when WriteHeader was not called.
func (s *StatusRecorder) ReadFrom(src io.Reader) (int64, error) {
	s.wroteHeader = true
	if rf, ok := s.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	return io.Copy(s.ResponseWriter, src)
}
