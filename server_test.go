package webhttp_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/webhttp"
)

func TestNewServer_defaults(t *testing.T) {
	h := http.NewServeMux()
	srv := webhttp.NewServer(h)

	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 10s", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout != 120*time.Second {
		t.Errorf("IdleTimeout = %v, want 120s", srv.IdleTimeout)
	}
	if srv.MaxHeaderBytes != 1<<20 {
		t.Errorf("MaxHeaderBytes = %d, want %d", srv.MaxHeaderBytes, 1<<20)
	}
	if srv.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, want 0 (streaming-safe)", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 (streaming-safe)", srv.WriteTimeout)
	}
	if srv.Handler != h {
		t.Error("Handler was not set to the provided handler")
	}
}

func TestNewServer_optionsOverride(t *testing.T) {
	srv := webhttp.NewServer(nil,
		webhttp.WithReadTimeout(1*time.Second),
		webhttp.WithWriteTimeout(2*time.Second),
		webhttp.WithIdleTimeout(3*time.Second),
		webhttp.WithReadHeaderTimeout(4*time.Second),
		webhttp.WithMaxHeaderBytes(512),
	)
	if srv.ReadTimeout != 1*time.Second {
		t.Errorf("ReadTimeout = %v, want 1s", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 2*time.Second {
		t.Errorf("WriteTimeout = %v, want 2s", srv.WriteTimeout)
	}
	if srv.IdleTimeout != 3*time.Second {
		t.Errorf("IdleTimeout = %v, want 3s", srv.IdleTimeout)
	}
	if srv.ReadHeaderTimeout != 4*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 4s", srv.ReadHeaderTimeout)
	}
	if srv.MaxHeaderBytes != 512 {
		t.Errorf("MaxHeaderBytes = %d, want 512", srv.MaxHeaderBytes)
	}
}

func TestWithErrorLog_setsServerErrorLog(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	srv := webhttp.NewServer(nil, webhttp.WithErrorLog(logger))
	if srv.ErrorLog != logger {
		t.Error("WithErrorLog did not set http.Server.ErrorLog")
	}
}

func TestNewServer_nilOptionIgnored(t *testing.T) {
	srv := webhttp.NewServer(nil, nil) // must not panic
	if srv == nil {
		t.Fatal("NewServer returned nil")
	}
	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want default 10s", srv.ReadHeaderTimeout)
	}
}

// runAndShutdown starts Run on a fresh loopback listener, confirms it serves a
// request, cancels the context, and returns Run's result. It fails the test if
// onShutdown was not called.
func runAndShutdown(t *testing.T, opts ...webhttp.RunOption) error {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var shutdownCalled atomic.Bool
	srv := webhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- webhttp.Run(ctx, srv, ln, func(context.Context) { shutdownCalled.Store(true) }, opts...)
	}()

	addr := ln.Addr().String()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/")
	if err != nil {
		cancel()
		t.Fatalf("get while serving: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("serving status = %d, want 200", resp.StatusCode)
	}

	cancel()
	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
	if !shutdownCalled.Load() {
		t.Error("onShutdown was not called during graceful shutdown")
	}
	return runErr
}

func TestRun_gracefulShutdown(t *testing.T) {
	if err := runAndShutdown(t); err != nil {
		t.Errorf("Run = %v, want nil on graceful shutdown", err)
	}
}

func TestRun_withShutdownGraceOption(t *testing.T) {
	if err := runAndShutdown(t, webhttp.WithShutdownGrace(2*time.Second)); err != nil {
		t.Errorf("Run = %v, want nil", err)
	}
}

func TestRun_nilOptionIgnored(t *testing.T) {
	if err := runAndShutdown(t, nil); err != nil {
		t.Errorf("Run with nil option = %v, want nil", err)
	}
}

func TestRun_serveErrorReturned(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Close the listener so Serve fails immediately with a non-ErrServerClosed
	// error; Run must surface it rather than block on the (never-cancelled)
	// context.
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	srv := webhttp.NewServer(nil)

	runErr := webhttp.Run(context.Background(), srv, ln, nil)
	if runErr == nil {
		t.Error("Run = nil, want a serve error from the closed listener")
	}
}

