package sse

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// startServer wraps a hub in an httptest server whose handler applies the
// given serve options.
//
// It uses Go 1.27's httptest.NewTestServer, whose in-memory network removes the
// loopback listener (and with it the ~50 ms per-test Close cost a real one
// carries here). Client() is called eagerly because on the in-memory network
// srv.URL stays "" until the first Client/Start/StartTLS call populates it, and
// every caller below reads srv.URL to build its request.
func startServer(t *testing.T, h *Hub, opts ...ServeOption) *httptest.Server {
	t.Helper()
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.Serve(w, r, opts...)
	}))
	_ = srv.Client()
	return srv
}

// openStream connects to the SSE endpoint and returns the response plus a
// line scanner. Callers must close the response body.
func openStream(t *testing.T, srv *httptest.Server, url string, header http.Header) (*http.Response, *bufio.Scanner) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp, bufio.NewScanner(resp.Body)
}

// readUntil scans lines until pred returns true, failing the test after a
// bounded number of lines (protects against a hung stream; the http client
// has no read deadline here).
func readUntil(t *testing.T, sc *bufio.Scanner, pred func(line string) bool) []string {
	t.Helper()
	var lines []string
	for range 200 {
		if !sc.Scan() {
			t.Fatalf("stream ended early; lines so far: %v", lines)
		}
		line := sc.Text()
		lines = append(lines, line)
		if pred(line) {
			return lines
		}
	}
	t.Fatalf("predicate never satisfied; lines: %v", lines)
	return nil
}

// requireClients asserts the hub's registered client count EXACTLY, inside a
// synctest bubble. It replaces a 5 ms-interval poll against a 5 s wall deadline:
// synctest.Wait returns once every other goroutine in the bubble is durably
// blocked, so registration has provably completed and the count is a fact
// rather than a race the poll was sampling. Under -race that poll was the sse
// package's whole cost — 50 ms per test, ten iterations of a 5 ms sleep.
func requireClients(t *testing.T, h *Hub, want int) {
	t.Helper()
	synctest.Wait()
	if got := h.ClientCount(); got != want {
		t.Fatalf("ClientCount = %d, want %d", got, want)
	}
}

func TestServeHeadersAndHandshake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := NewHub()
		srv := startServer(t, h)
		resp, sc := openStream(t, srv, srv.URL, nil)
		defer resp.Body.Close()

		if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
			t.Errorf("Content-Type = %q", ct)
		}
		if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-transform") {
			t.Errorf("Cache-Control = %q, want no-transform", cc)
		}
		if ab := resp.Header.Get("X-Accel-Buffering"); ab != "no" {
			t.Errorf("X-Accel-Buffering = %q", ab)
		}
		readUntil(t, sc, func(l string) bool { return l == ": connected" })
	})
}

func TestServeDeliversPublishedEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := NewHub()
		srv := startServer(t, h)
		resp, sc := openStream(t, srv, srv.URL, nil)
		defer resp.Body.Close()
		readUntil(t, sc, func(l string) bool { return l == ": connected" })

		requireClients(t, h, 1)
		h.Publish(Event{Name: "notify", Data: []byte(`{"n":1}`)})

		lines := readUntil(t, sc, func(l string) bool { return strings.HasPrefix(l, "data: ") })
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "id: 1") || !strings.Contains(joined, "event: notify") || !strings.Contains(joined, `data: {"n":1}`) {
			t.Errorf("frame lines = %v", lines)
		}
	})
}

func TestServeLastEventIDReplay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := NewHub()
		for i := 1; i <= 3; i++ {
			h.Publish(Event{Data: fmt.Appendf(nil, "e%d", i)})
		}
		srv := startServer(t, h)
		resp, sc := openStream(t, srv, srv.URL, http.Header{"Last-Event-ID": {"1"}})
		defer resp.Body.Close()

		var replayed []string
		readUntil(t, sc, func(l string) bool {
			if data, ok := strings.CutPrefix(l, "data: "); ok {
				replayed = append(replayed, data)
			}
			return l == ": connected" // handshake comes after replay
		})
		if len(replayed) != 2 || replayed[0] != "e2" || replayed[1] != "e3" {
			t.Errorf("replayed = %v, want [e2 e3]", replayed)
		}
	})
}

func TestServeTopicFilter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := NewHub()
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.Serve(w, r, WithTopic(r.URL.Query().Get("topic")))
		}))
		_ = srv.Client() // populates srv.URL; see startServer

		resp, sc := openStream(t, srv, srv.URL+"?topic=a", nil)
		defer resp.Body.Close()
		readUntil(t, sc, func(l string) bool { return l == ": connected" })
		requireClients(t, h, 1)

		h.Publish(Event{Topic: "b", Data: []byte("skip")})
		h.Publish(Event{Topic: "a", Data: []byte("take")})

		lines := readUntil(t, sc, func(l string) bool { return strings.HasPrefix(l, "data: ") })
		if got := lines[len(lines)-1]; got != "data: take" {
			t.Errorf("first delivered = %q, want data: take (topic b must be filtered)", got)
		}
	})
}

