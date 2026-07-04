package webhttp_test

import (
	"context"
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