// closedListener returns a listener Serve fails on immediately, the cheapest
// stand-in for a fatal serve error (a dead accept loop).
func closedListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return ln
}

// The default fatal path is unchanged: a serve error returns from Run with
// NEITHER graceful hook invoked. This is the behavior WithServeExit is opt-in
// against, so it is pinned rather than assumed.
func TestRun_fatalServeErrorSkipsGracefulHooksByDefault(t *testing.T) {
	var preDrainCalled, teardownCalled atomic.Bool
	runErr := webhttp.Run(context.Background(), webhttp.NewServer(nil), closedListener(t),
		func(context.Context) { teardownCalled.Store(true) },
		webhttp.WithPreDrain(func(context.Context) { preDrainCalled.Store(true) }))

	if runErr == nil {
		t.Fatal("Run = nil, want a serve error from the closed listener")
	}
	if preDrainCalled.Load() {
		t.Error("pre-drain hook ran on a fatal serve error; it is documented for the graceful path only")
	}
	if teardownCalled.Load() {
		t.Error("onShutdown ran on a fatal serve error; it is documented for the graceful path only")
	}
}

// WithServeExit is the opt-in teardown for that path: it runs, it runs before
// Run returns, and it gets a context bounded by the configured grace.
func TestRun_serveExitRunsOnFatalServeError(t *testing.T) {
	const grace = 2 * time.Second
	var (
		called            atomic.Bool
		preDrainCalled    atomic.Bool
		teardownCalled    atomic.Bool
		deadline          time.Time
		hasDeadline       bool
		ctxLiveInsideHook bool
	)
	appCtx := t.Context()
	start := time.Now()
	// Run calls the hook inline on its own goroutine — the test's, here — so
	// these are plain same-goroutine writes.
	serveExit := func(ctx context.Context) {
		called.Store(true)
		deadline, hasDeadline = ctx.Deadline()
		ctxLiveInsideHook = appCtx.Err() == nil
	}

	runErr := webhttp.Run(appCtx, webhttp.NewServer(nil), closedListener(t),
		func(context.Context) { teardownCalled.Store(true) },
		webhttp.WithShutdownGrace(grace),
		webhttp.WithPreDrain(func(context.Context) { preDrainCalled.Store(true) }),
		webhttp.WithServeExit(serveExit))

	if runErr == nil {
		t.Fatal("Run = nil, want the serve error; the hook must not swallow it")
	}
	if !called.Load() {
		t.Fatal("serve-exit hook did not run on a fatal serve error")
	}
	if !hasDeadline {
		t.Fatal("serve-exit context has no deadline; it must be bounded by the shutdown grace")
	}
	if span := deadline.Sub(start); span <= 0 || span > grace+250*time.Millisecond {
		t.Errorf("serve-exit deadline is %v out, want ~%v (the configured grace)", span, grace)
	}
	// The hook, not the graceful sequence: pre-drain and onShutdown stay
	// unrun, and the caller's own context is still live (nothing cancelled it),
	// which is why a teardown that waits on it must cancel it itself.
	if preDrainCalled.Load() || teardownCalled.Load() {
		t.Error("graceful hooks ran alongside the serve-exit hook; exactly one path must run")
	}
	if !ctxLiveInsideHook {
		t.Error("the caller's context was cancelled on the fatal path; WithServeExit documents it as still live")
	}
}

