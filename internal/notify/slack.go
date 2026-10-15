package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Slack is the Socket Mode provider: an outbound websocket to Slack, so no
// public URL is needed. Martin's replies in item threads come back over the
// socket and flow into the relay; only his user id is honored.
type Slack struct {
	botToken string
	appToken string
	martinID string
	http     *http.Client
	log      Logger
	mu       sync.Mutex
	threads  map[string]string // thread ts -> item id
	ack      func(ts string)
}

// Logger is the subset of slog the provider needs (keeps the package
// testable without a logger wired).
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

func NewSlack(botToken, appToken, martinUserID string, log Logger) *Slack {
	if log == nil {
		log = nopLogger{}
	}
	return &Slack{
		botToken: botToken, appToken: appToken, martinID: martinUserID,
		http:    &http.Client{Timeout: 30 * time.Second},
		log:     log,
		threads: map[string]string{},
	}
}

type nopLogger struct{}

func (nopLogger) Info(msg string, args ...any) {}
func (nopLogger) Warn(msg string, args ...any) {}

func (s *Slack) Name() string { return "slack" }

// Send posts a message via chat.postMessage; parent = thread_ts when set.
func (s *Slack) Send(ctx context.Context, parent, text string) (string, error) {
	form := url.Values{"channel": {dmChannel.Load()}, "text": {text}}
	if parent != "" {
		form.Set("thread_ts", parent)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/chat.postMessage", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+s.botToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.http.Do(req)
	if err != nil {
		s.log.Warn("slack send failed", "err", err)
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		TS      string `json:"ts"`
		Channel string `json:"channel"`
	}
	if err := json.Unmarshal(body, &out); err != nil || !out.OK {
		s.log.Warn("slack send rejected", "status", resp.StatusCode, "body", truncBody(body))
		return "", fmtAPIError("slack post", resp.StatusCode, string(body))
	}
	setDMChannel(out.Channel)
	return out.TS, nil
}

func truncBody(b []byte) string {
	if len(b) > 200 {
		return string(b[:200])
	}
	return string(b)
}

// dmChannel holds the bot's DM channel with Martin (resolved on first send).
var dmChannel atomicString

type atomicString struct {
	mu  sync.Mutex
	val string
}

func (a *atomicString) Load() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.val
}

func (a *atomicString) Set(v string) {
	a.mu.Lock()
	a.val = v
	a.mu.Unlock()
}

func setDMChannel(id string) { dmChannel.Set(id) }

// Start runs the Socket Mode loop: connect via the apps.connections.open
// handshake, then read events. Messages from Martin in known item threads
// become relay commands; handled ones get a ✅ so they never replay.
//
// The websocket transport is abstracted behind wsConn so the provider is
// testable without Slack (and so a future transport swap — e.g. the
// socketmode client library — is one function).
func (s *Slack) Start(ctx context.Context, relay Relay) error {
	backoff := time.Second
	for ctx.Err() == nil {
		ws, err := s.connect(ctx)
		if err != nil {
			if se, ok := err.(staticErr); ok {
				return se // config errors don't retry
			}
			if !sleep(ctx, backoff) {
				return ctx.Err()
			}
			backoff = min(backoff*2, 60*time.Second)
			continue
		}
		connected := time.Now()
		s.log.Info("slack socket mode connected")
		err = s.readLoop(ctx, ws, relay)
		ws.Close()
		s.log.Info("slack socket mode disconnected", "err", fmt.Sprint(err))
		// Backoff resets only when a connection LIVED (a healthy socket
		// that later dropped deserves a fast retry); a connect-then-drop
		// cycle keeps growing the backoff instead of spinning.
		if time.Since(connected) >= 10*time.Second {
			backoff = time.Second
		}
		if !sleep(ctx, backoff) {
			return ctx.Err()
		}
		backoff = min(backoff*2, 60*time.Second)
	}
	return ctx.Err()
}

type staticErr struct{ err error }

func (e staticErr) Error() string { return e.err.Error() }

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// wsConn is the transport: sequential text frames.
type wsConn interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, data []byte) error
	Close()
}

