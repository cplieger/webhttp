package sse

// ring is a fixed-size replay buffer of the most recent events. O(1) append
// via head/length index arithmetic; not concurrency-safe on its own (the Hub
// guards it with its mutex).
type ring struct {
	buf  []envelope
	head int // next write position
	n    int // occupied count
}

func newRing(capacity int) *ring {
	return &ring{buf: make([]envelope, capacity)}
}

// append stores e, evicting the oldest entry once full.
func (r *ring) append(e envelope) {
	if len(r.buf) == 0 {
		return
	}
	r.buf[r.head] = e
	r.head = (r.head + 1) % len(r.buf)
	if r.n < len(r.buf) {
		r.n++
	}
}

// since returns the buffered events with ID greater than sinceID that a
// subscriber with the given topic filter would receive, oldest first.
func (r *ring) since(sinceID uint64, topic string) []envelope {
	var out []envelope
	start := (r.head - r.n + len(r.buf)) % max(len(r.buf), 1)
	for i := range r.n {
		e := r.buf[(start+i)%len(r.buf)]
		if e.id <= sinceID {
			continue
		}
		if !topicMatches(topic, e.event.Topic) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// floor returns the oldest event ID still buffered, or 0 when empty.
func (r *ring) floor() uint64 {
	if r.n == 0 {
		return 0
	}
	oldest := (r.head - r.n + len(r.buf)) % len(r.buf)
	return r.buf[oldest].id
}