// Serve also returns on its own when the caller shuts the server down outside
// Run. That is the other half of the exactly-one-path contract: the graceful
// sequence never ran, so the serve-exit hook is what tears down.
func TestRun_serveExitRunsWhenCallerClosesServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := webhttp.NewServer(okHandler())
	var called, teardownCalled atomic.Bool

	done := make(chan error, 1)
	go func() {
		// context.Background(): nothing ever cancels the run, so only the
		// caller's own Shutdown can end it.
		done <- webhttp.Run(context.Background(), srv, ln,
			func(context.Context) { teardownCalled.Store(true) },
			webhttp.WithServeExit(func(context.Context) { called.Store(true) }))
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("get while serving: %v", err)
	}
	_ = resp.Body.Close()

	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("caller-driven Shutdown: %v", err)
	}

	select {
	case runErr := <-done:
		if runErr != nil {
			t.Errorf("Run = %v, want nil (ErrServerClosed is a clean stop)", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the caller shut the server down")
	}
	if !called.Load() {
		t.Error("serve-exit hook did not run when Serve returned on its own")
	}
	if teardownCalled.Load() {
		t.Error("onShutdown ran without a graceful stop; only the serve-exit hook covers this path")
	}
}

// On the graceful path the serve-exit hook must stay unrun: the drain sequence
// already carries the teardown, and running both would tear down twice.
func TestRun_serveExitNotRunOnGracefulPath(t *testing.T) {
	var serveExitCalled atomic.Bool
	var preDrainCalled atomic.Bool
	err := runAndShutdown(t,
		webhttp.WithPreDrain(func(context.Context) { preDrainCalled.Store(true) }),
		webhttp.WithServeExit(func(context.Context) { serveExitCalled.Store(true) }))
	if err != nil {
		t.Errorf("Run = %v, want nil on graceful shutdown", err)
	}
	if !preDrainCalled.Load() {
		t.Error("pre-drain hook did not run on the graceful path")
	}
	if serveExitCalled.Load() {
		t.Error("serve-exit hook ran on the graceful path; onShutdown already covers it")
	}
}

func TestRun_nilServeExitIgnored(t *testing.T) {
	// The fatal path with WithServeExit(nil): the nil hook is skipped, not
	// called, and the serve error still comes back.
	runErr := webhttp.Run(context.Background(), webhttp.NewServer(nil), closedListener(t), nil,
		webhttp.WithServeExit(nil))
	if runErr == nil {
		t.Error("Run = nil, want the serve error")
	}
}

func TestRun_onShutdownNilIsSafe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := webhttp.NewServer(okHandler())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- webhttp.Run(ctx, srv, ln, nil) }() // nil onShutdown

	// Give Serve a moment, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation with nil onShutdown")
	}
}

func TestRun_slowOnShutdownStillRunsWithinGrace(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := webhttp.NewServer(okHandler())

	var teardownDone atomic.Bool
	onShutdown := func(ctx context.Context) {
		// A teardown that takes real time must still complete: the shared grace
		// budget gives it room after Shutdown returns.
		select {
		case <-time.After(150 * time.Millisecond):
			teardownDone.Store(true)
		case <-ctx.Done():
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- webhttp.Run(ctx, srv, ln, onShutdown, webhttp.WithShutdownGrace(2*time.Second))
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if !teardownDone.Load() {
		t.Error("slow onShutdown did not complete within the shared grace budget")
	}
}

func TestRun_holdsRequestOpenAcrossShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	const (
		grace    = 2 * time.Second
		blockFor = 400 * time.Millisecond
	)
	started := make(chan struct{})
	srv := webhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(blockFor) // remain in-flight so Shutdown must wait for us
		w.WriteHeader(http.StatusOK)
	}))

	var (
		teardownDL    time.Time
		teardownHasDL bool
		teardownRan   atomic.Bool
	)
	onShutdown := func(ctx context.Context) {
		teardownDL, teardownHasDL = ctx.Deadline()
		teardownRan.Store(true)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- webhttp.Run(ctx, srv, ln, onShutdown, webhttp.WithShutdownGrace(grace))
	}()

	addr := ln.Addr().String()
	statusCh := make(chan int, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get("http://" + addr + "/")
		if err != nil {
			statusCh <- 0
			return
		}
		defer func() { _ = resp.Body.Close() }()
		statusCh <- resp.StatusCode
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("handler never became in-flight")
	}

	t0 := time.Now()
	cancel() // request is in-flight; graceful shutdown must let it finish

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	if runErr != nil {
		t.Errorf("Run = %v, want nil", runErr)
	}

	select {
	case code := <-statusCh:
		if code != http.StatusOK {
			t.Errorf("in-flight request status = %d, want 200 (held open across shutdown)", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request never completed")
	}

	if !teardownRan.Load() || !teardownHasDL {
		t.Fatal("onShutdown did not run with a deadline")
	}
	// One shared budget: the teardown deadline sits ~grace from when shutdown
	// began (t0), even though Shutdown first spent ~blockFor draining the
	// in-flight request. A per-phase timeout would push it out to ~grace+blockFor.
	if span := teardownDL.Sub(t0); span > grace+250*time.Millisecond {
		t.Errorf("teardown deadline is %v after shutdown start, want ~%v (shared budget, not ~%v)",
			span, grace, grace+blockFor)
	}
}

func TestRun_returnsShutdownDeadlineExceeded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var released atomic.Bool
	releaseHandler := func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}

	srv := webhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() {
		releaseHandler()
		_ = srv.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- webhttp.Run(ctx, srv, ln, nil, webhttp.WithShutdownGrace(25*time.Millisecond))
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	requestDone := make(chan error, 1)
	go func() {
		resp, err := client.Get("http://" + ln.Addr().String() + "/")
		if err == nil {
			_ = resp.Body.Close()
		}
		requestDone <- err
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("handler never became in-flight")
	}

	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run = nil, want shutdown deadline exceeded when in-flight requests outlive the grace period")
		}
		// Both facts hold: the wrapped context.DeadlineExceeded (the pre-existing
		// contract) and the origin marker that says WHICH deadline it was.
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Run error = %v, want it to wrap context.DeadlineExceeded", err)
		}
		if !errors.Is(err, webhttp.ErrShutdownGraceExpired) {
			t.Errorf("Run error = %v, want it to wrap ErrShutdownGraceExpired", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after shutdown grace expired")
	}

	releaseHandler()
	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request did not finish after release")
	}
}

