package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	mu       sync.Mutex
	threads  map[string]string // thread ts -> item id
	ack      func(ts string)
}

func NewSlack(botToken, appToken, martinUserID string) *Slack {
	return &Slack{
		botToken: botToken, appToken: appToken, martinID: martinUserID,
		http:    &http.Client{Timeout: 30 * time.Second},
		threads: map[string]string{},
	}
}

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
		return "", fmtAPIError("slack post", resp.StatusCode, string(body))
	}
	setDMChannel(out.Channel)
	return out.TS, nil
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
		backoff = time.Second
		err = s.readLoop(ctx, ws, relay)
		ws.Close()
		if !sleep(ctx, backoff) {
			return ctx.Err()
		}
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

// readLoop dispatches one envelope at a time.
func (s *Slack) readLoop(ctx context.Context, ws wsConn, relay Relay) error {
	for {
		data, err := ws.Read(ctx)
		if err != nil || ctx.Err() != nil {
			return err
		}
		var env struct {
			Type string          `json:"type"`
			Body json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(data, &env) != nil {
			continue
		}
		switch env.Type {
		case "events_api":
			s.handleEvent(ctx, ws, env.Body, relay)
		case "hello", "slack_events":
			// connection established / keepalive
		}
	}
}

func (s *Slack) handleEvent(ctx context.Context, ws wsConn, body json.RawMessage, relay Relay) {
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
	if e.Type != "message" || e.User != s.martinID {
		return // only Martin's replies count
	}
	s.mu.Lock()
	itemID := s.threads[e.ThreadTS]
	s.mu.Unlock()
	if itemID == "" {
		return
	}
	if relay.Apply(ctx, itemID, e.Text) {
		// ack with a reaction so the reply is never processed twice
		form := url.Values{"channel": {e.Channel}, "timestamp": {e.TS}, "name": {"white_check_mark"}}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/reactions.add", strings.NewReader(form.Encode()))
		req.Header.Set("Authorization", "Bearer "+s.botToken)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		_, _ = s.http.Do(req)
	}
}

// BindThread records which item a new thread belongs to.
func (s *Slack) BindThread(ts, itemID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.threads[ts] = itemID
}
