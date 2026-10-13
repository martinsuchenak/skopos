// Package notify is skopos's notification module: a provider interface with
// Slack first (Socket Mode — outbound only, no public URL). The gates are
// transport-agnostic: an approval is an authenticated workflow-action call,
// with the surface recorded as via. A second provider (Telegram) is a config
// change, not new gate logic.
package notify

import (
	"context"
	"fmt"
	"strings"
)

// Provider is one chat surface. Implementations must need NO inbound
// network (long-polling or outbound websocket only) — skopos can't be
// reached from the internet.
type Provider interface {
	Name() string
	// Start connects and begins delivering Martin's replies to the relay
	// until the context is cancelled. Reconnects internally.
	Start(ctx context.Context, relay Relay) error
	// Send posts a message (thread-per-item; parent identifies the thread).
	Send(ctx context.Context, parent string, text string) (ts string, err error)
}

// Relay receives Martin's commands from a provider and applies them to the
// workflow. The provider authenticates Martin its own way (user id check,
// allow-listed chat); the relay records via = provider name.
type Relay interface {
	// Apply maps a reply onto the item's workflow actions. Returns true
	// when the command was handled (so the provider may ack the message).
	Apply(ctx context.Context, itemID, text string) bool
	// ItemForThread resolves which item a thread belongs to.
	ItemForThread(parent string) string
}

// Notifier fans messages out to the configured provider (one active chat
// surface; the interface keeps a second one a config swap away).
type Notifier struct {
	provider Provider
	relay    Relay
}

func New(p Provider, r Relay) *Notifier { return &Notifier{provider: p, relay: r} }

// Run starts the provider loop (blocking; run in a goroutine).
func (n *Notifier) Run(ctx context.Context) error {
	if n.provider == nil {
		return nil // notifications disabled
	}
	return n.provider.Start(ctx, n.relay)
}

// Notify posts to an item's thread (parent = thread root ts; "" starts one).
func (n *Notifier) Notify(ctx context.Context, parent, text string) string {
	if n.provider == nil {
		return ""
	}
	ts, err := n.provider.Send(ctx, parent, cap2800(text))
	if err != nil {
		return ""
	}
	return ts
}

func cap2800(s string) string {
	const max = 2800
	if len(s) <= max {
		return s
	}
	cut := strings.LastIndex(s[:max], "\n")
	if cut < max/2 {
		cut = max
	}
	return s[:cut] + "\n… _(shortened; full detail in skopos)_"
}

// fmtAPIError keeps provider errors readable in logs.
func fmtAPIError(what string, status int, body string) error {
	return fmt.Errorf("%s: %d %s", what, status, strings.TrimSpace(body))
}