func TestRun_preDrainRunsBeforeShutdownDrain(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	// The handler blocks until the pre-drain hook releases it, so the test
	// discriminates on ordering: pre-drain before Shutdown lets the drain
	// finish and Run return nil; pre-drain after Shutdown would leave the
	// request in-flight for the whole grace window and Run would return
	// context.DeadlineExceeded.
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := webhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
	}))

	var (
		mu    sync.Mutex
		order []string
	)
	record := func(phase string) {
		mu.Lock()
		order = append(order, phase)
		mu.Unlock()
	}

	preDrain := func(ctx context.Context) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("pre-drain context has no deadline")
		}
		record("pre-drain")
		close(release)
	}
	onShutdown := func(context.Context) { record("teardown") }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- webhttp.Run(ctx, srv, ln, onShutdown,
			webhttp.WithPreDrain(preDrain), webhttp.WithShutdownGrace(2*time.Second))
	}()

	statusCh := make(chan int, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			statusCh <- 0
			return
		}
		defer func() { _ = resp.Body.Close() }()
		statusCh <- resp.StatusCode
	}()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("handler never became in-flight")
	}

	cancel() // request is in-flight; only the pre-drain hook can unblock it

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if runErr != nil {
		t.Errorf("Run = %v, want nil (pre-drain must run before the drain)", runErr)
	}

	select {
	case code := <-statusCh:
		if code != http.StatusOK {
			t.Errorf("in-flight request status = %d, want 200", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request never completed")
	}

	mu.Lock()
	defer mu.Unlock()
	if want := []string{"pre-drain", "teardown"}; !slices.Equal(order, want) {
		t.Errorf("phase order = %v, want %v", order, want)
	}
}

func TestRun_nilPreDrainIgnored(t *testing.T) {
	if err := runAndShutdown(t, webhttp.WithPreDrain(nil)); err != nil {
		t.Errorf("Run with WithPreDrain(nil) = %v, want nil", err)
	}
}

func TestWithSlogErrorLog_bridgesNetHTTPLinesIntoSlog(t *testing.T) {
	// slog.Default is process-global, so this test must not run in parallel.
	capture := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })

	srv := webhttp.NewServer(nil, webhttp.WithSlogErrorLog(slog.LevelWarn))
	if srv.ErrorLog == nil {
		t.Fatal("WithSlogErrorLog did not set http.Server.ErrorLog")
	}
	// Stand in for net/http's own connection-level line.
	srv.ErrorLog.Print("http: Accept error: too many open files; retrying")

	records := capture.snapshot()
	if len(records) != 1 {
		t.Fatalf("captured %d records, want 1", len(records))
	}
	if records[0].Level != slog.LevelWarn {
		t.Errorf("level = %v, want %v (the caller's chosen level)", records[0].Level, slog.LevelWarn)
	}
	if !strings.Contains(records[0].Message, "Accept error") {
		t.Errorf("message = %q, want it to carry net/http's line", records[0].Message)
	}
}

