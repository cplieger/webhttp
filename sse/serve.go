package sse

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/webhttp"
)

// config carries hub-level settings; assembled by NewHub from Options.
type config struct {
	logger       *slog.Logger
	ringSize     int
	clientBuffer int
	maxClients   int
	keepalive    time.Duration
}

func defaultConfig() config {
	return config{
		logger:       slog.Default(),
		ringSize:     256,
		clientBuffer: 256,
		maxClients:   0, // unlimited
		keepalive:    15 * time.Second,
	}
}

// Option configures a Hub at construction.
type Option func(*config)

// WithReplay sets the replay ring capacity (default 256). The client-side
// delivery buffer is sized to match unless WithClientBuffer overrides it.
// A capacity of 0 disables replay: events still carry IDs, but a reconnect
// starts from live traffic only.
func WithReplay(n int) Option {
	return func(c *config) {
		c.ringSize = max(n, 0)
		c.clientBuffer = max(c.ringSize, 1)
	}
}

// WithClientBuffer sets the per-subscriber channel capacity (default: the
// replay ring size). A subscriber that falls this many events behind is
// evicted and relies on reconnect + replay.
func WithClientBuffer(n int) Option {
	return func(c *config) { c.clientBuffer = max(n, 1) }
}

// WithMaxClients caps concurrent subscribers; Serve answers 503 beyond it.
// 0 (the default) means unlimited.
func WithMaxClients(n int) Option {
	return func(c *config) { c.maxClients = max(n, 0) }
}

// WithKeepalive sets the interval between `: keepalive` comments (default
// 15s, below common proxy idle timeouts: nginx 60s, ALB 120s). A
// non-positive value disables keepalives.
func WithKeepalive(d time.Duration) Option {
	return func(c *config) { c.keepalive = d }
}

// WithLogger sets the slog.Logger for connect/disconnect/eviction
// diagnostics (default slog.Default()).
func WithLogger(l *slog.Logger) Option {
	return func(c *config) {
		if l != nil {
			c.logger = l
		}
	}
}

// Writer lets an OnConnect hook write frames onto the stream before live
// delivery starts. Writes are unflushed until the hook returns (Serve
// flushes once after the hook).
type Writer struct {
	w io.Writer
}

// Event writes one SSE frame. id 0 omits the `id:` field; an empty name
// omits the `event:` field.
func (sw *Writer) Event(id uint64, name string, data []byte) error {
	return writeFrame(sw.w, id, name, data)
}

// Comment writes an SSE comment line (`: text`), invisible to EventSource
// consumers but useful as a connection liveness marker.
func (sw *Writer) Comment(text string) error {
	_, err := fmt.Fprintf(sw.w, ": %s\n\n", text)
	return err
}

// ServeOption configures one Serve call.
type ServeOption func(*serveConfig)

type serveConfig struct {
	onConnect func(w *Writer, floor, head uint64) error
	topic     string
}

// WithTopic subscribes this client to broadcasts plus events scoped to the
// given topic. An empty topic (the default) receives everything.
func WithTopic(topic string) ServeOption {
	return func(c *serveConfig) { c.topic = topic }
}

// OnConnect installs a hook that runs after the subscriber is registered and
// after any Last-Event-ID replay, before live delivery begins. It receives
// the replay bounds so the application can write a handshake carrying them
// (letting the client detect a replay gap) and any initial per-client state.
// When no hook is installed, Serve writes a bare `: connected` comment.
func OnConnect(fn func(w *Writer, floor, head uint64) error) ServeOption {
	return func(c *serveConfig) { c.onConnect = fn }
}

// Serve subscribes the request to the hub and streams events until the
// client disconnects, the request context ends, the subscriber is evicted,
// or the hub shuts down. It owns the response headers, Last-Event-ID replay,
// keepalive comments, and frame encoding.
//
// It answers 503 while the hub is draining or over the client cap, and 500
// when the ResponseWriter cannot stream: no http.Flusher reachable either
// directly or through an Unwrap() chain (the http.ResponseController
// discovery rule), so wrapping middleware that forwards flushes via Unwrap
// keeps streaming intact.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, opts ...ServeOption) {
	var sc serveConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&sc)
		}
	}

	if !canFlush(w) {
		webhttp.WriteError(w, r, http.StatusInternalServerError, "streaming_unsupported", "streaming not supported")
		return
	}
	rc := http.NewResponseController(w)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	lastID := parseLastEventID(h.logger, r.Header.Get("Last-Event-ID"))
	sub, replay, ok := h.subscribe(sc.topic, lastID, cancel)
	if !ok {
		webhttp.WriteError(w, r, http.StatusServiceUnavailable, "sse_unavailable", "sse unavailable")
		return
	}
	defer h.unsubscribe(sub)

	writeStreamHeaders(w)
	clearDeadlines(h.logger, rc)

	// Replay precedes the handshake so the OnConnect hook's bounds are
	// consistent with what the client has already been sent.
	for _, env := range replay {
		if err := writeFrame(w, env.id, env.event.Name, env.event.Data); err != nil {
			return
		}
	}

	sw := &Writer{w: w}
	if sc.onConnect != nil {
		floor, head := h.Bounds()
		if err := sc.onConnect(sw, floor, head); err != nil {
			return
		}
	} else if err := sw.Comment("connected"); err != nil {
		return
	}
	if rc.Flush() != nil {
		return
	}

	h.stream(ctx, w, rc, sub)
}

