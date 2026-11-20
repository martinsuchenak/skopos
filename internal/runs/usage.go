package runs

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/martinsuchenak/skopos/internal/usage"
)

// Usage is what a run's agents used, as their runners reported it (the
// shared record, internal/usage), summed over the run's agent calls and
// attempts: each report carries only what its attempt used, like the cost.
type Usage = usage.Usage

// parseUsage reads the stored column (empty = none).
func parseUsage(raw string) *Usage {
	if raw == "" {
		return nil
	}
	var u Usage
	if json.Unmarshal([]byte(raw), &u) != nil || u.Empty() {
		return nil
	}
	return &u
}

// addUsage is the run's usage with a report's added (within the stored
// record's bounds), for `usage = COALESCE(?, usage)`: nil (keep what is
// stored) when the report carries none.
func addUsage(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string, in *Usage) (any, error) {
	if in.Empty() {
		return nil, nil
	}
	var raw string
	if err := q.QueryRowContext(ctx, `SELECT usage FROM runs WHERE id = ?`, id).Scan(&raw); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	sum := parseUsage(raw)
	if sum == nil {
		sum = &Usage{}
	}
	sum.Add(in)
	if sum.Empty() {
		return nil, nil
	}
	b, err := json.Marshal(sum)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}
