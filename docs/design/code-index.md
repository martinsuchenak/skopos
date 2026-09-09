# Design: Code Index for skopos

Status: proposed (plan agreed in principle; implementation not started)

## Goal

Give skopos a central, queryable **code index** — symbols, references, and call
graphs for indexed repositories — exposed over MCP tools, REST, and CLI, so AI
agents explore code structure instead of grepping raw files. The index is
**central**: skopos is the shared knowledge server, so one index serves every
machine and every agent working on a project.

## Non-goals (v1)

- Type-accurate cross-references (would require a per-language compiler
  toolchain in the server; the indexer resolves references by name only).
- Code embeddings are **optional** and off by default (see below).
- Live file watching on the server for arbitrary directories.

## Spikes done (evidence)

- `modernc.org/sqlite` supports **FTS5** (prefix match, porter, rank) — no CGO.
- `github.com/odvcencio/gotreesitter` v0.52 (pure-Go tree-sitter runtime, MIT,
  206 grammars, oracle-gated against tree-sitter v0.25.1):
  - Symfony corpus: **11,457 PHP files / 2.1M LOC → 36s single-threaded,
    0 hard failures, 68k symbols, ~584MB peak RSS**.
  - One pathological file (11s) is capped cleanly by `Parser.SetTimeoutMicros`.
  - Binary size: all grammars embedded ≈ **+17MB** (skopos 15MB → ~32MB).
    Accepted: single-binary simplicity beats size. Subset/external-blob build
    tags remain available if this ever needs revisiting.

## Concepts

- **Workspace** — the existing skopos workspace is the top-level scope.
  Gains optional fields: `path` (local checkout, for server-side indexing on
  the same host) and `git_url` (for server-side clone/pull indexing).
- **Branch** — the index is keyed `(workspace, branch)`. Branch is a
  first-class dimension, mirroring the blackboard's branch scope.
