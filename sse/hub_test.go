package sse

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestRing(t *testing.T) {
	t.Run("since returns events after id, oldest first", func(t *testing.T) {
		r := newRing(4)
		for i := uint64(1); i <= 3; i++ {
			r.append(envelope{id: i})
		}
		got := r.since(1, "")
		if len(got) != 2 || got[0].id != 2 || got[1].id != 3 {
			t.Errorf("since(1) = %+v, want ids [2 3]", got)
		}
	})
	t.Run("eviction keeps newest", func(t *testing.T) {
		r := newRing(2)
		for i := uint64(1); i <= 5; i++ {
			r.append(envelope{id: i})
		}
		if f := r.floor(); f != 4 {
			t.Errorf("floor = %d, want 4", f)
		}
		got := r.since(0, "")
		if len(got) != 2 || got[0].id != 4 || got[1].id != 5 {
			t.Errorf("since(0) = %+v, want ids [4 5]", got)
		}
	})
	t.Run("topic filter in replay", func(t *testing.T) {
		r := newRing(8)
		r.append(envelope{id: 1, event: Event{Topic: "a"}})
		r.append(envelope{id: 2, event: Event{Topic: "b"}})
		r.append(envelope{id: 3}) // broadcast
		got := r.since(0, "a")
		if len(got) != 2 || got[0].id != 1 || got[1].id != 3 {
			t.Errorf("since(0,a) = %+v, want ids [1 3]", got)
		}
	})
	t.Run("zero-capacity ring is inert", func(t *testing.T) {
		r := newRing(0)
		r.append(envelope{id: 1})
		if got := r.since(0, ""); got != nil {
			t.Errorf("since on empty ring = %+v", got)
		}
		if r.floor() != 0 {
			t.Error("floor on empty ring != 0")
		}
	})
}

func TestTopicMatches(t *testing.T) {
	tests := []struct {
		sub, evt string
		want     bool
	}{
		{"", "", true},    // unfiltered, broadcast
		{"", "a", true},   // unfiltered receives scoped
		{"a", "", true},   // broadcast reaches filtered
		{"a", "a", true},  // exact match
		{"a", "b", false}, // mismatch
	}
	for _, tt := range tests {
		if got := topicMatches(tt.sub, tt.evt); got != tt.want {
			t.Errorf("topicMatches(%q,%q) = %v, want %v", tt.sub, tt.evt, got, tt.want)
		}
	}
}

func TestHubPublishFanout(t *testing.T) {
	h := NewHub()
	subA, _, ok := h.subscribe("a", 0, func() {})
	if !ok {
		t.Fatal("subscribe a")
	}
	subAll, _, ok := h.subscribe("", 0, func() {})
	if !ok {
		t.Fatal("subscribe all")
	}
	h.Publish(Event{Topic: "a", Data: []byte("x")})
	h.Publish(Event{Topic: "b", Data: []byte("y")})
	h.Publish(Event{Data: []byte("z")}) // broadcast

	if got := len(subA.ch); got != 2 { // a + broadcast
		t.Errorf("topic-a subscriber got %d events, want 2", got)
	}
	if got := len(subAll.ch); got != 3 {
		t.Errorf("unfiltered subscriber got %d events, want 3", got)
	}
	e := <-subA.ch
	if e.id != 1 || string(e.event.Data) != "x" {
		t.Errorf("first event = id %d data %q", e.id, e.event.Data)
	}
}

func TestHubSlowClientEviction(t *testing.T) {
	h := NewHub(WithClientBuffer(1))
	cancelled := false
	sub, _, ok := h.subscribe("", 0, func() { cancelled = true })
	if !ok {
		t.Fatal("subscribe")
	}
	h.Publish(Event{Data: []byte("1")}) // fills the buffer
	h.Publish(Event{Data: []byte("2")}) // overflows -> evict
	if !cancelled {
		t.Error("slow subscriber was not cancelled")
	}
	if n := h.ClientCount(); n != 0 {
		t.Errorf("ClientCount = %d after eviction, want 0", n)
	}
	if len(sub.ch) != 1 {
		t.Errorf("buffer holds %d, want the 1 pre-eviction event", len(sub.ch))
	}
}

func TestHubBounds(t *testing.T) {
	h := NewHub(WithReplay(2))
	if f, hd := h.Bounds(); f != 0 || hd != 0 {
		t.Errorf("empty Bounds = (%d,%d)", f, hd)
	}
	for range 5 {
		h.Publish(Event{Data: []byte("e")})
	}
	f, hd := h.Bounds()
	if f != 4 || hd != 5 {
		t.Errorf("Bounds = (%d,%d), want (4,5)", f, hd)
	}
}

