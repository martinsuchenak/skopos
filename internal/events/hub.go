package events

import "sync"

// Event is a change notification pushed to connected SSE clients.
type Event struct {
	Type string `json:"type"`
}

// Hub fans out Events to all subscribed SSE clients. It is in-process and
// therefore valid for a single skopos instance.
type Hub struct {
	mu          sync.Mutex
	subscribers map[chan Event]struct{}
}

func NewHub() *Hub {
	return &Hub{subscribers: make(map[chan Event]struct{})}
}

// Subscribe returns a buffered channel of events and an unsubscribe function.
// The unsubscribe is idempotent and safe to call multiple times.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 16)
	h.mu.Lock()
	h.subscribers[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subscribers[ch]; ok {
			delete(h.subscribers, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// Publish broadcasts an event to all subscribers, non-blockingly: a subscriber
// whose buffer is full is skipped (it will re-sync on its next poll/refresh).
func (h *Hub) Publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subscribers {
		select {
		case ch <- e:
		default:
		}
	}
}
