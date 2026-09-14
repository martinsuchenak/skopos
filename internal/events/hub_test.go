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

// TestHubFailsClosedForScopedSubscribers pins the tenant-isolation rule:
// scoped subscribers receive only attributed, in-scope events; unattributed
// events reach unfiltered subscribers only (third pentest round, vuln-0004:
// the previous fail-open delivery disclosed every tenant's activity).
func TestHubFailsClosedForScopedSubscribers(t *testing.T) {
	hub := NewHub()
	defer hub.Close()
	canAccess := func(ws string) bool { return ws == "ws-a" }

	scoped, unsubScoped := hub.SubscribeFiltered(canAccess)
	defer unsubScoped()
	root, unsubRoot := hub.Subscribe()
	defer unsubRoot()

	hub.Publish(Event{Type: "blackboard", Workspace: "ws-b"}) // foreign: nobody scoped
	hub.Publish(Event{Type: "blackboard", Workspace: "ws-a"}) // own
	hub.Publish(Event{Type: "change"})                         // unattributed: root only

	drain := func(ch <-chan Event) []Event {
		var out []Event
		for {
			select {
			case ev := <-ch:
				out = append(out, ev)
			default:
				return out
			}
		}
	}
	scopedGot := drain(scoped)
	if len(scopedGot) != 1 || scopedGot[0].Workspace != "ws-a" {
		t.Fatalf("scoped subscriber must get only the in-scope event: %+v", scopedGot)
	}
	rootGot := drain(root)
	if len(rootGot) != 3 {
		t.Fatalf("root must receive everything incl. unattributed: %+v", rootGot)
	}
}
