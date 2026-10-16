package inbox

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

type recordingNotifier struct {
	mu    sync.Mutex
	seen  map[string][]string
	total int
	all   chan struct{}
	want  int
	ctxOK bool
}

func (r *recordingNotifier) NotifyItem(ctx context.Context, itemID string, from, to Status) {
	time.Sleep(time.Millisecond) // a slow provider: interleave the shards
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := ctx.Deadline(); ctx.Err() != nil || !ok {
		r.ctxOK = false
	}
	r.seen[itemID] = append(r.seen[itemID], string(to))
	r.total++
	if r.total == r.want {
		close(r.all)
	}
}

// Notifications for one item arrive in the order they were raised, even
// with several items in flight and the request contexts already cancelled
// (the response was written long before the provider runs).
func TestAsyncNotifierOrderPerItem(t *testing.T) {
	ctx := t.Context()
	const items, per = 6, 20
	rec := &recordingNotifier{seen: map[string][]string{}, all: make(chan struct{}), want: items * per, ctxOK: true}
	a := newAsyncNotifier(ctx, rec, nil, 3, items*per, time.Minute)

	for i := range per {
		for it := range items {
			reqCtx, reqCancel := context.WithCancel(context.Background())
			a.NotifyItem(reqCtx, fmt.Sprintf("item-%d", it), "", Status(fmt.Sprintf("s%02d", i)))
			reqCancel()
		}
	}
	select {
	case <-rec.all:
	case <-time.After(10 * time.Second):
		t.Fatal("not every notification was delivered")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for it := range items {
		got := rec.seen[fmt.Sprintf("item-%d", it)]
		for i, s := range got {
			if s != fmt.Sprintf("s%02d", i) {
				t.Fatalf("item-%d out of order: %v", it, got)
			}
		}
	}
	if !rec.ctxOK {
		t.Fatal("delivery ctx must be live (WithoutCancel) and carry its own deadline")
	}
}

type blockingNotifier struct {
	release chan struct{}
	calls   chan string
}

func (b *blockingNotifier) NotifyItem(ctx context.Context, itemID string, from, to Status) {
	b.calls <- string(to)
	if to == "panic" {
		panic("boom")
	}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
}

type countingLogger struct {
	mu   sync.Mutex
	msgs []string
}

func (c *countingLogger) Warn(msg string, args ...any) {
	c.mu.Lock()
	c.msgs = append(c.msgs, msg)
	c.mu.Unlock()
}

// A stuck provider never blocks the caller: a full queue drops with a
// warning, and the per-call timeout frees the shard.
func TestAsyncNotifierNeverBlocksCaller(t *testing.T) {
	ctx := t.Context()
	b := &blockingNotifier{release: make(chan struct{}), calls: make(chan string, 100)}
	log := &countingLogger{}
	a := newAsyncNotifier(ctx, b, log, 1, 2, 200*time.Millisecond)

	// The first notification is in flight (stuck) before the flood.
	a.NotifyItem(context.Background(), "item", "", "0")
	<-b.calls
	start := time.Now()
	for i := 1; i < 10; i++ {
		a.NotifyItem(context.Background(), "item", "", Status(fmt.Sprint(i)))
	}
	if time.Since(start) > time.Second {
		t.Fatalf("NotifyItem blocked for %s", time.Since(start))
	}
	log.mu.Lock()
	dropped := len(log.msgs)
	log.mu.Unlock()
	if dropped == 0 {
		t.Fatal("a full queue must drop with a warning")
	}
	// The per-call timeout frees the stuck shard: the queued two run.
	for range 2 {
		select {
		case <-b.calls:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout must free the shard for the queued notifications")
		}
	}
}

// A panicking notifier is recovered; the shard keeps delivering.
func TestAsyncNotifierRecoversPanic(t *testing.T) {
	ctx := t.Context()
	b := &blockingNotifier{release: make(chan struct{}), calls: make(chan string, 10)}
	close(b.release)
	log := &countingLogger{}
	a := newAsyncNotifier(ctx, b, log, 1, 4, time.Second)
	a.NotifyItem(context.Background(), "item", "", "panic")
	a.NotifyItem(context.Background(), "item", "", "after")
	for _, want := range []string{"panic", "after"} {
		select {
		case got := <-b.calls:
			if got != want {
				t.Fatalf("got %q want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%q never delivered", want)
		}
	}
}
