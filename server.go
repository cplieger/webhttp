package webhttp

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"time"
)

// Default server and shutdown tunables.
const (
	defaultReadHeaderTimeout = 10 * time.Second
	defaultIdleTimeout       = 120 * time.Second
	defaultMaxHeaderBytes    = 1 << 20
	defaultShutdownGrace     = 5 * time.Second
)

// ServerOption configures the *http.Server built by NewServer.
type ServerOption func(*http.Server)

// WithReadTimeout sets http.Server.ReadTimeout, the deadline for reading the
// entire request. Leave it unset for streaming request bodies.
func WithReadTimeout(d time.Duration) ServerOption {
	return func(s *http.Server) { s.ReadTimeout = d }
}

// WithWriteTimeout sets http.Server.WriteTimeout, the deadline for writing the
// entire response. It is unset by default: streaming apps (SSE, WebSocket, long
// responses) MUST omit it, because it would cut off an in-progress stream.
func WithWriteTimeout(d time.Duration) ServerOption {
	return func(s *http.Server) { s.WriteTimeout = d }
}

// WithIdleTimeout sets http.Server.IdleTimeout, the keep-alive idle deadline.
func WithIdleTimeout(d time.Duration) ServerOption {
	return func(s *http.Server) { s.IdleTimeout = d }
}

// WithReadHeaderTimeout sets http.Server.ReadHeaderTimeout, the slowloris guard
// bounding how long a client may take to send request headers.
func WithReadHeaderTimeout(d time.Duration) ServerOption {
	return func(s *http.Server) { s.ReadHeaderTimeout = d }
}

// WithMaxHeaderBytes sets http.Server.MaxHeaderBytes.
func WithMaxHeaderBytes(n int) ServerOption {
	return func(s *http.Server) { s.MaxHeaderBytes = n }
}

// WithErrorLog sets http.Server.ErrorLog so connection-level errors go to the
// caller's logger instead of the standard logger. Wire it to slog with
// slog.NewLogLogger(handler, slog.LevelError).
func WithErrorLog(l *log.Logger) ServerOption { return func(s *http.Server) { s.ErrorLog = l } }

// NewServer builds an *http.Server for handler with streaming-safe defaults:
// ReadHeaderTimeout 10s (a slowloris guard), IdleTimeout 120s, MaxHeaderBytes
// 1 MiB, and ReadTimeout/WriteTimeout left unset (0) so SSE, WebSocket, and
// other long-lived responses work out of the box. Options override the
// defaults.
//
// Because ReadTimeout and WriteTimeout are unset by default, only header
// reading is time-bounded (by ReadHeaderTimeout); a slow request BODY is not.
// A non-streaming handler should add WithReadTimeout to bound slowloris-style
// slow bodies. A streaming handler, which cannot use a whole-request timeout,
// should instead apply per-request deadlines via
// http.ResponseController.SetReadDeadline/SetWriteDeadline. Note that
// MaxBytesReader (see LimitBody) bounds body SIZE, not the time taken to send
// it.
func NewServer(handler http.Handler, opts ...ServerOption) *http.Server {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		IdleTimeout:       defaultIdleTimeout,
		MaxHeaderBytes:    defaultMaxHeaderBytes,
	}
	for _, o := range opts {
		if o != nil {
			o(srv)
		}
	}
	return srv
}

// runConfig holds resolved Run configuration.
type runConfig struct {
	preDrain      func(ctx context.Context)
	shutdownGrace time.Duration
}

// RunOption configures Run.
type RunOption func(*runConfig)

// WithShutdownGrace sets how long Run allows for graceful shutdown: the window
// for the pre-drain hook to run, for in-flight requests to finish, and for the
// onShutdown teardown to run. Defaults to 5s.
func WithShutdownGrace(d time.Duration) RunOption {
	return func(c *runConfig) { c.shutdownGrace = d }
}

// WithPreDrain registers a hook Run invokes after ctx is cancelled and
// strictly before srv.Shutdown starts draining in-flight requests. It is the
// place for the release-the-streams phase a graceful stop needs ahead of the
// drain: flip a readiness gate so a load balancer stops routing here, cancel
// the server's BaseContext or shut down an SSE hub so long-lived connections
// end (otherwise Shutdown waits its full grace period for them). fn receives
// a context bounded by the SAME shutdown deadline that Shutdown and
// onShutdown share; whatever budget it spends is no longer available to
// them. A nil fn is ignored.
func WithPreDrain(fn func(ctx context.Context)) RunOption {
	return func(c *runConfig) { c.preDrain = fn }
}

// Run serves srv on ln until ctx is cancelled, then shuts down gracefully.
//
// It starts srv.Serve(ln) in a goroutine (treating http.ErrServerClosed as a
// clean stop) and blocks until either ctx is cancelled or Serve returns on its
// own. On cancellation it computes a single shutdown deadline (now + the
// shutdown grace period) and runs the shutdown sequence against it: first the
// WithPreDrain hook if one is registered (readiness flips, stream releases —
// see WithPreDrain), then srv.Shutdown with a context bounded by the deadline,
// then, if onShutdown is non-nil, onShutdown with a context bounded by that
// SAME deadline: each later phase runs within whatever grace budget REMAINS
// after the earlier ones, not a fresh full window. Run returns the first
// non-ErrServerClosed error it observes (a serve error, else a shutdown error),
// or nil on a clean graceful stop.
//
// The caller binds ln up front (for example with net.ListenConfig.Listen) so a
// port-in-use error surfaces synchronously before Run is called, and passes
// application teardown as onShutdown.
func Run(ctx context.Context, srv *http.Server, ln net.Listener, onShutdown func(ctx context.Context), opts ...RunOption) error {
	c := &runConfig{shutdownGrace: defaultShutdownGrace}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}

	serveErr := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		// Serve returned before shutdown was requested (a fatal serve error;
		// ErrServerClosed was already normalized to nil).
		return err
	case <-ctx.Done():
	}

	deadline := time.Now().Add(c.shutdownGrace)
	if c.preDrain != nil {
		preCtx, preCancel := context.WithDeadline(context.Background(), deadline)
		c.preDrain(preCtx)
		preCancel()
	}
	shutdownCtx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)

	if onShutdown != nil {
		teardownCtx, teardownCancel := context.WithDeadline(context.Background(), deadline)
		defer teardownCancel()
		onShutdown(teardownCtx)
	}

	// Serve returns ErrServerClosed (normalized to nil) once Shutdown runs. A
	// real serve error takes precedence over the shutdown error.
	if err := <-serveErr; err != nil {
		return err
	}
	return shutdownErr
}
