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

Live agent-CLI tests are opt-in (they spend tokens): `SKOPOS_LIVE_CLAUDE=1` / `SKOPOS_LIVE_OPENCODE=1 go test ./internal/worker -run TestLive -v` — they define what "verified lockdown" means; re-run them after upgrading an agent CLI.

User-facing docs: the pipeline tutorial is `docs/guides/pipeline.md`, the worker reference `docs/deployment/worker.md`, config keys `docs/configuration.md` — keep them in step with worker/runner changes.

## Architecture

```
main.go → cmd/register.go → cmd/serve.go
                              ├── cmd/routes/       (HTTP handlers)
                              ├── cmd/mcp/          (MCP tool definitions)
                              └── cmd/              (CLI commands: report, blackboard, plans, worker init|install|check|runner …;
                                                     relay_slack.go = the in-process Slack relay)
internal/
  ├── status/     handler → service → storage   (agent status, sessions, events)
  ├── blackboard/ handler → service → storage   (scoped knowledge entries)
  ├── plans/      handler → service → storage   (plans, items, dependencies, revisions+locking)
  ├── inbox/      handler → service → storage   (workspace inbox: capture → enrich → convert-to-plan, agent-pipeline workflow)
  ├── workspaces/ handler → service → storage   (workspace registry, auto-register)
  ├── events/     in-process SSE hub + middleware (publishes named events on mutations)
  ├── codeindex/  code index (parse → storage → service → handler; per-workspace SQLite DBs)
  ├── install/    skopos install — wires MCP config into AI agent configs (+ Claude Code hook suite in assets/hooks/)
  ├── auth/       principals + root/scoped API key auth (Bearer); scoping enforced in services; approver permission gate
  ├── apikeys/    scoped API key domain (handler → service → storage; sk_ keys, sha256-hashed; workspace groups)
  ├── audit/      append-only mutation log (actor/via rendering, entity index, scoped reads; plan 01a0bf7c)
  ├── approvals/  append-only approvals tied to immutable subjects (plan revision+base, review head)
  ├── runs/       run queue: stages, affinity leases, chaining (agent-pipeline lean 1b)
  ├── worker/     `skopos worker`: execution plane — claim/execute/report loop, runners (claude, opencode, command), lockdown, profiles, knowledge (AGENTS.md/skills at base), account pool, probes
  ├── notify/     notification module: provider interface (Slack Socket Mode first)
  ├── health/     background goroutine: stuck-agent detection
  ├── cleanup/    background goroutine: data retention cleanup
  ├── db/         SQLite connection + schema.sql migrations
web/              Alpine.js + Tailwind CSS, embedded via go:embed, Bun build
```

Every domain package follows `handler → service → storage` layering. Storage uses raw `*sql.DB`. Services are interface-based for testability.

## Key Patterns

