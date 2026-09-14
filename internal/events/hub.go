package events

import "sync"

// Event is a change notification pushed to connected SSE clients. Workspace
// carries the mutation's scope when the middleware could derive it; scoped
// subscribers do not receive events for workspaces outside their key.
type Event struct {
	Type      string `json:"type"`
	Workspace string `json:"workspace,omitempty"`
}

// Hub fans out Events to all subscribed SSE clients. It is in-process and
// therefore valid for a single skopos instance.
type Hub struct {
	mu          sync.Mutex
	subscribers map[chan Event]struct{}
	filters     map[chan Event]func(string) bool
	closed      bool
}

func NewHub() *Hub {
	return &Hub{subscribers: make(map[chan Event]struct{}), filters: make(map[chan Event]func(string) bool)}
}

// SubscribeFiltered behaves like Subscribe but drops events whose workspace
// is outside the filter (empty-workspace events always pass: they are either
// unattributed or global signals, and carry no data).
func (h *Hub) SubscribeFiltered(canAccess func(workspace string) bool) (<-chan Event, func()) {
	ch := make(chan Event, 16)
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	h.filters[ch] = canAccess
	h.subscribers[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subscribers[ch]; ok {
			delete(h.subscribers, ch)
			delete(h.filters, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
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
		if ws := e.Workspace; ws != "" {
			if f, ok := h.filters[ch]; ok && f != nil && !f(ws) {
				continue
			}
		}
		select {
		case ch <- e:
		default:
		}
	}
}