func TestWithSlogErrorLog_lastAppliedWins(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(&captureHandler{}))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// WithErrorLog remains the override for a custom logger.
	custom := log.New(io.Discard, "", 0)
	srv := webhttp.NewServer(nil, webhttp.WithSlogErrorLog(slog.LevelError), webhttp.WithErrorLog(custom))
	if srv.ErrorLog != custom {
		t.Error("WithErrorLog did not override an earlier WithSlogErrorLog")
	}
}

func TestNewServer_errorLogDefaultUnchanged(t *testing.T) {
	// Neither option passed: the default stays net/http's standard logger (nil),
	// so the additions change no existing default.
	if srv := webhttp.NewServer(nil); srv.ErrorLog != nil {
		t.Errorf("ErrorLog = %v, want nil (net/http's standard logger)", srv.ErrorLog)
	}
}

func TestAwaitDone_reportsCompletion(t *testing.T) {
	done := make(chan struct{})
	close(done)

	if !webhttp.AwaitDone(t.Context(), done) {
		t.Error("AwaitDone = false, want true when done is already closed")
	}
}

func TestAwaitDone_reportsExpiry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if webhttp.AwaitDone(ctx, make(chan struct{})) {
		t.Error("AwaitDone = true, want false when the budget runs out first")
	}
}

func TestAwaitDone_completionWinsWhenBothReadyAtOnce(t *testing.T) {
	// The case the post-expiry recheck exists for: a drain that consumed the
	// whole grace hands the teardown an ALREADY-EXPIRED context, so both select
	// cases are ready and the choice is pseudo-random. Without the recheck this
	// reports a teardown that DID finish as still running, roughly half the time.
	done := make(chan struct{})
	close(done)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for i := range 500 {
		if !webhttp.AwaitDone(ctx, done) {
			t.Fatalf("AwaitDone = false on iteration %d; a completion that landed with the expiry must win", i)
		}
	}
}

func TestAwaitDone_nilChannelIsBoundedByContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if webhttp.AwaitDone(ctx, nil) {
		t.Error("AwaitDone = true, want false: a nil channel never becomes ready")
	}
}

func TestAwaitDone_waitsForALateCompletion(t *testing.T) {
	done := make(chan struct{})
	go func() {
		time.Sleep(30 * time.Millisecond)
		close(done)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if !webhttp.AwaitDone(ctx, done) {
		t.Error("AwaitDone = false, want true: it must block for a completion still inside the budget")
	}
}

func TestCausedByCancellation(t *testing.T) {
	liveCtx := t.Context()

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	expiredCtx, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	// A cause that does NOT wrap context.Canceled: the shape net/http surfaces
	// verbatim, which a ctx.Err()-only check would miss.
	customCause := errors.New("listener torn down by the supervisor")
	causeCtx, cancelCause := context.WithCancelCause(context.Background())
	cancelCause(customCause)
	defer cancelCause(nil)

	var nilCtx context.Context

	tests := map[string]struct {
		ctx  context.Context
		err  error
		want bool
	}{
		"live context, real fault":         {liveCtx, errors.New("bind: address already in use"), false},
		"live context, canceled-ish error": {liveCtx, context.Canceled, false},
		"cancelled context, nil error":     {cancelledCtx, nil, false},
		"cancelled context, its own error": {cancelledCtx, context.Canceled, true},
		"cancelled context, wrapped":       {cancelledCtx, fmt.Errorf("binding :9190: %w", context.Canceled), true},
		"cancelled context, real fault":    {cancelledCtx, errors.New("bind: address already in use"), false},
		"expired context, its own error":   {expiredCtx, fmt.Errorf("drain: %w", context.DeadlineExceeded), true},
		"expired context, real fault":      {expiredCtx, errors.New("bind: address already in use"), false},
		"cause-only error":                 {causeCtx, fmt.Errorf("serve: %w", customCause), true},
		"cause context, unrelated fault":   {causeCtx, errors.New("bind: address already in use"), false},
		"nil context":                      {nilCtx, context.Canceled, false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := webhttp.CausedByCancellation(tc.ctx, tc.err); got != tc.want {
				t.Errorf("CausedByCancellation() = %v, want %v", got, tc.want)
			}
		})
	}
}

