package sse

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// startServer wraps a hub in an httptest server whose handler applies the
// given serve options.
func startServer(t *testing.T, h *Hub, opts ...ServeOption) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.Serve(w, r, opts...)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// openStream connects to the SSE endpoint and returns the response plus a
// line scanner. Callers must close the response body.
func openStream(t *testing.T, url string, header http.Header) (*http.Response, *bufio.Scanner) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
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

func waitForClients(t *testing.T, h *Hub, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.ClientCount() != want {
		if time.Now().After(deadline) {
			t.Fatalf("ClientCount = %d, want %d", h.ClientCount(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestServeHeadersAndHandshake(t *testing.T) {
	h := NewHub()
	srv := startServer(t, h)
	resp, sc := openStream(t, srv.URL, nil)
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
}

func TestServeDeliversPublishedEvents(t *testing.T) {
	h := NewHub()
	srv := startServer(t, h)
	resp, sc := openStream(t, srv.URL, nil)
	defer resp.Body.Close()
	readUntil(t, sc, func(l string) bool { return l == ": connected" })

	waitForClients(t, h, 1)
	h.Publish(Event{Name: "notify", Data: []byte(`{"n":1}`)})

	lines := readUntil(t, sc, func(l string) bool { return strings.HasPrefix(l, "data: ") })
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "id: 1") || !strings.Contains(joined, "event: notify") || !strings.Contains(joined, `data: {"n":1}`) {
		t.Errorf("frame lines = %v", lines)
	}
}

func TestServeLastEventIDReplay(t *testing.T) {
	h := NewHub()
	for i := 1; i <= 3; i++ {
		h.Publish(Event{Data: fmt.Appendf(nil, "e%d", i)})
	}
	srv := startServer(t, h)
	resp, sc := openStream(t, srv.URL, http.Header{"Last-Event-ID": {"1"}})
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
}

func TestServeTopicFilter(t *testing.T) {
	h := NewHub()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.Serve(w, r, WithTopic(r.URL.Query().Get("topic")))
	}))
	t.Cleanup(srv.Close)

	resp, sc := openStream(t, srv.URL+"?topic=a", nil)
	defer resp.Body.Close()
	readUntil(t, sc, func(l string) bool { return l == ": connected" })
	waitForClients(t, h, 1)

	h.Publish(Event{Topic: "b", Data: []byte("skip")})
	h.Publish(Event{Topic: "a", Data: []byte("take")})

	lines := readUntil(t, sc, func(l string) bool { return strings.HasPrefix(l, "data: ") })
	if got := lines[len(lines)-1]; got != "data: take" {
		t.Errorf("first delivered = %q, want data: take (topic b must be filtered)", got)
	}
}

func TestServeOnConnectHook(t *testing.T) {
	h := NewHub()
	h.Publish(Event{Data: []byte("pre")})
	srv := startServer(t, h, OnConnect(func(w *Writer, floor, head uint64) error {
		return w.Event(head, "connected", fmt.Appendf(nil, `{"floor":%d,"head":%d}`, floor, head))
	}))
	resp, sc := openStream(t, srv.URL, nil)
	defer resp.Body.Close()

	lines := readUntil(t, sc, func(l string) bool { return strings.HasPrefix(l, "data: {\"floor\"") })
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "event: connected") || !strings.Contains(joined, `{"floor":1,"head":1}`) {
		t.Errorf("handshake lines = %v", lines)
	}
}

func TestServeMaxClients503(t *testing.T) {
	h := NewHub(WithMaxClients(1))
	srv := startServer(t, h)
	resp1, sc := openStream(t, srv.URL, nil)
	defer resp1.Body.Close()
	readUntil(t, sc, func(l string) bool { return l == ": connected" })
	waitForClients(t, h, 1)

	resp2, _ := openStream(t, srv.URL, nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("second client status = %d, want 503", resp2.StatusCode)
	}
}

func TestServeDraining503(t *testing.T) {
	h := NewHub()
	h.Shutdown()
	srv := startServer(t, h)
	resp, _ := openStream(t, srv.URL, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 while draining", resp.StatusCode)
	}
}

func TestServeShutdownDisconnectsClient(t *testing.T) {
	h := NewHub()
	srv := startServer(t, h)
	resp, sc := openStream(t, srv.URL, nil)
	defer resp.Body.Close()
	readUntil(t, sc, func(l string) bool { return l == ": connected" })
	waitForClients(t, h, 1)

	h.Shutdown()
	deadline := time.Now().Add(5 * time.Second)
	for sc.Scan() { // stream should end shortly
		if time.Now().After(deadline) {
			t.Fatal("stream still open after Shutdown")
		}
	}
}

func TestServeKeepalive(t *testing.T) {
	h := NewHub(WithKeepalive(20 * time.Millisecond))
	srv := startServer(t, h)
	resp, sc := openStream(t, srv.URL, nil)
	defer resp.Body.Close()
	readUntil(t, sc, func(l string) bool { return l == ": connected" })
	readUntil(t, sc, func(l string) bool { return l == ": keepalive" })
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
