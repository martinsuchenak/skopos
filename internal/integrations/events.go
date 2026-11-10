// Package integrations tells other systems what happens to items
// (tmp/integrations — design.md §3): every event is a row in
// integration_events, written in the transaction of the change that caused
// it; webhook subscriptions get each event delivered at least once, signed
// per Standard Webhooks (standardwebhooks.com); GET /api/events serves the
// same rows as a feed. Payloads are thin — ids and state — and the
// receiver reads the details from GET /api/inbox/{id}/state.
package integrations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/martinsuchenak/skopos/internal/ids"
)

// Event types.
const (
	TypeItemCreated       = "item.created"
	TypeItemStatusChanged = "item.status_changed"
	TypeItemNeedsYou      = "item.needs_you"
	TypeItemClosed        = "item.closed"
	TypePlanReady         = "plan.ready"
	TypeQuestionsWaiting  = "questions.waiting"
	TypeQuestionsAnswered = "questions.answered"
	TypeRunStarted        = "run.started"
	TypeRunFinished       = "run.finished"
	TypeChecksFinished    = "checks.finished"
	TypeReviewFinished    = "review.finished"
	TypePROpened          = "pr.opened"
	TypeWebhookTest       = "webhook.test"
)

// Types is every type a subscription may name.
var Types = []string{
	TypeItemCreated, TypeItemStatusChanged, TypeItemNeedsYou, TypeItemClosed, TypePlanReady,
	TypeQuestionsWaiting, TypeQuestionsAnswered, TypeRunStarted, TypeRunFinished,
	TypeChecksFinished, TypeReviewFinished, TypePROpened,
}

// optIn types are high volume: a subscription gets them only by naming them.
var optIn = map[string]bool{TypeRunStarted: true, TypeRunFinished: true}

func knownType(t string) bool {
	for _, k := range Types {
		if k == t {
			return true
		}
	}
	return false
}

// Event is what a producer records. Data holds the type's own fields; the
// item's workspace, ref, external_url and status are added from the item
// at write time.
type Event struct {
	Type        string
	WorkspaceID string
	ItemID      string
	Data        map[string]any
}

// DB is what Emit writes through: a *sql.DB or the producer's *sql.Tx.
type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Emitter records events and wakes their readers (the dispatcher, feed
// long-polls). A nil *Emitter records nothing, so producers and their tests
// run without it.
type Emitter struct {
	mu  sync.Mutex
	ch  chan struct{} // closed and replaced on every emit
	now func() time.Time
}

func NewEmitter() *Emitter { return &Emitter{ch: make(chan struct{}), now: time.Now} }

// Emit writes the events through db — the producer's transaction, so an
// event exists exactly when its change does.
func (e *Emitter) Emit(ctx context.Context, db DB, evs ...Event) error {
	if e == nil || len(evs) == 0 {
		return nil
	}
	for _, ev := range evs {
		if err := insert(ctx, db, ev, e.now()); err != nil {
			return err
		}
	}
	e.wake()
	return nil
}

func (e *Emitter) wake() {
	e.mu.Lock()
	close(e.ch)
	e.ch = make(chan struct{})
	e.mu.Unlock()
}

// wakeIfAny wakes readers when there is an emitter.
func (e *Emitter) wakeIfAny() {
	if e != nil {
		e.wake()
	}
}

// Woken is closed by the next emit.
func (e *Emitter) Woken() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ch
}

func insert(ctx context.Context, db DB, ev Event, now time.Time) error {
	data := map[string]any{}
	for k, v := range ev.Data {
		data[k] = v
	}
	if ev.ItemID != "" {
		data["item_id"] = ev.ItemID
		var ws, ref, extURL, status sql.NullString
		err := db.QueryRowContext(ctx, `SELECT workspace_id, ref, external_url, status FROM inbox_items WHERE id = ?`, ev.ItemID).
			Scan(&ws, &ref, &extURL, &status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("reading the event's item: %w", err)
		}
		if ev.WorkspaceID == "" {
			ev.WorkspaceID = ws.String
		}
		setIf(data, "ref", ref.String)
		setIf(data, "external_url", extURL.String)
		if _, ok := data["status"]; !ok {
			setIf(data, "status", status.String)
		}
	}
	if ev.WorkspaceID != "" {
		data["workspace_id"] = ev.WorkspaceID
	}
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encoding event data: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO integration_events (id, type, workspace_id, item_id, occurred_ms, data) VALUES (?, ?, ?, ?, ?, ?)`,
		"msg_"+ids.New(), ev.Type, ev.WorkspaceID, ev.ItemID, now.UnixMilli(), string(b)); err != nil {
		return fmt.Errorf("recording event %s: %w", ev.Type, err)
	}
	return nil
}

func setIf(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}

// Stored is an event row.
type Stored struct {
	Seq         int64
	ID          string
	Type        string
	WorkspaceID string
	ItemID      string
	OccurredMS  int64
	Data        map[string]any
}

// Payload is the delivered and fed form (Standard Webhooks' {type,
// timestamp, data}); data carries seq and links.
type Payload struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Timestamp string         `json:"timestamp"`
	Data      map[string]any `json:"data"`
}

// Payload renders the event; publicURL ("" = no links) is where the
// server answers.
func (s Stored) Payload(publicURL string) Payload {
	data := map[string]any{}
	for k, v := range s.Data {
		data[k] = v
	}
	data["seq"] = s.Seq
	if publicURL != "" && s.ItemID != "" {
		data["links"] = map[string]string{
			"api":       publicURL + "/api/inbox/" + s.ItemID + "/state",
			"dashboard": publicURL + "/?item=" + s.ItemID,
		}
	}
	return Payload{ID: s.ID, Type: s.Type, Timestamp: time.UnixMilli(s.OccurredMS).UTC().Format("2006-01-02T15:04:05.000Z"), Data: data}
}

const eventColumns = `seq, id, type, workspace_id, item_id, occurred_ms, data`

type rowScanner interface{ Scan(dest ...any) error }

func scanEvent(r rowScanner) (Stored, error) {
	var s Stored
	var data string
	if err := r.Scan(&s.Seq, &s.ID, &s.Type, &s.WorkspaceID, &s.ItemID, &s.OccurredMS, &data); err != nil {
		return s, err
	}
	_ = json.Unmarshal([]byte(data), &s.Data)
	return s, nil
}
