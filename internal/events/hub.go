package events

import "sync"

// Event type names. The dashboard reacts to these exact strings.
const (
	TypeSessions   = "sessions"
	TypeBlackboard = "blackboard"
	TypePlans      = "plans"
	TypeWorkspaces = "workspaces"
	TypeInbox      = "inbox"
	TypeChange     = "change"
)

// Event is a change notification pushed to connected SSE clients. Workspace
// is the mutation's authoritative scope, supplied by the service that
// performed the mutation; scoped subscribers receive only events whose
// workspace they can access, and unattributed events reach unfiltered
// (root/internal) subscribers only.
type Event struct {
	Type      string `json:"type"`
	Workspace string `json:"workspace,omitempty"`
}

// Publisher is the subset of *Hub that domains publish through; a nil
// Publisher (or nil *Hub) is a valid no-op, so services and their tests run
// without an event bus.
type Publisher interface {
	Publish(e Event)
}

// Hub fans out Events to all subscribed SSE clients. It is in-process and
// therefore valid for a single skopos instance.
type Hub struct {
	mu          sync.Mutex
	subscribers map[chan Event]struct{}
	filters     map[chan Event]func(string) bool
	byKey       map[string]map[chan Event]struct{}
	closed      bool
}

func NewHub() *Hub {
	return &Hub{
		subscribers: make(map[chan Event]struct{}),
		filters:     make(map[chan Event]func(string) bool),
		byKey:       make(map[string]map[chan Event]struct{}),
	}
}

// SubscribeKeyed subscribes on behalf of a specific API key: the stream is
// scope-filtered and is terminated by DropKey when that key is revoked.
func (h *Hub) SubscribeKeyed(keyID string, canAccess func(workspace string) bool) (<-chan Event, func()) {
	ch := make(chan Event, 16)
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	h.subscribers[ch] = struct{}{}
	h.filters[ch] = canAccess
	if h.byKey[keyID] == nil {
		h.byKey[keyID] = make(map[chan Event]struct{})
	}
	h.byKey[keyID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, h.unsubscribe(ch)
}

// unsubscribe returns the removal closure shared by Subscribe variants.
func (h *Hub) unsubscribe(ch chan Event) func() {
	return func() {
		h.mu.Lock()
		h.remove(ch)
		h.mu.Unlock()
	}
}

// remove detaches a subscriber; caller holds h.mu.
func (h *Hub) remove(ch chan Event) {
	if _, ok := h.subscribers[ch]; ok {
		delete(h.subscribers, ch)
		delete(h.filters, ch)
		for keyID, set := range h.byKey {
			if delete(set, ch); len(set) == 0 {
				delete(h.byKey, keyID)
			}
		}
		close(ch)
	}
}

// DropKey terminates every stream subscribed with keyID (API key revoked).
func (h *Hub) DropKey(keyID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	for ch := range h.byKey[keyID] {
		h.remove(ch)
	}
}

// SubscribeFiltered behaves like Subscribe but enforces tenant isolation:
// the subscriber receives only events attributed to a workspace the filter
// accepts. Unattributed (empty-workspace) events are withheld — a scoped
// subscriber cannot verify their ownership, so delivery fails closed.
// Unfiltered (root/internal) subscribers receive everything.
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
	return ch, h.unsubscribe(ch)
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
	h.filters = make(map[chan Event]func(string) bool)
	h.byKey = make(map[string]map[chan Event]struct{})
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
		if f, ok := h.filters[ch]; ok && f != nil {
			// Scoped subscriber: fail closed — only attributed, in-scope
			// events are delivered.
			if e.Workspace == "" || !f(e.Workspace) {
				continue
			}
		}
		select {
		case ch <- e:
		default:
		}
	}
}