func TestServeOnConnectHook(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := NewHub()
		h.Publish(Event{Data: []byte("pre")})
		srv := startServer(t, h, OnConnect(func(w *Writer, b ReplayBounds) error {
			return w.Event(b.Head, "connected", fmt.Appendf(nil, `{"floor":%d,"head":%d}`, b.Floor, b.Head))
		}))
		resp, sc := openStream(t, srv, srv.URL, nil)
		defer resp.Body.Close()

		lines := readUntil(t, sc, func(l string) bool { return strings.HasPrefix(l, "data: {\"floor\"") })
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "event: connected") || !strings.Contains(joined, `{"floor":1,"head":1}`) {
			t.Errorf("handshake lines = %v", lines)
		}
	})
}

func TestServeMaxClients503(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := NewHub(WithMaxClients(1))
		srv := startServer(t, h)
		resp1, sc := openStream(t, srv, srv.URL, nil)
		defer resp1.Body.Close()
		readUntil(t, sc, func(l string) bool { return l == ": connected" })
		requireClients(t, h, 1)

		resp2, _ := openStream(t, srv, srv.URL, nil)
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("second client status = %d, want 503", resp2.StatusCode)
		}
	})
}

func TestServeDraining503(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := NewHub()
		h.Shutdown()
		srv := startServer(t, h)
		resp, sc := openStream(t, srv, srv.URL, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503 while draining", resp.StatusCode)
		}
		if sc.Scan(); !strings.Contains(sc.Text(), "sse_unavailable") {
			t.Errorf("body = %q, want the standard coded envelope", sc.Text())
		}
	})
}

func TestServeShutdownDisconnectsClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := NewHub()
		srv := startServer(t, h)
		resp, sc := openStream(t, srv, srv.URL, nil)
		defer resp.Body.Close()
		readUntil(t, sc, func(l string) bool { return l == ": connected" })
		requireClients(t, h, 1)

		h.Shutdown()
		// Inside the bubble the drain is observable rather than raced: Wait
		// returns once the server goroutine has finished cancelling the client,
		// so the response body is already at EOF and the scan terminates
		// without a deadline. It replaces a scan loop bounded by a 5 s wall
		// clock, which could only say "it ended eventually". A few frame bytes
		// may already be buffered, so drain them; the property is that the
		// stream ENDS, and a still-open stream would block here forever, which
		// the bubble reports as a deadlock rather than a timeout.
		synctest.Wait()
		var tail []string
		for sc.Scan() {
			tail = append(tail, sc.Text())
		}
		if len(tail) > 2 {
			t.Errorf("stream delivered %d lines after Shutdown, want only the trailing frame separator: %q", len(tail), tail)
		}
		if got := h.ClientCount(); got != 0 {
			t.Errorf("ClientCount = %d after Shutdown, want 0", got)
		}
	})
}

// TestServeKeepalive pins the keepalive comment AND its cadence. Inside a
// synctest bubble the clock is synthetic, so the interval is an equality rather
// than a tolerance: on a real clock the test could only assert that a keepalive
// eventually arrived, which passes for a ticker of any period.
func TestServeKeepalive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = 20 * time.Millisecond
		h := NewHub(WithKeepalive(interval))
		srv := startServer(t, h)
		resp, sc := openStream(t, srv, srv.URL, nil)
		defer resp.Body.Close()
		readUntil(t, sc, func(l string) bool { return l == ": connected" })

		start := time.Now()
		readUntil(t, sc, func(l string) bool { return l == ": keepalive" })
		if got := time.Since(start); got != interval {
			t.Errorf("first keepalive after %v, want exactly %v", got, interval)
		}
		readUntil(t, sc, func(l string) bool { return l == ": keepalive" })
		if got := time.Since(start); got != 2*interval {
			t.Errorf("second keepalive after %v, want exactly %v (the ticker must repeat, not fire once)", got, 2*interval)
		}
	})
}

func TestServeNoFlusher500(t *testing.T) {
	h := NewHub()
	rec := &noFlushRecorder{header: make(http.Header)}
	req := httptest.NewRequest(http.MethodGet, "/events", http.NoBody)
	h.Serve(rec, req)
	if rec.status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.status)
	}
}

// noFlushRecorder implements http.ResponseWriter without http.Flusher.
type noFlushRecorder struct {
	header http.Header
	status int
}

func (r *noFlushRecorder) Header() http.Header         { return r.header }
func (r *noFlushRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *noFlushRecorder) WriteHeader(code int)        { r.status = code }

// unwrapOnlyWriter exposes streaming only via Unwrap, never implementing
// http.Flusher itself — the shape of middleware built for
// http.ResponseController discovery. Interface embedding promotes only the
// three ResponseWriter methods, so the wrapper genuinely has no Flush.
type unwrapOnlyWriter struct{ http.ResponseWriter }

func (u *unwrapOnlyWriter) Unwrap() http.ResponseWriter { return u.ResponseWriter }

func TestServeFlushesThroughUnwrapChain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := NewHub()
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.Serve(&unwrapOnlyWriter{ResponseWriter: w}, r)
		}))
		_ = srv.Client() // populates srv.URL; see startServer

		resp, sc := openStream(t, srv, srv.URL, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (flusher reachable via Unwrap must stream)", resp.StatusCode)
		}
		readUntil(t, sc, func(l string) bool { return l == ": connected" })

		requireClients(t, h, 1)
		h.Publish(Event{Data: []byte("via-unwrap")})
		readUntil(t, sc, func(l string) bool { return l == "data: via-unwrap" })
	})
}
