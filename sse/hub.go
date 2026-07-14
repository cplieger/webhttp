package sse

import (
	"context"
	"log/slog"
	"sync"
)

// Event is one server-sent event to broadcast.
type Event struct {
	// Name is the SSE `event:` field. Empty emits an unnamed event, which an
	// EventSource client receives via onmessage; a non-empty name requires
	// addEventListener(name, ...) on the client.
	Name string
	// Topic scopes delivery: a topic-filtered subscriber receives an event
	// only when the topics match. An event with an empty Topic is a broadcast
	// delivered to every subscriber regardless of filter.
	Topic string
	// Data is the pre-marshaled payload for the `data:` field. The hub does
	// no serialization of its own. A payload containing newlines is emitted
	// as one `data:` line per line, per the SSE specification.
	Data []byte
}

// envelope is an Event stamped with its replay ID.
type envelope struct {
	event Event
	id    uint64
}

// subscriber is one connected client's delivery state.
type subscriber struct {
	ch     chan envelope
	cancel context.CancelFunc
	topic  string
}

// topicMatches reports whether an event with eventTopic reaches a subscriber
// filtered to subTopic: broadcasts (empty event topic) always do, an
// unfiltered subscriber ("" filter) receives everything, and a scoped event
// otherwise requires an exact match.
func topicMatches(subTopic, eventTopic string) bool {
	return eventTopic == "" || subTopic == "" || subTopic == eventTopic
}

// Hub is a broadcast fan-out for Server-Sent Events with a replay ring.
// The zero value is not usable; construct with NewHub. A single Hub is safe
// for concurrent use by any number of publishers and subscribers.
type Hub struct {
	logger      *slog.Logger
	subscribers map[*subscriber]struct{}
	ring        *ring
	cfg         config
	seq         uint64 // last assigned event ID; guarded by mu
	mu          sync.Mutex
	draining    bool
}

// NewHub returns a Hub configured by the supplied options.
func NewHub(opts ...Option) *Hub {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	return &Hub{
		subscribers: make(map[*subscriber]struct{}),
		ring:        newRing(cfg.ringSize),
		cfg:         cfg,
		logger:      cfg.logger,
	}
}

// Publish assigns the event its ID, appends it to the replay ring, and fans
// it out to every matching subscriber. A subscriber whose buffer is full is
// evicted (its connection cancelled) rather than blocking the hub; the
// client's EventSource reconnects and resumes from Last-Event-ID. Publish on
// a nil Hub is a no-op, so an optional hub can be threaded without guards.
func (h *Hub) Publish(evt Event) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.seq++
	env := envelope{event: evt, id: h.seq}
	h.ring.append(env)
	for sub := range h.subscribers {
		if !topicMatches(sub.topic, evt.Topic) {
			continue
		}
		select {
		case sub.ch <- env:
		default:
			h.logger.Warn("sse: evicting slow client", "topic", sub.topic)
			sub.cancel()
			delete(h.subscribers, sub)
		}
	}
	h.mu.Unlock()
}

// Bounds returns the replay window: floor is the oldest event ID still in
// the ring, head the newest ID assigned. Both are 0 before the first
// publish. A reconnecting client whose last-seen ID is below floor has
// missed events and should refetch authoritative state.
func (h *Hub) Bounds() (floor, head uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ring.floor(), h.seq
}

// ReplayEvent is one buffered event with its assigned ID, as returned by
// Replay.
type ReplayEvent struct {
	Event Event
	ID    uint64
}

// Replay returns the buffered events with ID greater than sinceID that a
// subscriber filtered to topic would receive, oldest first. Replay(0, "")
// is the full buffered window. Intended for diagnostics and for
// applications that deliver initial state over a side channel; the
// Last-Event-ID replay in Serve does not go through this method.
func (h *Hub) Replay(sinceID uint64, topic string) []ReplayEvent {
	h.mu.Lock()
	envs := h.ring.since(sinceID, topic)
	h.mu.Unlock()
	out := make([]ReplayEvent, len(envs))
	for i, e := range envs {
		out[i] = ReplayEvent{Event: e.event, ID: e.id}
	}
	return out
}

// ClientCount returns the number of connected subscribers.
func (h *Hub) ClientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subscribers)
}

// Shutdown flips the hub into draining mode and cancels every connected
// subscriber. Subsequent Serve calls are answered 503 immediately, so a
// last-instant reconnect cannot register after the cancel sweep. Safe to
// call more than once.
func (h *Hub) Shutdown() {
	h.mu.Lock()
	h.draining = true
	for sub := range h.subscribers {
		sub.cancel()
	}
	clear(h.subscribers)
	h.mu.Unlock()
}

// subscribe registers a new subscriber and, in the same critical section,
// snapshots the replay ring for events after lastID. The atomicity makes
// replay + live delivery gap-free and overlap-free: everything at or below
// the snapshot head arrives from the returned replay slice, everything
// after it through the channel.
//
// It returns ok=false when the hub is draining or the client cap is reached.
func (h *Hub) subscribe(topic string, lastID uint64, cancel context.CancelFunc) (sub *subscriber, replay []envelope, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.draining {
		return nil, nil, false
	}
	if h.cfg.maxClients > 0 && len(h.subscribers) >= h.cfg.maxClients {
		return nil, nil, false
	}
	sub = &subscriber{
		ch:     make(chan envelope, h.cfg.clientBuffer),
		cancel: cancel,
		topic:  topic,
	}
	h.subscribers[sub] = struct{}{}
	if lastID > 0 {
		replay = h.ring.since(lastID, topic)
	}
	h.logger.Debug("sse: client connected", "clients", len(h.subscribers), "topic", topic, "last_event_id", lastID)
	return sub, replay, true
}

// unsubscribe removes a subscriber (idempotent; eviction may already have).
func (h *Hub) unsubscribe(sub *subscriber) {
	h.mu.Lock()
	delete(h.subscribers, sub)
	n := len(h.subscribers)
	h.mu.Unlock()
	h.logger.Debug("sse: client disconnected", "clients", n, "topic", sub.topic)
}
