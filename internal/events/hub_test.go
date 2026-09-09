package events

import "testing"

func TestHubPublishSubscribe(t *testing.T) {
	h := NewHub()
	ch, unsub := h.Subscribe()
	defer unsub()

	h.Publish(Event{Type: "sessions"})
	select {
	case ev := <-ch:
		if ev.Type != "sessions" {
			t.Errorf("got %q, want sessions", ev.Type)
		}
	default:
		t.Fatal("expected to receive the published event")
	}
}

func TestHubUnsubscribeClosesChannel(t *testing.T) {
	h := NewHub()
	ch, unsub := h.Subscribe()
	unsub()

	h.Publish(Event{Type: "x"}) // must not panic on closed-channel subscriber
	ev, ok := <-ch
	if ok {
		t.Errorf("expected channel to be closed, got %v", ev)
	}
}

// TestHubPublishDoesNotBlockWhenFull ensures a slow subscriber can't wedge Publish.
func TestHubPublishDoesNotBlockWhenFull(t *testing.T) {
	h := NewHub()
	_, unsub := h.Subscribe()
	defer unsub()

	for i := 0; i < 1000; i++ {
		h.Publish(Event{Type: "x"})
	}
}

func TestHubCloseClosesSubscribers(t *testing.T) {
	h := NewHub()
	ch, unsub := h.Subscribe()
	defer unsub()

	h.Close()
	if ev, ok := <-ch; ok {
		t.Fatalf("expected channel to be closed, got %v", ev)
	}
}

func TestHubPublishAfterCloseIsNoop(t *testing.T) {
	h := NewHub()
	ch, unsub := h.Subscribe()
	defer unsub()
	h.Close()
	// Drain the closed-channel marker, if any.
	for range ch {
	}

	h.Publish(Event{Type: "x"}) // must not panic
	h.Close()                   // second close is a no-op
}

func TestHubSubscribeAfterClose(t *testing.T) {
	h := NewHub()
	h.Close()

	ch, unsub := h.Subscribe()
	defer unsub()
	if ev, ok := <-ch; ok {
		t.Fatalf("expected already-closed channel, got %v", ev)
	}
}
