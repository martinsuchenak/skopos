package integrations

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
)

// ErrGone: the cursor points before the oldest kept event, so events were
// missed; the reader resyncs from the state endpoint.
type ErrGone struct{ Oldest int64 }

func (e *ErrGone) Error() string {
	return fmt.Sprintf("events after this cursor are no longer kept; the oldest is %d — resync from /api/inbox/{id}/state", e.Oldest)
}

// FeedQuery selects events after a cursor.
type FeedQuery struct {
	After       int64
	Types       []string
	WorkspaceID string
	Limit       int
	Wait        time.Duration // long-poll up to this long when nothing is new
}

// FeedPage is one page: the events, and the cursor for the next call.
type FeedPage struct {
	Events    []Payload `json:"events"`
	NextAfter int64     `json:"next_after"`
}

const (
	maxFeedLimit = 500
	maxFeedWait  = 25 * time.Second
)

// Feed reads events after q.After in the caller's scope (a scoped key sees
// its workspaces' events; root sees all), oldest first.
func (s *Service) Feed(ctx context.Context, q FeedQuery) (*FeedPage, error) {
	if q.Limit <= 0 || q.Limit > maxFeedLimit {
		q.Limit = 100
	}
	if q.Wait > maxFeedWait {
		q.Wait = maxFeedWait
	}
	for _, t := range q.Types {
		if !knownType(t) {
			return nil, fmt.Errorf("%w: unknown event type %q", ErrInvalidInput, t)
		}
	}
	if q.WorkspaceID != "" {
		if err := auth.RequireWorkspace(ctx, q.WorkspaceID); err != nil {
			return nil, err
		}
	}
	deadline := time.Now().Add(q.Wait)
	for {
		var woken <-chan struct{}
		if s.emitter != nil {
			woken = s.emitter.Woken()
		}
		page, err := s.feedOnce(ctx, q)
		if err != nil || len(page.Events) > 0 || q.Wait <= 0 {
			return page, err
		}
		left := time.Until(deadline)
		if left <= 0 {
			return page, nil
		}
		select {
		case <-ctx.Done():
			return page, nil
		case <-woken:
		case <-time.After(min(left, time.Second)):
		}
	}
}

func (s *Service) feedOnce(ctx context.Context, q FeedQuery) (*FeedPage, error) {
	var oldest, newest, everIssued int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MIN(seq), 0), COALESCE(MAX(seq), 0),
		COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'integration_events'), 0) FROM integration_events`).Scan(&oldest, &newest, &everIssued); err != nil {
		return nil, err
	}
	// AUTOINCREMENT never reuses a seq: rows between the cursor and the
	// oldest one were pruned — or, with every row pruned, those up to the
	// highest seq ever issued.
	if oldest == 0 {
		oldest = everIssued + 1
	}
	if q.After > 0 && oldest > q.After+1 {
		return nil, &ErrGone{Oldest: oldest}
	}
	where := []string{"seq > ?", "type <> ?"}
	args := []any{q.After, TypeWebhookTest}
	if q.WorkspaceID != "" {
		where = append(where, "workspace_id = ?")
		args = append(args, q.WorkspaceID)
	} else if auth.ScopedContext(ctx) {
		list := auth.PrincipalFromContext(ctx).WorkspaceList()
		if len(list) == 0 {
			return &FeedPage{Events: []Payload{}, NextAfter: max(q.After, newest)}, nil
		}
		where = append(where, "workspace_id IN (?"+strings.Repeat(", ?", len(list)-1)+")")
		for _, ws := range list {
			args = append(args, ws)
		}
	}
	if len(q.Types) > 0 {
		where = append(where, "type IN (?"+strings.Repeat(", ?", len(q.Types)-1)+")")
		for _, t := range q.Types {
			args = append(args, t)
		}
	}
	args = append(args, q.Limit)
	rows, err := s.db.QueryContext(ctx, `SELECT `+eventColumns+` FROM integration_events WHERE `+strings.Join(where, " AND ")+` ORDER BY seq LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	page := &FeedPage{Events: []Payload{}, NextAfter: q.After}
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		page.Events = append(page.Events, ev.Payload(s.PublicURL))
		page.NextAfter = ev.Seq
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// A short page read everything up to newest the caller may see: skip
	// the cursor past rows filtered out, so the next call doesn't rescan.
	if len(page.Events) < q.Limit && newest > page.NextAfter {
		page.NextAfter = newest
	}
	return page, nil
}

// parseTypes splits a comma-separated types parameter.
func parseTypes(v string) []string {
	var out []string
	for _, t := range strings.Split(v, ",") {
		if t = strings.TrimSpace(t); t != "" && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

func isGone(err error) (*ErrGone, bool) {
	var g *ErrGone
	ok := errors.As(err, &g)
	return g, ok
}
