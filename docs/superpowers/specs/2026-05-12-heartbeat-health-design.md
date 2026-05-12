# Heartbeat & Health Monitoring Design

**Date:** 2026-05-12
**Status:** Approved
**Part of:** Skopos v1.1

## Summary

Add passive staleness detection to Skopos. A background ticker sweeps `agent_states` every 60 seconds and marks agents that have been in an active status for longer than the configured threshold (default 15 minutes) as `stuck`. Sessions whose every agent is stuck or terminal are marked `orphaned`. Both states surface through the existing API with no new endpoints. Recovery is automatic — the next report from a stuck agent clears the stuck state.

## Goals

- Detect agents that have gone silent without requiring changes to any agent integration.
- Surface stuck agents and orphaned sessions in the existing dashboard via the existing polling API.
- Keep the implementation small: one new package, two new schema columns, two new status values.

## Non-Goals

- Active heartbeat endpoint (agents do not need to ping Skopos).
- Notifications or webhooks on stuck detection (API-only for now).
- HITL as a distinct status — the existing `waiting` status covers it; `waiting` agents are exempt from stuck detection.

## Architecture

A new `internal/health/` package owns all staleness logic. It runs as a background goroutine started from `serve.go` alongside the HTTP and MCP servers, using the server's context for clean shutdown.

**Ticker loop (every 60 seconds):**
1. Query `agent_states` for agents in active statuses with `updated_at` older than the threshold.
2. For each match: copy `status → original_status`, set `status = 'stuck'`, set `stuck_at = now()`. Insert a synthetic `events` row (`status = 'stuck'`, `message = 'agent not responding'`).
3. Update sessions: any session not already terminal whose every agent is in `{stuck, succeeded, failed, cancelled, handoff}` → `status = 'orphaned'`.

Both sweeps run in a single transaction so the session orphan check sees the freshly-marked stuck agents.

**Active statuses** (trigger stuck detection): `pending`, `running`, `thinking`, `planning`, `editing`, `testing`.

**Exempt statuses** (never trigger stuck detection): `waiting`, `paused`, `blocked`, `handoff`, `succeeded`, `failed`, `cancelled`.

**Recovery**: When a stuck agent sends a new `POST /api/reports`, the existing service upsert overwrites `status` with the new value and clears `original_status` and `stuck_at` (null). No special recovery code path — one added clause to the existing upsert query.

## Schema Changes

**No migration.** The database is wiped and recreated from `schema.sql`.

### `agent_states` — two new nullable columns

```sql
original_status TEXT,   -- last self-reported status before stuck; NULL when healthy
stuck_at        TEXT,   -- ISO timestamp when Skopos marked this agent stuck; NULL when healthy
```

### New status values

Added to `models.go` `Status` type:
- `stuck` — written only by the health ticker, never self-reported by agents.
- `orphaned` — session-level only; written only by the health ticker.

### `sessions` — no new columns

The existing `status TEXT` column accepts `orphaned` as a new valid value.

## Package: `internal/health/`

### `ticker.go`

```go
type Checker struct {
    db        *sql.DB
    threshold time.Duration
    interval  time.Duration
}

func NewChecker(db *sql.DB, threshold time.Duration) *Checker
func (c *Checker) Start(ctx context.Context)        // launches goroutine
func (c *Checker) check(ctx context.Context) error  // single sweep, used in tests
```

`Start` returns immediately and runs `check` on a ticker until the context is cancelled.

### `ticker_test.go`

Tests run against a real `:memory:` SQLite DB seeded via `internal/db` migrations:

| Test | Assertion |
|---|---|
| Active agent stale >threshold | Marked `stuck`, `original_status` set, `stuck_at` set, event inserted |
| Active agent stale <threshold | Untouched |
| Agent in `waiting` | Untouched (exempt) |
| Agent in `succeeded` | Untouched (terminal) |
| Session: all agents stuck/terminal | Session → `orphaned` |
| Session: one agent still active | Session untouched |
| Recovery: stuck agent reports | `status` restored, `original_status` = NULL, `stuck_at` = NULL |

## Configuration

New `[health]` section in `skopos-config.toml`:

```toml
[health]
stuck_threshold_minutes = 15
```

New flag on the `serve` command:

```
--health-stuck-threshold   int   Minutes before an active agent is marked stuck (default 15)
                                 config: health.stuck_threshold_minutes
                                 env:    HEALTH_STUCK_THRESHOLD
```

## API Changes

No new endpoints. `stuck` and `orphaned` are new valid values in the existing `status` fields.

`AgentState` gains two optional JSON fields:

```json
{
  "status": "stuck",
  "original_status": "running",
  "stuck_at": "2026-05-12T10:23:00Z"
}
```

Both fields are omitted (`omitempty`) when the agent is healthy.

## Dashboard Changes

The Alpine.js status badge mapping gets two new entries:
- `stuck` → amber/warning colour, label "Stuck"
- `orphaned` → red/muted colour, label "Orphaned"

No structural dashboard changes — the badge component already handles statuses gracefully.

## Test Plan

- `internal/health/ticker_test.go` — all cases in the table above, real SQLite `:memory:` DB.
- `internal/status/service_test.go` — one new test: reporting from a stuck agent clears `original_status` and `stuck_at`.
- Run: `task test` covers everything.