func TestHubSubscribeAtomicReplay(t *testing.T) {
	// The replay snapshot and registration are atomic: an event published
	// after subscribe() must appear on the channel and NOT in the replay.
	h := NewHub()
	h.Publish(Event{Data: []byte("old-1")})
	h.Publish(Event{Data: []byte("old-2")})
	sub, replay, ok := h.subscribe("", 1, func() {})
	if !ok {
		t.Fatal("subscribe")
	}
	h.Publish(Event{Data: []byte("live-3")})
	if len(replay) != 1 || replay[0].id != 2 {
		t.Errorf("replay = %+v, want just id 2", replay)
	}
	if len(sub.ch) != 1 {
		t.Fatalf("channel holds %d events, want 1", len(sub.ch))
	}
	if e := <-sub.ch; e.id != 3 {
		t.Errorf("live event id = %d, want 3", e.id)
	}
}

func TestHubMaxClients(t *testing.T) {
	h := NewHub(WithMaxClients(1))
	if _, _, ok := h.subscribe("", 0, func() {}); !ok {
		t.Fatal("first subscribe refused")
	}
	if _, _, ok := h.subscribe("", 0, func() {}); ok {
		t.Error("second subscribe admitted past cap")
	}
}

func TestHubShutdown(t *testing.T) {
	h := NewHub()
	cancelled := false
	_, _, ok := h.subscribe("", 0, func() { cancelled = true })
	if !ok {
		t.Fatal("subscribe")
	}
	h.Shutdown()
	if !cancelled {
		t.Error("Shutdown did not cancel the subscriber")
	}
	if _, _, ok := h.subscribe("", 0, func() {}); ok {
		t.Error("subscribe admitted while draining")
	}
	h.Shutdown() // idempotent
}

func TestNilHubPublish(t *testing.T) {
	var h *Hub
	h.Publish(Event{Data: []byte("x")}) // must not panic
}

func TestHubConcurrency(t *testing.T) {
	h := NewHub(WithClientBuffer(4096))
	ctx := t.Context()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 200 {
				h.Publish(Event{Topic: "t", Data: []byte("x")})
			}
		})
	}
	for range 8 {
		wg.Go(func() {
			sub, _, ok := h.subscribe("t", 0, func() {})
			if !ok {
				return
			}
			defer h.unsubscribe(sub)
			for {
				select {
				case <-sub.ch:
				case <-ctx.Done():
					return
				default:
					return
				}
			}
		})
	}
	wg.Wait()
	f, head := h.Bounds()
	if head != 1600 {
		t.Errorf("head = %d, want 1600", head)
	}
	if f == 0 {
		t.Error("floor = 0 after publishes")
	}
}

func TestWriteFrame(t *testing.T) {
	tests := []struct {
		name  string
		id    uint64
		event string
		data  string
		want  string
	}{
		{name: "id and data", id: 7, data: `{"a":1}`, want: "id: 7\ndata: {\"a\":1}\n\n"},
		{name: "named event", id: 1, event: "notify", data: "x", want: "id: 1\nevent: notify\ndata: x\n\n"},
		{name: "no id", data: "x", want: "data: x\n\n"},
		{name: "multiline data split per spec", id: 2, data: "a\nb", want: "id: 2\ndata: a\ndata: b\n\n"},
		{name: "empty data still emits one line", id: 3, data: "", want: "id: 3\ndata: \n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeFrame(&buf, tt.id, tt.event, []byte(tt.data)); err != nil {
				t.Fatal(err)
			}
			if got := buf.String(); got != tt.want {
				t.Errorf("frame = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWriterComment(t *testing.T) {
	var buf bytes.Buffer
	sw := &Writer{w: &buf}
	if err := sw.Comment("connected"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), ": connected\n\n") {
		t.Errorf("comment = %q", buf.String())
	}
}

func TestHubReplayPublicWindow(t *testing.T) {
	h := NewHub(WithReplay(4))
	for i := 1; i <= 6; i++ {
		topic := ""
		if i%2 == 0 {
			topic = "even"
		}
		h.Publish(Event{Topic: topic, Data: []byte{byte('0' + i)}})
	}
	all := h.Replay(0, "")
	if len(all) != 4 { // ring keeps the newest 4 (ids 3..6)
		t.Fatalf("Replay(0) len = %d, want 4", len(all))
	}
	// IDs strictly monotonic, oldest first — the resume-ordering property.
	for i := 1; i < len(all); i++ {
		if all[i].ID != all[i-1].ID+1 {
			t.Errorf("non-monotonic ids: %d then %d", all[i-1].ID, all[i].ID)
		}
	}
	if all[0].ID != 3 || all[3].ID != 6 {
		t.Errorf("window = [%d..%d], want [3..6]", all[0].ID, all[3].ID)
	}
	// Topic filter: broadcasts (odd ids here) + matching topic.
	evens := h.Replay(3, "even")
	for _, e := range evens {
		if e.Event.Topic != "even" && e.Event.Topic != "" {
			t.Errorf("foreign topic in filtered replay: %+v", e)
		}
	}
}
