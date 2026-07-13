package webhttp_test

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
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
		if got, want := err.Error(), context.DeadlineExceeded.Error(); got != want {
			t.Fatalf("Run error = %q, want %q", got, want)
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
