package webhttp

import "net/http"

// StatusRecorder wraps an http.ResponseWriter to capture the response status
// code while remaining transparent to http.ResponseController. Middleware that
// needs to observe the final status (access logging, metrics) wraps the writer
// in a StatusRecorder; the Unwrap method lets http.NewResponseController reach
// the underlying writer, so Flush and Hijack still work for SSE, WebSocket, and
// other streaming handlers running behind the middleware.
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