func (s *Slack) connect(ctx context.Context) (wsConn, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/apps.connections.open", nil)
	req.Header.Set("Authorization", "Bearer "+s.appToken)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		OK  bool   `json:"ok"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &out); err != nil || !out.OK || out.URL == "" {
		if strings.Contains(string(body), "invalid_auth") {
			return nil, staticErr{fmt.Errorf("slack socket mode: invalid app token")}
		}
		return nil, fmtAPIError("apps.connections.open", resp.StatusCode, string(body))
	}
	return dialWS(ctx, out.URL)
}

// readLoop dispatches one envelope at a time. EVERY envelope must be
// acknowledged ({"envelope_id": ...} back over the socket) — Slack
// redelivers un-acked envelopes and drops connections from clients that
// stay silent, which turns into a redelivery/reconnect churn (the 0.13.0
// CPU spin).
func (s *Slack) readLoop(ctx context.Context, ws wsConn, relay Relay) error {
	seen := &seenSet{}
	for {
		data, err := ws.Read(ctx)
		if err != nil || ctx.Err() != nil {
			return err
		}
		var env struct {
			EnvelopeID string          `json:"envelope_id"`
			Type       string          `json:"type"`
			Body       json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(data, &env) != nil {
			continue
		}
		// Ack first, dispatch second: even unknown types are acked, so
		// Slack never queues them for redelivery.
		if env.EnvelopeID != "" {
			if werr := ws.Write(ctx, []byte(`{"envelope_id":`+strconv.Quote(env.EnvelopeID)+`}`)); werr != nil {
				s.log.Warn("slack ack write failed", "err", werr)
			}
		}
		switch env.Type {
		case "events_api":
			s.handleEvent(ctx, ws, env.Body, relay, seen)
		case "disconnect":
			// Slack wants us on a new connection; drop this one now
			// instead of waiting for the TCP timeout.
			return fmt.Errorf("slack disconnect notice")
		case "hello", "slack_events":
			// connection established / keepalive
		}
	}
}

// seenSet is a bounded redelivery dedup (message ts).
type seenSet struct{ m map[string]bool }

func (s *seenSet) dup(ts string) bool {
	if s.m == nil {
		s.m = map[string]bool{}
	}
	if s.m[ts] {
		return true
	}
	if len(s.m) > 512 {
		s.m = map[string]bool{}
	}
	s.m[ts] = true
	return false
}

func trunc80(s string) string {
	if len(s) > 80 {
		return s[:80]
	}
	return s
}

func (s *Slack) handleEvent(ctx context.Context, ws wsConn, body json.RawMessage, relay Relay, seen *seenSet) {
	var ev struct {
		Event struct {
			Type     string `json:"type"`
			User     string `json:"user"`
			Text     string `json:"text"`
			ThreadTS string `json:"thread_ts"`
			TS       string `json:"ts"`
			Channel  string `json:"channel"`
		} `json:"event"`
	}
	if json.Unmarshal(body, &ev) != nil {
		return
	}
	e := ev.Event
	s.log.Info("slack event", "type", e.Type, "user", e.User, "channel", e.Channel, "thread", e.ThreadTS, "ts", e.TS, "text", trunc80(e.Text))
	// The DM channel id rides on every inbound event — learn it here, or
	// the FIRST outbound post has no channel to post to (the id was only
	// being learned from a successful send, which never happens without
	// one: chicken and egg, and every relay reply silently failed).
	if e.Channel != "" {
		setDMChannel(e.Channel)
	}
	if e.Type != "message" || e.User != s.martinID {
		return // only Martin's messages count (bot echoes included)
	}
	// Slack redelivers events that were slow to process — the ts dedup
	// keeps a redelivered command from applying twice.
	if seen.dup(e.TS) {
		return
	}
	if e.ThreadTS == "" {
		// Top-level DM: capture ("new: ...") and help. Handled ones get the
		// same ✅ ack as thread replies.
		if relay.TopLevel(ctx, e.Text, e.TS) {
			s.ackMessage(ctx, e.Channel, e.TS)
		}
		return
	}
	s.mu.Lock()
	itemID := s.threads[e.ThreadTS]
	s.mu.Unlock()
	if itemID == "" {
		// Unknown thread root: after a restart the in-memory map is empty —
		// ask the relay (it persists bindings) before dropping the reply.
		itemID = relay.ItemForThread(e.ThreadTS)
		if itemID != "" {
			s.BindThread(e.ThreadTS, itemID)
		}
	}
	if itemID == "" {
		return
	}
	if relay.Apply(ctx, itemID, e.Text) {
		s.ackMessage(ctx, e.Channel, e.TS)
	}
}

// ackMessage reacts with ✅ so a handled message is never processed twice
// (the reaction lives on Slack's side — it survives restarts).
func (s *Slack) ackMessage(ctx context.Context, channel, ts string) {
	form := url.Values{"channel": {channel}, "timestamp": {ts}, "name": {"white_check_mark"}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/reactions.add", strings.NewReader(form.Encode()))
	req.Header.Set("Authorization", "Bearer "+s.botToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_, _ = s.http.Do(req)
}

// BindThread records which item a new thread belongs to.
func (s *Slack) BindThread(ts, itemID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.threads[ts] = itemID
}
