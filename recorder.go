package webhttp

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
)

// StatusRecorder wraps an http.ResponseWriter to record the response status
// for middleware such as access logging and metrics.
//
// It implements http.Flusher, FlushError, http.Hijacker and io.ReaderFrom
// itself, each returning the underlying writer's result (a passed write
// deadline, http.ErrNotSupported on an HTTP/2 hijack), so type assertions,
// http.NewResponseController and the sendfile fast path work through it.
// Unwrap reaches the rest of the underlying writer, such as its deadlines.
type StatusRecorder struct {
	http.ResponseWriter
	status       int
	wroteHeader  bool
	hijackedBare bool
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
// first Write or ReadFrom, a Flush that returned anything but
// http.ErrNotSupported (a failed flush included), or a successful Hijack has run. Middleware such as Recoverer consults it to avoid
// writing a second status and body onto a response a handler has already
// started, which would corrupt the body and mislabel the status.
func (s *StatusRecorder) Wrote() bool {
	return s.wroteHeader
}

// Unwrap returns the wrapped http.ResponseWriter, so http.NewResponseController
// reaches the optional methods the recorder does not implement itself, such as
// SetWriteDeadline and EnableFullDuplex.
func (s *StatusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

// Flush is FlushError for callers that type-assert http.Flusher, which has no
// way to report the error.
func (s *StatusRecorder) Flush() { _ = s.FlushError() }

// FlushError flushes the underlying writer through http.ResponseController and
// returns its error: http.ErrNotSupported when no writer below could flush, or
// the connection's own failure, such as os.ErrDeadlineExceeded. Any other result
// commits the response, because net/http latches its implicit 200 before it
// flushes. A wrapper that writes before reporting http.ErrNotSupported is not
// seen as committed, which is why Gzip belongs inside Recoverer.
func (s *StatusRecorder) FlushError() error {
	err := http.NewResponseController(s.ResponseWriter).Flush()
	// errors.Is, not ==: ResponseController wraps the sentinel.
	if !errors.Is(err, http.ErrNotSupported) {
		s.wroteHeader = true
	}
	return err
}

// Hijack forwards to the underlying writer via http.ResponseController for
// libraries that hijack the connection through a direct http.Hijacker assertion.
// Returns an error (e.g. http.ErrNotSupported) when the underlying writer, such
// as an HTTP/2 stream, cannot be hijacked.
func (s *StatusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(s.ResponseWriter).Hijack()
	if err == nil {
		// Latch before the commit below erases it: a hijack with nothing written
		// means net/http sent nothing and the handler owns the wire
		// (gorilla/websocket writes its own 101), so the default 200 describes no
		// response. switchedProtocol reads this; Status() keeps the default for
		// callers that want a status.
		s.hijackedBare = !s.wroteHeader
		// The connection is now the caller's; the ResponseWriter can no longer
		// emit a status/body, so mark it committed to stop Recoverer writing onto it.
		s.wroteHeader = true
	}
	return conn, rw, err
}

// switchedProtocol reports whether the response left HTTP request/response
// framing instead of completing an exchange: it recorded 101 Switching
// Protocols (the handshake went through this writer, as coder/websocket's
// WriteHeader(101)-then-Hijack does), or it was hijacked before anything was
// recorded (the handler wrote the handshake onto the connection itself). It is
// the FACT behind RequestLogger's WithSkipUpgrades, which owns the policy of
// what to do about it; nothing here decides whether such a response is logged.
func (s *StatusRecorder) switchedProtocol() bool {
	return s.status == http.StatusSwitchingProtocols || s.hijackedBare
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
	// earlier true from WriteHeader/Write is preserved.
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
