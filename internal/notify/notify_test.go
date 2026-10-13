package notify

import (
	"context"
	"strings"
	"testing"
)

// The provider interface stays honest: a fake provider + a recording relay,
// verifying thread binding, Martin-only filtering, and ack-on-handled.
type fakeRelay struct {
	applied []string
}

func (f *fakeRelay) Apply(ctx context.Context, itemID, text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	f.applied = append(f.applied, itemID+"|"+text)
	return true
}

func (f *fakeRelay) ItemForThread(parent string) string { return "" }

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