// canFlush reports whether w can flush, either by implementing http.Flusher
// itself or by exposing one through an Unwrap() http.ResponseWriter chain —
// the same discovery http.ResponseController performs. The probe never
// writes, so a non-streamable writer can still receive the JSON 500 refusal.
func canFlush(w http.ResponseWriter) bool {
	for {
		switch t := w.(type) {
		case http.Flusher:
			return true
		case interface{ Unwrap() http.ResponseWriter }:
			w = t.Unwrap()
		default:
			return false
		}
	}
}

// stream is the live delivery loop: channel events, keepalives, and
// context/eviction termination. A write or flush error inside either helper
// ends the stream (the client is gone or the connection is unusable).
func (h *Hub) stream(ctx context.Context, w io.Writer, rc *http.ResponseController, sub *subscriber) {
	var keepaliveC <-chan time.Time
	if h.cfg.keepalive > 0 {
		t := time.NewTicker(h.cfg.keepalive)
		defer t.Stop()
		keepaliveC = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case env := <-sub.ch:
			if !writeBatch(w, rc, sub, env) {
				return
			}
		case <-keepaliveC:
			if !writeKeepalive(w, rc) {
				return
			}
		}
	}
}

// writeBatch writes env plus everything already queued behind it, then
// flushes once, so a burst reaches the client in a single flush. Returns
// false on a write or flush error (client gone).
func writeBatch(w io.Writer, rc *http.ResponseController, sub *subscriber, env envelope) bool {
	if err := writeFrame(w, env.id, env.event.Name, env.event.Data); err != nil {
		return false
	}
	for {
		select {
		case env = <-sub.ch:
			if err := writeFrame(w, env.id, env.event.Name, env.event.Data); err != nil {
				return false
			}
		default:
			return rc.Flush() == nil
		}
	}
}

// writeKeepalive emits one keepalive comment and flushes it. Returns false
// on a write or flush error.
func writeKeepalive(w io.Writer, rc *http.ResponseController) bool {
	if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
		return false
	}
	return rc.Flush() == nil
}

// writeFrame emits one SSE frame: optional id and event fields, then the
// data payload split into one `data:` line per newline, per the SSE spec.
func writeFrame(w io.Writer, id uint64, name string, data []byte) error {
	if id > 0 {
		if _, err := fmt.Fprintf(w, "id: %d\n", id); err != nil {
			return err
		}
	}
	if name != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", name); err != nil {
			return err
		}
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// writeStreamHeaders sets the proxy-defensive SSE response headers.
// no-transform (RFC 7234 §5.2.2.4) stops intermediaries from wrapping the
// stream in gzip, which would buffer per-event flushes; X-Accel-Buffering
// disables nginx-style response buffering.
func writeStreamHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")
}

// clearDeadlines removes the server's read/write deadlines for this
// long-lived connection; a per-connection deadline would sever the stream
// mid-flight. Failure is logged and otherwise ignored (an http2 conn or a
// test recorder may not support deadlines).
func clearDeadlines(logger *slog.Logger, rc *http.ResponseController) {
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		logger.Debug("sse: clear write deadline", "error", err)
	}
	if err := rc.SetReadDeadline(time.Time{}); err != nil {
		logger.Debug("sse: clear read deadline", "error", err)
	}
}

// parseLastEventID parses the Last-Event-ID header. Empty or malformed (a
// proxy-mangled header) yields 0, logged at Debug for gap-tracing.
func parseLastEventID(logger *slog.Logger, raw string) uint64 {
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		logger.Debug("sse: Last-Event-ID parse failed", "raw", raw, "error", err)
		return 0
	}
	return n
}
