package webhttp_test

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
)

// captureHandler is a slog.Handler that records every emitted record for
// assertions. It is safe for concurrent use.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// snapshot returns a copy of the records captured so far.
func (h *captureHandler) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]slog.Record, len(h.records))
	copy(out, h.records)
	return out
}

// attrsOf collects a record's attributes into a map keyed by attribute name.
func attrsOf(r slog.Record) map[string]any {
	m := make(map[string]any, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.Any()
		return true
	})
	return m
}

// discardLogger returns a logger that drops everything, for tests that exercise
// a code path whose log output is not under assertion.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// okHandler responds 200 with no body.
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// statusHandler responds with the given status code and no body.
func statusHandler(code int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
	})
}

// opaqueWriter wraps an http.ResponseWriter WITHOUT an Unwrap method: it stands
// in for third-party middleware that hides the writer net/http passed to the
// handler. LimitBody's Unwrap walk stops at it, which is why the too-large
// connection-close signal cannot reach net/http through such a wrapper.
type opaqueWriter struct {
	http.ResponseWriter
}

// unwrappableWriter wraps an http.ResponseWriter and exposes it through Unwrap,
// the http.ResponseController convention webhttp's own StatusRecorder follows.
// LimitBody walks through it.
type unwrappableWriter struct {
	http.ResponseWriter
}

func (u *unwrappableWriter) Unwrap() http.ResponseWriter { return u.ResponseWriter }

// selfUnwrappingWriter returns ITSELF from Unwrap: a buggy (or adversarial)
// wrapper whose chain never ends. It exists to pin that LimitBody's bounded
// walk terminates instead of spinning.
type selfUnwrappingWriter struct {
	http.ResponseWriter
}

func (s *selfUnwrappingWriter) Unwrap() http.ResponseWriter { return s }

// nilUnwrappingWriter returns a nil writer from Unwrap. The walk must keep the
// last real writer rather than handing nil onward.
type nilUnwrappingWriter struct {
	http.ResponseWriter
}

func (n *nilUnwrappingWriter) Unwrap() http.ResponseWriter { return nil }

// pipeListener is an in-memory net.Listener over net.Pipe, plus the client that
// dials it. It exists so a Run test can live inside a testing/synctest bubble.
//
// The bubble is the point, and a real listener forecloses it: measured on
// go1.27.0, Run over a loopback listener inside a bubble HANGS to the go-test
// timeout, its Serve goroutine parked in internal/poll.runtime_pollWait at
// net.(*TCPListener).Accept and reported as "[IO wait, synctest bubble 1]".
// Accept on a socket is not a DURABLY blocking operation, so the synthetic clock
// can never advance. A channel receive is, which is the whole difference here.
//
// Go 1.27's httptest.NewTestServer solves the same problem for a handler test,
// but its in-memory listener lives in net/http/internal/nettest and is not
// exported, and Run takes a net.Listener rather than building its own. So this
// is the reachable equivalent, deliberately the same shape.
type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

// dial hands the listener one end of a fresh pipe and returns the other.
func (l *pipeListener) dial() (net.Conn, error) {
	server, client := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.closed:
		_ = server.Close()
		_ = client.Close()
		return nil, net.ErrClosed
	}
}

// client returns an http.Client whose every request reaches this listener,
// whatever host it names.
func (l *pipeListener) client() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) { return l.dial() },
	}}
}

// pipeAddr is the address of an in-memory pipe listener. The value is never
// dialed — pipeListener.client bypasses address resolution entirely — so it only
// has to be a stable, printable net.Addr.
type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }
