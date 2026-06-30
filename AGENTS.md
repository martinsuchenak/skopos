# AGENTS

## Project

- Name: `skopos`
- Module: `github.com/martinsuchenak/skopos`
- Go 1.26.3, SQLite (modernc.org/sqlite — no CGO), Bun for frontend

## Commands

```sh
task build-local      # build for current OS (runs frontend-build first)
task build            # cross-compile for linux/darwin amd64+arm64
task test             # go test ./... -v -count=1
task lint             # golangci-lint run .
task frontend-build   # bun install && bun run build in web/
```

Running a single test package: `go test ./internal/status/... -v -count=1`

The server must be running for MCP/REST integration testing: `go run . serve` (HTTP on :8080, MCP at /mcp).

## Architecture

```
main.go → cmd/register.go → cmd/serve.go
                              ├── cmd/routes/       (HTTP handlers)
                              ├── cmd/mcp/          (MCP tool definitions)
                              └── cmd/              (CLI commands: report, blackboard, plans)
internal/
  ├── status/     handler → service → storage   (agent status, sessions, events)
  ├── blackboard/ handler → service → storage   (scoped knowledge entries)
  ├── plans/      handler → service → storage   (plans, items, dependencies)
  ├── workspaces/ handler → service → storage   (workspace registry, auto-register)
  ├── events/     in-process SSE hub + middleware (publishes named events on mutations)
  ├── install/    skopos install — wires MCP config into AI agent configs
  ├── auth/       API key auth (X-API-Key or Authorization: Bearer, write-only)
  ├── health/     background goroutine: stuck-agent detection
  ├── cleanup/    background goroutine: data retention cleanup
  ├── db/         SQLite connection + schema.sql migrations
  └── valkey/     Valkey client (SRV DNS support, not wired in by default)
web/              Alpine.js + Tailwind CSS, embedded via go:embed, Bun build
```

Every domain package follows `handler → service → storage` layering. Storage uses raw `*sql.DB`. Services are interface-based for testability.

## Key Patterns

- Commands self-register via `init()` in `cmd/` using `Register()`.
- Routes self-register via `init()` in `cmd/routes/`.
- `go-scaffolder:` comments are patch markers — do not remove them.
- Add new CLI commands, API endpoints, or MCP tools with `go-scaffolder add` where possible.
- All IDs are UUIDv7 (text, time-sortable).
- Config: TOML (`skopos-config.toml`, gitignored — copy from `skopos-config.example.toml`; optional, falls back to flag defaults + env) + env var overrides (`SERVER_PORT`, `SKOPOS_API_KEY`, etc.).
- Frontend assets are embedded in the binary via `web/embed.go`. Run `task frontend-build` before `task build` (the build task depends on it automatically).
- `task lint` sets `GOCACHE` to a local directory — don't run bare `golangci-lint`.
- The dashboard subscribes to `/api/events/stream` (SSE) for real-time updates; the `events` package's middleware publishes named events on successful mutations.
- Workspaces are strict-scoped: blackboard entries and plans require an exact `workspace_id` match when filtered. Session-derived workspaces are auto-registered so they persist.

## Blackboard

Three scopes: `session` (requires `session_id`, cascade-deleted with session via the `blackboard_entries.session_id` foreign key), `branch` (requires `branch_name`), `project` (global).
Six entry types: `finding`, `decision`, `warning`, `context`, `bug`, `debt`.
`bug` and `debt` are "floating" — always included in reads regardless of branch filter.
Read returns both structured `entries` array and a `markdown_bundle` text block.

## Plans

Item statuses: `pending`, `in_progress`, `done`, `blocked`.
`add_dependency` auto-blocks the dependent item. Completing a dependency auto-unblocks dependents.
`remove_dependency` auto-unblocks if remaining deps are all done.
Plan-to-plan dependencies work the same way — adding one blocks the dependent plan, completing the dependency plan auto-unblocks it.

## Database

Schema lives in `internal/db/schema.sql`. On `serve`, `db.RunMigrations(conn.SQL)` runs all CREATE statements. No separate migration files — schema.sql is the source of truth and is re-run with `IF NOT EXISTS`. The DB is file-based SQLite (`skopos.db` by default).