- **Content-addressed symbols** — symbol/edge rows are keyed by **file content
  hash**, not path. Unchanged files are shared across branches for free;
  incremental pushes and new branches only upload genuinely changed files
  (manifest negotiation: client sends hashes, server requests the missing
  ones — git's pack-negotiation trick applied to symbols).
- **Index state** — one per `(workspace, branch)`: HEAD SHA at build time,
  timestamp, source (`local-push:<host>` or `server-build`). Every query
  response carries this metadata so agents know exactly what they are seeing.
  Last writer wins per path; content-addressing makes concurrent pushes
  converge.

### Branch fallback semantics

| Query target | Behavior |
|---|---|
| Indexed branch | Exact answers |
| Never-pushed branch | Fall back to the workspace's default branch, **labeled** in the response (`"index": "main@a1b2c3 — branch feat/x not indexed"`) |
| Same branch, two pushers | Converges (content addressing); per-path last-writer-wins |
| After rebase/merge | Changed files get new hashes → only those re-parsed/re-uploaded |

## Language support

- **All 206 grammars** embedded (build-tag subsets or external blobs remain
  available as escape hatches).
- Language detected by file extension; unknown extensions skipped;
  `.gitignore`-style exclusion for `node_modules`, `vendor` (opt-in),
  `dist`, `bin`, etc.
- **Multi-language parsing**: (a) polyglot repos handled per-file;
  (b) mixed-language files (PHP+HTML templates, Vue SFCs, HTML+JS+CSS,
  Markdown code fences) via gotreesitter's `InjectionParser`.
- Per-file parse budget (default 2s) using the timeout API; files that exceed
  it are indexed via error recovery and flagged.

## Storage: alternatives considered

Hard constraints: **pure Go / no CGO** (cross-compile `CGO_ENABLED=0`),
**embedded** (no separate server process), **single binary** philosophy.
Everything else was evaluated against those:

| Option | Pure Go | Full-text | Vectors | SQL/graph | New deps | Verdict |
|---|---|---|---|---|---|---|
| **SQLite (modernc), per-workspace files** | ✓ | FTS5 (measured working) | BLOB brute-force (measured, ceiling documented) | recursive CTEs | **0 — already in the stack** | **chosen** |
| Bleve + SQLite state | text ✓, vectors ✗ | best-in-class (BM25, custom analyzers) | ✗ (FAISS = CGO, even in v2.5+) | app-side | +1 engine | Rejected — better text search, but a second engine to keep consistent, and vectors remain unsolved |
| bbolt / pebble / badger (KV) | ✓ | ✗ hand-build | hand-build | hand-build | +1 | Rejected — rebuilding FTS5 and query layers in app code for zero net gain at our write volumes |
| chromem-go | ✓ | ✗ | brute force (same algorithm), in-memory + gob persistence, <100k sweet spot, beta | ✗ | +1 | Rejected — identical brute-force math to our SQLite BLOB approach, but weaker persistence, no SQL, no FTS |
| External servers (Qdrant, Meilisearch, …) | n/a | ✓ | ✓ ANN | ✓ | server process | Rejected — violates single-binary, zero-dependency deployment |
| CGO family (mattn/go-sqlite3 + sqlite-vec, LanceDB-go, DuckDB+VSS, go-libsql, Bleve+FAISS) | ✗ | ✓ | ✓ ANN | ✓ | native toolchains | Rejected — breaks `CGO_ENABLED=0` cross-compilation and the goreleaser pipeline (4 platforms + windows) |
| coder/hnsw | ✓ | n/a | **pure-Go HNSW ANN** (used in production by Coder) | n/a | +1 later | Designated growth path: swap in behind `VectorStore` if a workspace outgrows ~300k vectors |

Note: the similarly named `Bithack/go-hnsw` (top GitHub search result for
"go-hnsw") is unmaintained since **2017** and ships **no license** — do not
confuse it with the option above. The pure-Go HNSW landscape is otherwise
sparse: casibase/go-hnsw (6★, 2023), dmarro89/hnsw-go (3★, hobby),
fogfish/hnsw (27★, stale 2024) — coder/hnsw (237★, active, CC0) is the only
one with production backing.

Notable FTS5 detail discovered during research: modernc SQLite cannot load
custom C tokenizers, and the built-in `unicode61` tokenizer does not split
`camelCase`/`snake_case` identifiers. Mitigation (write-time, we control the
writer): store a `name_parts` column with the identifier split into subtokens
(`AuthorizationError` → `authorization error`), or use FTS5's built-in
`trigram` tokenizer for substring matching. This is preprocessing, not an
engine limitation.

### Rationale

- **Zero new storage dependencies** for the default (no-embeddings) build —
  SQLite is already shipped and its FTS5 was measured working.
- Every pure-Go vector option is brute force anyway (chromem-go's own docs
  say so; sqlite-vec is brute-force too even where it's usable) — so our
  measured BLOB+cosine numbers are the same math with better surrounding
  infrastructure (transactions, SQL, FTS, one file per workspace).
- The only real capability gap versus the CGO world is ANN at 1M+ vectors;
  `coder/hnsw` covers exactly that gap, purely in Go, behind the
  `VectorStore` interface — adopt it only when a real workspace needs it.

## Architecture

New package `internal/codeindex` following the house pattern
(`handler → service → storage`), plus `cmd/index.go` (CLI) and `cmd/mcp/code_*.go`
(tools). Parser runs behind `internal/codeindex/parse` so the pure-Go
dependency is isolated to one leaf package.

### Storage: a separate index DB per workspace

Index data lives in `indexes/<workspace-slug>.db`, **not** in `skopos.db`:

- Keeps the coordination DB small (backup/cleanup/retention story unaffected);
  a workspace index can be dropped by deleting a file or rebuilt without ever
  touching session/blackboard/plan data.
- Separate SQLite write locks per workspace — index pushes don't contend with
  live coordination writes.
- Multiple `*sql.DB` pools on modernc SQLite are fine; same DSN pragmas.

### Data model (per-workspace index DB)

```
index_files    (branch, path, content_hash, lang, mtime, err_flag)
index_symbols  (content_hash, name, kind, line, start_byte, end_byte, signature, lang)
index_edges    (content_hash, caller_symbol, callee_name, kind, line)   -- call/reference heuristics
index_state    (branch, head_sha, built_at, source, symbol_count, file_count)
index_blobs    (content_hash, payload)          -- uploaded symbol+edge payloads, dedup
FTS5 virtual table over index_symbols (name, signature, kind, path) with porter+unicode61
```

Branch deletion: TTL cleanup in the existing cleanup worker, plus explicit
`skopos index drop --branch`.

### Search layers

1. **FTS5** — prefix/stemmed text search over symbol names and signatures.
2. **Graph** — callers/callees/impact traversal over `index_edges`.
3. **Semantic embeddings — optional, disabled by default.**
   - Providers: any OpenAI-compatible endpoint (covers **Ollama / LM Studio
     for fully-local embeddings without CGO**, plus OpenAI, Voyage, etc.).
     Config under `[codeindex.embeddings]` (provider, base_url, model, api_key).
     No local ONNX in-process (would need CGO); local-first is satisfied via
     the OpenAI-compatible local servers.
   - **Why SQLite still works here:** modernc SQLite cannot load extensions,
     so there is no ANN index (sqlite-vec etc.) — vectors are brute-force
     cosine. That is acceptable because we embed **symbols**, not document
     chunks: a 2.1M-LOC repo measures ~68k symbols, and the RRF fusion does
     not need exact top-k from the vector leg. Measured (unoptimized scalar
     Go, top-10 over 768d): 68k → 122ms/199MB, 150k → 210ms/439MB,
     300k → 423ms/879MB, 1M → 1.4s/2.9GB. int8 quantization: ~15% faster,
     4× smaller, recall loss immaterial under fusion.
   - Vectors stored as BLOBs in the workspace index DB, cached in memory per
     workspace (normalized float32; optional int8 mode).
   - **Guardrails:** embeddings sit behind a `VectorStore` interface. The
     brute-force SQLite implementation is the default; queries restrict to
     the workspace (and optionally the FTS/graph candidate neighborhood);
     int8 is a config switch. Documented ceiling: comfortable to ~300k
     vectors per workspace; beyond that, swap in `coder/hnsw` (pure-Go HNSW)
     as another `VectorStore` implementation without touching the index core.
   - **RRF fusion** (k=60) of FTS + vector + graph expansion.
   - Embedding runs wherever indexing runs (push client or server).

### API (REST, behind the existing API key when set)

```
POST /api/codeindex/manifest        -- negotiate which hashes the server needs
POST /api/codeindex/blobs           -- upload symbol/edge payloads (ndjson)
POST /api/codeindex/commit          -- atomically point (workspace, branch) at a file set
GET  /api/codeindex/status          -- per workspace/branch state
GET  /api/codeindex/search?q=       -- FTS/fused search
GET  /api/codeindex/symbol?name=    -- definition + references
GET  /api/codeindex/outline?path=
GET  /api/codeindex/graph?symbol=&direction=callers|callees&depth=
DELETE /api/codeindex/{workspace}/{branch}
POST /api/codeindex/export / import -- portable bundle
POST /api/codeindex/refresh         -- (server-side mode) clone/pull + reindex a workspace
```

### MCP tools

`code_search`, `code_symbol`, `code_outline`, `code_callers`, `code_callees`,
`code_impact` (transitive), `code_call_tree`, `code_dead` (uncalled symbols),
`code_branch_diff` (symbols changed vs default branch — merge-prep
intelligence unique to the central model), `code_index_status`.
All accept `workspace_id` + optional `branch` (fallback semantics above).
`skopos_context` gains a small structural summary for the caller's branch.

### CLI (manual control)

```
skopos index build   <path> [--branch] [--full]     -- local build
skopos index watch   <path>                          -- debounced rebuild (2s)
skopos index push    <path> --server-url --workspace --branch [--git-head]
skopos index status  [--server-url] [--workspace] [--branch]
skopos index drop    --workspace --branch
skopos index export  [--workspace --branch] -o bundle.ndjson
skopos index import  bundle.ndjson
skopos search  <query>   [--server-url] [--workspace] [--branch] [--semantic]
skopos symbol  <name>    [...same flags]
skopos who-calls <name>  [...]
skopos call-tree <name>  [--depth] [...]
skopos impact   <name>   [...]
skopos dead-code [--workspace] [--branch]
skopos outline  <file>
```

CLI query commands default to a local `skopos.db` when present, else require
`--server-url` — consistent with existing `report`/`blackboard`/`plan`
commands. `skopos install` snippets teach agents to push after checkout;
optional git `post-checkout`/`post-commit` hooks.

## Indexing modes: both local and server-side

- **Local push (primary).** One machine (dev box or CI) builds and pushes the
  working-copy state of a branch. No git credentials on the server; captures
  uncommitted changes; satisfies "not every machine indexes" — one pusher,
  all consumers.
- **Server-side (opt-in).** The skopos host clones/pulls the workspace's
  `git_url` and indexes locally (uses the host's git credentials via the git
  CLI, same as `workspace.Resolve` shells out today). Triggered via
  `skopos index refresh --workspace X` (authenticated REST) or a schedule.
  Sees committed state only; intended mainly for default-branch baselines.
- Precedence: one index state per `(workspace, branch)`; last writer wins
  regardless of source; the state's `source` field says which. (Hybrid
  base+overlay is a future option, not v1.)

## Security

- All index endpoints behind the API key when configured (read and write).
- Embedding API keys stored in `skopos-config.toml` (0600) like `auth.api_key`;
  never returned by any API. Embedding content is sent only to the configured
  provider — with local Ollama that is localhost.
- Server-side cloning only fetches configured URLs; no arbitrary-path reads
  beyond registered workspace paths.

## Phases & estimates

| Phase | Content | Est. |
|---|---|---|
| 0 | Dependency, schema, workspace `path`/`git_url`, language detection | 0.5d |
| 1 | Parser core: extraction queries (incl. PHP et al.), timeouts, incremental hashing, export/import | 1.5d |
| 2 | Storage + REST: content-addressed tables, FTS5, manifest/push/commit, auth | 1.5d |
| 3 | MCP tools (search/symbol/outline/graph/impact/status) + `skopos_context` enrichment | 1d |
| 4 | CLI: index lifecycle + search/symbol/who-calls/call-tree/impact/dead-code/outline | 1d |
| 5 | Server-side indexing (clone/pull worker, refresh API) | 1d |
| 6 | Analysis: dead-code, cycles, call-tree rendering, Mermaid export, branch diff | 1d |
| 7 | Optional embeddings: provider interface, vector store, RRF fusion, config | 1.5d |

Core value (Phases 0–4) ≈ **6.5 days**; full scope ≈ **2 weeks**.

## Risks / open questions

- gotreesitter is a one-maintainer project — pin the version; the parser is
  isolated behind one package so a fallback (CGO bindings or external blobs)
  stays possible.
- Call/reference heuristics per language (incl. `$obj->method()`, `Foo::bar()`,
  dynamic calls) will be imperfect — interfaces, dynamic dispatch, and
  reflection are invisible to name resolution; tools should label confidence,
  not pretend certainty.
- Binary +17MB accepted (32MB total) — revisit via build tags if it hurts.
- FTS ranking quality across languages with different naming conventions —
  tune tokenizers per field if needed.