- Commands self-register via `init()` in `cmd/` using `Register()`.
- Routes self-register via `init()` in `cmd/routes/`.
- `go-scaffolder:` comments are patch markers — do not remove them.
- Add new CLI commands, API endpoints, or MCP tools with `go-scaffolder add` where possible.
- All IDs are UUIDv7 (text, time-sortable).
- DB timestamps go through `internal/dbtime` (fixed-width UTC text: text order = time order); never `Format(time.RFC3339Nano)` into SQLite. New tables use INTEGER Unix ms instead. Versioned migrations / typed rebuilds of the old tables are planned after the multi-agent feature is polished (tmp/db-types — review.md).
- Config: TOML (`skopos-config.toml`, gitignored — copy from `skopos-config.example.toml`; optional, falls back to flag defaults + env) + env var overrides (`SERVER_PORT`, `SKOPOS_API_KEY`, etc.).
- Frontend assets are embedded in the binary via `web/embed.go`. Run `task frontend-build` before `task build` (the build task depends on it automatically).
- `task lint` sets `GOCACHE` to a local directory — don't run bare `golangci-lint`.
- The dashboard subscribes to `/api/events/stream` (SSE) for real-time updates; the `events` package's middleware publishes named events on successful mutations.
- Dashboard markup: shared structure lives in `web/templates/partials.html` (`modal-open`/`modal-close` dialog shell, `checklist`, `ws-badge`, `index-branch`; `dict` builds the params). Status colours go through `toneOf(kind, value)` → `.tone-*` classes in `style.css` (defined once per theme) — never hand-written `bg-x-500/15 text-x-300` pairs. `TestDashboardIDsAndDialogLabels` guards ids and dialog labels.
- Workspaces are strict-scoped: blackboard entries and plans require an exact `workspace_id` match when filtered — and writes require `workspace_id` for every principal (root included). Unscoped reads for a scoped key return exactly its slice; by-id access to foreign objects 404s. Session-derived workspaces are auto-registered (system privilege) so they persist.
- API keys: root key from config + DB-backed scoped keys (internal/apikeys). The principal travels in the request context (internal/auth, ctxkeys.PrincipalKey); nil principal = internal/background caller (registrar, health ticker, refresher).
- Workspace groups: a key's scope = explicit workspaces + group members + registered workspaces matching group `path.Match` patterns. Resolved inside `LookupKey` on every request (no cache); scope changes and pattern-matching workspace registrations drop the affected keys' SSE streams. Root-only management (`skopos group …`, `/api/groups`); `skopos key who-can <workspace>` is the reverse query.
- Approver permission: `auth.RequireApprover` gates the inbox workflow's human actions; root and nil (internal) pass, plain scoped keys get `ErrApproverRequired`. Agent and worker keys never hold it.
- Agent-pipeline workflow (docs/design/agent-pipeline.md): inbox statuses + frozen transition matrix in `internal/inbox/models.go` — human actions approver-gated, system transitions (`/api/inbox/{id}/transition`) executor-only; every transition audit-logged; workflow items never change workspace, and past the plan gate (status approved+ or a recorded plan approval) title changes and content rewrites are approver-only (appends keeping the content as a prefix stay open — the worker's verdict/amendment/enrichment sections) — every workflow-item edit is audited (`inbox.edit`); retry answers append inside the transition's transaction; `plan_revisions` lock on approve (structural edits then 409 `ErrRevisionLocked` until an amendment); approvals name immutable subjects (`<revision>+<base_sha>` / `<head_sha>`). One-time cutover: `skopos inbox migrate-workflow`.
- Multi-agent stages (opt-in; tmp/multi-agent-stages — design.md): `--run-stage ws:review=focus@runner,…` (review/qa) opens a **group** (`run_groups`, STRICT + CHECKs, Unix-ms times; partial unique index = one open group per item+stage). Members are pinned to their own runner (`claimable`); `chain()` hands a member's result to `groupMemberDone`, which decides once (`DecideGroup` is the open→decided transaction) per policy (any-blocks default / majority / all-pass, early-exit, timeout via `SweepGroups`), writes one summary section (`inbox.AppendSection`) and moves the item. Focus briefs `.skopos/focus/<name>.md` are read at the approved base; review findings carry severity (blocking/nit) and the worker enforces fail ⇔ blocking. One runner per stage = unchanged behaviour. Plan groups (`ws:plan=a,b`): the first claimed member's `ensurePlan` hook creates+links the plan; members call `pin-base` then `POST /api/plans/{id}/candidates` (revision with `group_id`/`label`/`candidate_state`, step ids pre-assigned, live items untouched); `ApproveCandidate` materialises the candidate with ITS step ids (hash re-verified), locks it, supersedes siblings — one tx; `LatestRevision` skips superseded candidates; only the newest round's open candidates are approvable. Request-changes with `revision_id` = one seeded single plan run (`QueueSeeded`, pinned to the candidate's runner); without = round N+1. Advisory stages `compare` (judge → `## Comparison`) and `synthesize` (one more candidate, label `synthesis`) run after a plan round with ≥2 candidates when configured (`afterAdvisory`: candidates → compare → synthesize → gate); single runner each, never grouped, and a failure still opens the gate. Per-item switch: `inbox_items.agents` (`panel`/`solo`, NULL = workspace default `--agents-default`, panel when unset) is read by `runs.Queue` via the `SetItemAgents` callback at every queue — solo = one run on the group's first runner, no focus; approver-only + audited on workflow items.
- Run queue (lean 1b): a run is one stage (plan/implement/review/qa) of one item, claimed by one worker in a single transaction. Affinity is first-class: an interrupted run re-claims to its owner (its session lives there); takeover only via `resume_kind: branch`. Routing is per-workspace stage preference — no tag engine — stored in `internal/pipeline` (`pipeline_stages` / `pipeline_defaults`, STRICT; owner = workspace or group; per stage workspace beats group; group-save refuses a second group setting the same stage, or the agents default, for a shared workspace; workspace delete drops its rows via `workspaces.SetDeleteNotifier`) and read through `runs.SetPrefsSource` at every queue/claim (10s cache, dropped on writes). Edits: approver-in-scope for workspaces, root for groups, audited (`workspace_pipeline`). The old `--run-stage`/`--stage-policy`/`--agents-default` flags are imported once at startup (`Service.Import`) and are deprecated. Chaining walks the item through the workflow matrix on success; failure blocks with the reason. Placement (accounts, quota) is worker-side policy, never skopos-core. Worker registry (`workers`, upserted on claim): rows belong to the key that first wrote them (`key_id`, '' = root, NULL = legacy, adopted by the next writer); `ListWorkers` cuts rows and workspace lists to the caller's scope. Presence: every claim poll upserts the row (idle polls throttled to one write per 30s unless something changed) with `version`/`poll_ms`; heartbeats touch `last_seen`; `POST /api/workers/{id}/offline` is the worker's goodbye; state = busy / idle (seen within max(3×poll, 2m)) / offline.
- `skopos worker`: deployment-agnostic execution daemon (systemd/container/Nomad — needs only skopos over HTTP, a local clone per repo, agent CLIs). Account pool picks a credential at RUN START only (sessions are account-bound); quota deaths mark the account until reset and pause the run with affinity. Agent processes run scrubbed (no SSH agent/tokens, SSH git disabled, deny rules) and locked down (`--setting-sources "" --restricted --strict-mcp-config`: no user/project settings or CLAUDE.md, file tools confined to the worktree, MCP only from `mcp_config`). The push path is the only push: validated refspec, env-only token, never force.
- Runners (internal/worker/runners.go): agent-CLI agnostic. `claude` and `opencode` adapters have live-verified lockdowns (TestLiveLockdown / TestLiveOpencodeLockdown, opt-in env vars); `kind = "command"` (any CLI, argv template) is unverified and only runs inside a wrapper. Non-native structured output: schema in the prompt, final JSON extracted + validated. Every agent command gets `PWD=<run dir>` (opencode trusts $PWD — it once committed in the wrong repo). Review worktrees reset instruction/agent-config paths to base before the run.
- Capability profiles (decision: sandboxing is NOT a skopos feature — `tmp/capability-profiles — design.md`): `worker.profiles.<name>.<stage>` lets the operator contain a stage (`wrapper` argv prefix, or `settings` with `sandbox.enabled`); capabilities (`tools`, `env`, settings allow) only on a contained stage — fail closed at config load. Runs record `executor` (`local` / `wrapper:<hash>` / `sandbox:<hash>`). Knowledge: AGENTS.md/CLAUDE.md and `.skopos/skills/<name>/SKILL.md` are read with `git show <base>:…` (never the worktree head); the plan's selected skills (planner picks ∪ item-tag hints, resolved once at plan time) are stored on the plan + revision and covered by `content_hash` (unchanged when none).

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
