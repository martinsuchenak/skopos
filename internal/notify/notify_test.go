package notify

import (
	"context"
	"net/url"
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
	s := NewSlack("bot", "app", "U123", nil)
	s.BindThread("111.222", "item-1")
	s.mu.Lock()
	item := s.threads["111.222"]
	s.mu.Unlock()
	if item != "item-1" {
		t.Fatalf("bind: %q", item)
	}
	// handleEvent only relays Martin's messages in bound threads.
	relay := &fakeRelay{}
	s.handleEvent(context.Background(), nil, []byte(`{"event":{"type":"message","user":"U999","text":"approve","thread_ts":"111.222","ts":"1","channel":"D1","channel_type":"im"}}`), relay, &seenSet{})
	if len(relay.applied) != 0 {
		t.Fatal("foreign user's message must not relay")
	}
	s.handleEvent(context.Background(), nil, []byte(`{"event":{"type":"message","user":"U123","text":"approve","thread_ts":"111.222","ts":"1","channel":"D1","channel_type":"im"}}`), relay, &seenSet{})
	if len(relay.applied) != 1 || relay.applied[0] != "item-1|approve" {
		t.Fatalf("relay: %v", relay.applied)
	}
	s.handleEvent(context.Background(), nil, []byte(`{"event":{"type":"message","user":"U123","text":"approve","thread_ts":"999.999","ts":"1","channel":"D1","channel_type":"im"}}`), relay, &seenSet{})
	if len(relay.applied) != 1 {
		t.Fatal("unbound thread must not relay")
	}
}

// The outbound channel defaults to Martin's user id (no restart gap) and is
// learned only from HIS im events — another user's DM, or a channel the app
// can see, must neither relay nor redirect where gate messages go (review H1).
func TestSlackChannelNotHijackable(t *testing.T) {
	s := NewSlack("bot", "app", "U123", nil)
	if got := s.channel(); got != "U123" {
		t.Fatalf("before any event, posts must go to Martin's user id: %q", got)
	}
	relay := &fakeRelay{topLevel: map[string]string{}}
	for _, ev := range []string{
		`{"event":{"type":"message","user":"U999","text":"new: hi","ts":"1","channel":"D999","channel_type":"im"}}`,
		`{"event":{"type":"message","user":"U123","text":"new: hi","ts":"2","channel":"C42","channel_type":"channel"}}`,
		`{"event":{"type":"app_mention","user":"U999","text":"hi","ts":"3","channel":"C43"}}`,
	} {
		s.handleEvent(context.Background(), nil, []byte(ev), relay, &seenSet{})
	}
	if got := s.channel(); got != "U123" {
		t.Fatalf("foreign events must not redirect the outbound channel: %q", got)
	}
	if len(relay.topLevel) != 0 {
		t.Fatalf("foreign / non-im events must not reach the relay: %v", relay.topLevel)
	}
	s.handleEvent(context.Background(), nil, []byte(`{"event":{"type":"message","user":"U123","text":"new: hi","ts":"4","channel":"D1","channel_type":"im"}}`), relay, &seenSet{})
	if got := s.channel(); got != "D1" {
		t.Fatalf("Martin's IM must be learned: %q", got)
	}
}

// Top-level DMs route to TopLevel (capture/help); unknown-thread replies
// consult ItemForThread (restart recovery) before being dropped.
func TestSlackTopLevelAndRebind(t *testing.T) {
	s := NewSlack("bot", "app", "U123", nil)
	relay := &fakeRelay{topLevel: map[string]string{}}
	s.handleEvent(context.Background(), nil, []byte(`{"event":{"type":"message","user":"U123","text":"new: fix the thing","ts":"777","channel":"D1","channel_type":"im"}}`), relay, &seenSet{})
	if relay.topLevel["new: fix the thing"] != "777" {
		t.Fatalf("top-level DM must reach the relay with its ts: %v", relay.topLevel)
	}
	// A reply in an unknown thread is re-resolved via ItemForThread.
	relay.resolved = "item-9"
	s.handleEvent(context.Background(), nil, []byte(`{"event":{"type":"message","user":"U123","text":"approve","thread_ts":"555.1","ts":"2","channel":"D1","channel_type":"im"}}`), relay, &seenSet{})
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

// fakeWS feeds scripted frames and records writes (the acks).
type fakeWS struct {
	frames [][]byte
	writes []string
	done   chan struct{}
}

func (f *fakeWS) Read(ctx context.Context) ([]byte, error) {
	if len(f.frames) == 0 {
		<-f.done
		return nil, ctx.Err()
	}
	frame := f.frames[0]
	f.frames = f.frames[1:]
	return frame, nil
}

func (f *fakeWS) Write(ctx context.Context, data []byte) error {
	f.writes = append(f.writes, string(data))
	return nil
}

func (f *fakeWS) Close() { close(f.done) }

// The 0.13.0 CPU-spin class: every envelope MUST be acked (unknown types
// included), redelivered events dedup by ts, and a disconnect notice ends
// the connection for a clean reconnect.
func TestReadLoopAcksAndDedups(t *testing.T) {
	s := NewSlack("bot", "app", "U123", nil)
	s.BindThread("111.222", "item-1")
	relay := &fakeRelay{}
	ws := &fakeWS{done: make(chan struct{})}
	ws.frames = [][]byte{
		[]byte(`{"type":"hello","envelope_id":"e0"}`),
		[]byte(`{"type":"events_api","envelope_id":"e1","payload":{"event":{"type":"message","user":"U123","text":"approve","thread_ts":"111.222","ts":"5","channel":"D1","channel_type":"im"}}}`),
		[]byte(`{"type":"events_api","envelope_id":"e2","payload":{"event":{"type":"message","user":"U123","text":"approve","thread_ts":"111.222","ts":"5","channel":"D1","channel_type":"im"}}}`),
		[]byte(`{"type":"something_else","envelope_id":"e3"}`),
		[]byte(`{"type":"disconnect"}`),
	}
	err := s.readLoop(context.Background(), ws, relay)
	if err == nil {
		t.Fatal("disconnect must end the read loop")
	}
	if len(relay.applied) != 1 {
		t.Fatalf("redelivered event must dedup: %v", relay.applied)
	}
	acks := 0
	for _, w := range ws.writes {
		if strings.HasPrefix(w, `{"envelope_id"`) {
			acks++
		}
	}
	if acks != 4 {
		t.Fatalf("every envelope must be acked (incl. unknown types), got %d: %v", acks, ws.writes)
	}
}

// Socket Mode URLs carry no port; the dial address must default it or
// tls.Dial dies with "missing port in address" (found live — the relay
// had never connected).
func TestDialAddrDefaultsPort(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"wss://wss-primary.slack.com/link/?ts=1", "wss-primary.slack.com:443"},
		{"wss://host.evil:8443/x", "host.evil:8443"},
		{"ws://127.0.0.1/debug", "127.0.0.1:80"},
	} {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := dialAddr(u); got != tc.want {
			t.Errorf("dialAddr(%s) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}
