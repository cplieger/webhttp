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

// Wrote reports whether the response has been committed, i.e. WriteHeader, the
// first Write or ReadFrom, or a successful Flush or Hijack has run. Middleware
// such as Recoverer consults it to avoid writing a second status and body onto
// a response a handler has already started, which would corrupt the body and
// mislabel the status.
func (s *StatusRecorder) Wrote() bool {
	return s.wroteHeader
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
	// A successful flush commits the response (net/http writes an implicit 200
	// header when none was set yet), so mark the recorder written to keep Wrote()
	// honest for Recoverer's commit gate. Best-effort: an underlying writer that
	// cannot flush has nothing to do, so its error is intentionally discarded.
	if err := http.NewResponseController(s.ResponseWriter).Flush(); err == nil {
		s.wroteHeader = true
	}
}

// Hijack forwards to the underlying writer via http.ResponseController for
// libraries that hijack the connection through a direct http.Hijacker assertion.
// Returns an error (e.g. http.ErrNotSupported) when the underlying writer, such
// as an HTTP/2 stream, cannot be hijacked.
func (s *StatusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(s.ResponseWriter).Hijack()
	if err == nil {
		// The connection is now the caller's; the ResponseWriter can no longer
		// emit a status/body, so mark it committed to stop Recoverer writing onto it.
		s.wroteHeader = true
	}
	return conn, rw, err
}

// ReadFrom preserves the zero-copy (sendfile) fast path for io.Copy and
// http.ServeContent when the underlying writer implements io.ReaderFrom. Writing
// the body implies a 200 status when WriteHeader was not called.
func (s *StatusRecorder) ReadFrom(src io.Reader) (int64, error) {
	// Delegate first, then commit only when at least one byte was actually
	// written. A zero-byte failure (src returns (0, err) immediately) must leave
	// the response uncommitted so Recoverer can still emit its 500 on a following
	// panic; committing unconditionally before the copy would leak an empty 200.
	// A partial write (n > 0 with an error) has already started the body, so it
	// commits like a normal Write. This only ever sets wroteHeader to true, so an
	// earlier true from WriteHeader/Write is preserved. Mirrors the success-only
	// commit gate on Flush/Hijack.
	var n int64
	var err error
	if rf, ok := s.ResponseWriter.(io.ReaderFrom); ok {
		n, err = rf.ReadFrom(src)
	} else {
		n, err = io.Copy(s.ResponseWriter, src)
	}
	if n > 0 {
		s.wroteHeader = true
	}
	return n, err
}
