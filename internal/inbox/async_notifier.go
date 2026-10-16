package inbox

import (
	"context"
	"fmt"
	"hash/fnv"
	"time"
)

// WarnLogger is the subset of slog the async notifier needs.
type WarnLogger interface {
	Warn(msg string, args ...any)
}

// AsyncNotifier takes item notifications off the request path (review M3):
// transition() used to call the Slack relay synchronously — a
// chat.postMessage (30s client timeout) plus draining buffered commands —
// so a worker's run report could outlive serve's WriteTimeout after the
// transition had already committed.
//
// Notifications are sharded by item id onto bounded queues, one goroutine
// per shard: an item's notifications are delivered in order, a slow item
// only delays the items hashed to its shard, and a full queue drops (with a
// warning) instead of ever blocking the caller.
type AsyncNotifier struct {
	next    ItemNotifier
	log     WarnLogger
	timeout time.Duration
	shards  []chan notifyJob
}

type notifyJob struct {
	ctx      context.Context
	itemID   string
	from, to Status
}

const (
	notifyShards  = 4
	notifyDepth   = 256
	notifyTimeout = 2 * time.Minute
)

// NewAsyncNotifier starts the dispatch goroutines; they stop when ctx is
// cancelled (queued notifications are then dropped).
func NewAsyncNotifier(ctx context.Context, next ItemNotifier, log WarnLogger) *AsyncNotifier {
	return newAsyncNotifier(ctx, next, log, notifyShards, notifyDepth, notifyTimeout)
}

func newAsyncNotifier(ctx context.Context, next ItemNotifier, log WarnLogger, shards, depth int, timeout time.Duration) *AsyncNotifier {
	a := &AsyncNotifier{next: next, log: log, timeout: timeout, shards: make([]chan notifyJob, shards)}
	for i := range a.shards {
		ch := make(chan notifyJob, depth)
		a.shards[i] = ch
		go a.run(ctx, ch)
	}
	return a
}

// NotifyItem enqueues and returns immediately. The request's ctx values
// travel with the job but not its cancellation: the response being written
// must not abort the notification.
func (a *AsyncNotifier) NotifyItem(ctx context.Context, itemID string, from, to Status) {
	job := notifyJob{ctx: context.WithoutCancel(ctx), itemID: itemID, from: from, to: to}
	select {
	case a.shards[a.shard(itemID)] <- job:
	default:
		a.warn("item notification dropped: queue full", "item", itemID, "from", string(from), "to", string(to))
	}
}

func (a *AsyncNotifier) shard(itemID string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(itemID))
	return int(h.Sum32() % uint32(len(a.shards)))
}

func (a *AsyncNotifier) run(ctx context.Context, ch <-chan notifyJob) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-ch:
			a.deliver(job)
		}
	}
}

// deliver runs one notification under its own timeout; a panic in the
// notifier is logged, never fatal to the server.
func (a *AsyncNotifier) deliver(job notifyJob) {
	ctx, cancel := context.WithTimeout(job.ctx, a.timeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			a.warn("item notifier panicked", "item", job.itemID, "panic", fmt.Sprint(r))
		}
	}()
	a.next.NotifyItem(ctx, job.itemID, job.from, job.to)
}

func (a *AsyncNotifier) warn(msg string, args ...any) {
	if a.log != nil {
		a.log.Warn(msg, args...)
	}
}
