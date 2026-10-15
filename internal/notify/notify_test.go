package notify

import (
	"context"
	"strings"
	"testing"
)

// The provider interface stays honest: a fake provider + a recording relay,
// verifying thread binding, Martin-only filtering, and ack-on-handled.
type fakeRelay struct {
	applied  []string
	topLevel map[string]string
	resolved string
}

func (f *fakeRelay) Apply(ctx context.Context, itemID, text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	f.applied = append(f.applied, itemID+"|"+text)
	return true
}

func (f *fakeRelay) TopLevel(ctx context.Context, text, ts string) bool {
	if f.topLevel == nil {
		return false
	}
	if strings.HasPrefix(text, "new:") || text == "help" {
		f.topLevel[text] = ts
		return true
	}
	return false
}

func (f *fakeRelay) ItemForThread(parent string) string { return f.resolved }

func TestCap(t *testing.T) {
	if got := cap2800(strings.Repeat("a", 4000)); len(got) > 2900 {
		t.Fatalf("cap failed: %d", len(got))
	}
	if got := cap2800("short"); got != "short" {
		t.Fatalf("short mangled: %q", got)
	}
}

func TestSlackBindAndFilter(t *testing.T) {
	s := NewSlack("bot", "app", "U123")
	s.BindThread("111.222", "item-1")
	s.mu.Lock()
	item := s.threads["111.222"]
	s.mu.Unlock()
	if item != "item-1" {
		t.Fatalf("bind: %q", item)
	}
	// handleEvent only relays Martin's messages in bound threads.
	relay := &fakeRelay{}
	s.handleEvent(context.Background(), nil, []byte(`{"event":{"type":"message","user":"U999","text":"approve","thread_ts":"111.222","ts":"1","channel":"C"}}`), relay)
	if len(relay.applied) != 0 {
		t.Fatal("foreign user's message must not relay")
	}
	s.handleEvent(context.Background(), nil, []byte(`{"event":{"type":"message","user":"U123","text":"approve","thread_ts":"111.222","ts":"1","channel":"C"}}`), relay)
	if len(relay.applied) != 1 || relay.applied[0] != "item-1|approve" {
		t.Fatalf("relay: %v", relay.applied)
	}
	s.handleEvent(context.Background(), nil, []byte(`{"event":{"type":"message","user":"U123","text":"approve","thread_ts":"999.999","ts":"1","channel":"C"}}`), relay)
	if len(relay.applied) != 1 {
		t.Fatal("unbound thread must not relay")
	}
}

// Top-level DMs route to TopLevel (capture/help); unknown-thread replies
// consult ItemForThread (restart recovery) before being dropped.
func TestSlackTopLevelAndRebind(t *testing.T) {
	s := NewSlack("bot", "app", "U123")
	relay := &fakeRelay{topLevel: map[string]string{}}
	s.handleEvent(context.Background(), nil, []byte(`{"event":{"type":"message","user":"U123","text":"new: fix the thing","ts":"777","channel":"C"}}`), relay)
	if relay.topLevel["new: fix the thing"] != "777" {
		t.Fatalf("top-level DM must reach the relay with its ts: %v", relay.topLevel)
	}
	// A reply in an unknown thread is re-resolved via ItemForThread.
	relay.resolved = "item-9"
	s.handleEvent(context.Background(), nil, []byte(`{"event":{"type":"message","user":"U123","text":"approve","thread_ts":"555.1","ts":"2","channel":"C"}}`), relay)
	if len(relay.applied) != 1 || relay.applied[0] != "item-9|approve" {
		t.Fatalf("rebound reply: %v", relay.applied)
	}
	s.mu.Lock()
	bound := s.threads["555.1"]
	s.mu.Unlock()
	if bound != "item-9" {
		t.Fatalf("resolved thread must re-bind in memory: %q", bound)
	}
}
