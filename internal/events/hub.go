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
	closed      bool
}

func NewHub() *Hub {
	return &Hub{subscribers: make(map[chan Event]struct{})}
}

// Subscribe returns a buffered channel of events and an unsubscribe function.
// The unsubscribe is idempotent and safe to call multiple times. Subscribing
// to a closed hub yields an already-closed channel, so late subscribers exit
// immediately (used during server shutdown).
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 16)
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
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

// Close unsubscribes everyone and puts the hub in a terminal state: subscriber
// channels are closed so SSE handlers return, and later publishes are no-ops.
// The HTTP server invokes this on shutdown — otherwise Shutdown() blocks until
// its timeout because open SSE streams never become idle connections.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for ch := range h.subscribers {
		delete(h.subscribers, ch)
		close(ch)
	}
}

// Publish broadcasts an event to all subscribers, non-blockingly: a subscriber
// whose buffer is full is skipped (it will re-sync on its next poll/refresh).
func (h *Hub) Publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	for ch := range h.subscribers {
		select {
		case ch <- e:
		default:
		}
	}
}