// fatalAcceptListener substitutes its own fatal error for whatever the wrapped
// listener's Accept reports, and signals when Serve returns (Serve closes the
// listener through a deferred Close on its way out, after it has already decided
// which error to return). That signal is what makes the precedence test
// deterministic: the pre-drain hook can wait for Serve to have committed to the
// fatal error BEFORE Run reaches srv.Shutdown, which is the only window in which
// net/http reports a serve error rather than ErrServerClosed.
type fatalAcceptListener struct {
	net.Listener
	fatal      error
	serveDone  chan struct{}
	closedOnce sync.Once
}

func (l *fatalAcceptListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, l.fatal
	}
	return c, nil
}

func (l *fatalAcceptListener) Close() error {
	l.closedOnce.Do(func() { close(l.serveDone) })
	return l.Listener.Close()
}

// A real Serve error still takes precedence over the shutdown error, and stays
// matchable: the grace-expiry marker must not displace it, and must not be
// attached to it.
func TestRun_serveErrorTakesPrecedenceOverGraceExpiry(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fatal := errors.New("accept loop is gone")
	ln := &fatalAcceptListener{Listener: inner, fatal: fatal, serveDone: make(chan struct{})}

	entered := make(chan struct{})
	release := make(chan struct{})
	var released atomic.Bool
	releaseHandler := func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}
	t.Cleanup(releaseHandler)

	// The handler stays in-flight so srv.Shutdown cannot finish inside the grace,
	// which is what makes the shutdown error a grace expiry.
	srv := webhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() { _ = srv.Close() })

	preDrain := func(context.Context) {
		// Break the accept loop while the server is not yet shutting down, so
		// Serve returns the fatal error instead of ErrServerClosed, and wait until
		// it has done so.
		_ = inner.Close()
		select {
		case <-ln.serveDone:
		case <-time.After(2 * time.Second):
			t.Error("Serve did not return after the accept loop broke")
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- webhttp.Run(ctx, srv, ln, nil,
			webhttp.WithShutdownGrace(100*time.Millisecond), webhttp.WithPreDrain(preDrain))
	}()

	client := &http.Client{Timeout: 5 * time.Second}
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		resp, err := client.Get("http://" + inner.Addr().String() + "/")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("handler never became in-flight")
	}

	cancel()

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}

	if !errors.Is(runErr, fatal) {
		t.Fatalf("Run = %v, want the serve error %v (it takes precedence and stays matchable)", runErr, fatal)
	}
	if errors.Is(runErr, webhttp.ErrShutdownGraceExpired) {
		t.Error("the serve error was marked as a grace expiry; the marker belongs to the shutdown error only")
	}

	releaseHandler()
	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request did not finish after release")
	}
}

func TestRun_cleanShutdownIsNotMarkedAsGraceExpiry(t *testing.T) {
	err := runAndShutdown(t, webhttp.WithShutdownGrace(2*time.Second))
	if err != nil {
		t.Fatalf("Run = %v, want nil on a clean graceful stop", err)
	}
	if errors.Is(err, webhttp.ErrShutdownGraceExpired) {
		t.Error("a clean stop was marked as a grace expiry")
	}
}
