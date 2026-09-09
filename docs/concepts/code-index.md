# Code Index

skopos maintains a **central, queryable symbol index** of your repositories:
definitions, files, and heuristic call/reference edges, stored per workspace
and per branch. Agents query it through MCP tools, the REST API, or the CLI —
instead of grepping raw files.

## Getting started

Quick walkthrough with real commands. Everything below works against a
running skopos server (`skopos serve`); CLI examples add `--server-url` /
`--api-key` (or set `SKOPOS_SERVER_URL` / `SKOPOS_API_KEY`).

### 1. Push a repo's index (from any checkout)

```sh
cd ~/code/myproject
skopos index push
# -> pushed 1995 files (1995 uploaded) for github.com/me/myproject@main
# workspace defaults to the git remote, branch to the current branch
```

Repeat pushes upload only what changed:

```sh
skopos index push          # -> pushed 1995 files (0 uploaded)
```

### 2. Query it

```sh
skopos search loadconfig            # symbol search, camelCase-aware
# LoadConfig    func    app/auth.go:42

skopos symbol Handler               # exact-name definitions
skopos outline app/auth.go          # a file's definitions in order
skopos who-calls Handler            # call sites
skopos call-tree main --depth 4     # what main calls, recursively
skopos call-tree main --mermaid     # same, as a Mermaid diagram
skopos impact Handler               # what breaks if Handler changes
```

### 3. Branches

Each branch has its own index state. From a feature branch:

```sh
git checkout -b feat/search
skopos index push                   # only feat/search files upload
skopos branch-diff feat/search      # symbol deltas vs the default branch
skopos index drop --branch feat/search   # clean up when merged
```

Queries on an unindexed branch fall back to the default branch and say so.

### 4. Server-side indexing (no local checkout needed)

Register the workspace with a git URL, then let the server index itself:

```sh
curl -X POST $SKOPOS/api/workspaces   -H 'Authorization: Bearer $SKOPOS_API_KEY'   -d '{"id":"github.com/me/myproject","git_url":"git@github.com:me/myproject.git"}'

skopos index refresh --workspace github.com/me/myproject --wait
skopos index status --workspace github.com/me/myproject
# main   1995 files   6971 symbols  2026-09-09T07:11:59Z  (server-build)
```

### 5. Semantic search (optional)

Enable an OpenAI-compatible embeddings endpoint (local Ollama keeps
everything on-host) and push again — vectors build in the background:

```sh
skopos serve --embeddings-url http://localhost:11434/v1              --embeddings-model nomic-embed-text
skopos index push

curl "$SKOPOS/api/codeindex/github.com%2Fme%2Fmyproject/search?q=send+a+test+email&semantic=true"
```

Keyword search misses that query entirely; semantic returns
`it_sends_a_test_email`, `sendEmailVerificationNotification`, …
For monorepo scale switch the vector backend:
`--vector-store qdrant --qdrant-url https://qdrant.example.com`.

### 6. Agents (MCP)

The same queries are MCP tools — agents connected through `skopos install`
already have them: `code_search`, `code_symbol`, `code_outline`,
`code_callers`, `code_callees`, `code_impact`, `code_call_tree`,
`code_dead`, `code_cycles`, `code_branch_diff`, `code_index_status`.

### 7. Portability and cleanup

```sh
skopos index export --workspace github.com/me/myproject --out bundle.ndjson
skopos index import bundle.ndjson --workspace github.com/me/myproject
skopos index drop-workspace --workspace github.com/me/myproject
```

## Model

- **Workspace-scoped**: each workspace has its own index database (under
  `--index-dir`, default `indexes/`), separate from `skopos.db`.
- **Branch-aware**: every branch has its own index state. Queries accept a
  `branch`; an unindexed branch falls back to the workspace's default branch,
  **labeled as a fallback** in the response — never silently wrong.
- **Content-addressed**: symbols and edges are keyed by file content hash.
  Identical files are shared across branches; incremental pushes upload only
  genuinely changed files (manifest negotiation).
- **Freshness is visible**: every branch records the git HEAD it was built
  from, a timestamp, and the source (`push:<host>` or `server-build`).

## Getting an index in

**Local push** (primary): from any checkout, one machine pushes the branch:

```sh
skopos index push --server-url https://skopos.internal --workspace github.com/org/repo
# workspace defaults to the git remote, branch to the current git branch
```

**Server-side** (opt-in): register the workspace with a `git_url` and ask the
server to clone/pull and index itself:

```sh
curl -X POST $SKOPOS/api/workspaces -d '{"id":"github.com/org/repo","git_url":"git@github.com:org/repo.git"}'
skopos index refresh --workspace github.com/org/repo --server-url ... --wait
```

**Local only**: `skopos index build <path>` writes to a local index directory;
the query commands (`skopos search` etc.) work against it without a server.

**Portability**: `skopos index export -o bundle.ndjson [--branch]` and
`skopos index import bundle.ndjson`.

## Querying

CLI (add `--server-url` for remote, or omit for the local index dir):

```sh
skopos search loadconfig     # FTS over symbols; camelCase is split-tokenized
skopos symbol Handler        # exact-name definitions with file:line
skopos outline pkg/util.go   # a file's definitions in source order
skopos who-calls Handler     # call sites
skopos call-tree main        # recursive callees (--mermaid for a diagram)
skopos impact Handler        # transitive "what breaks if I change this"
skopos dead-code             # unreferenced symbols (heuristic — verify)
skopos cycles                # cycles in the call graph
skopos branch-diff feat/x    # symbols changed vs the default branch
```

MCP tools: `code_search`, `code_symbol`, `code_outline`, `code_callers`,
`code_callees`, `code_impact`, `code_call_tree`, `code_dead`, `code_cycles`,
`code_branch_diff`, `code_index_status` — all take `workspace_id` and an
optional `branch`.

REST: everything under `/api/codeindex/{workspace}/…` (see `openapi.yaml`),
behind the API key when one is configured.

## Analysis notes

Call edges are **name-based heuristics** (tree-sitter is syntactic — no type
resolution). They are right most of the time and honestly wrong sometimes
(dynamic dispatch, interface implementations, reflection). Treat `dead-code`
and `cycles` as leads to verify, not verdicts.

## Semantic search (optional, off by default)

Configure any OpenAI-compatible embeddings endpoint — including a local
Ollama, which keeps everything on your machine:

```toml
[codeindex.embeddings]
url = "http://localhost:11434/v1"
model = "nomic-embed-text"
# api_key = ""   # not needed for local servers
```

Embeddings are computed in the background after each push. `code_search` and
`/search?semantic=true` then fuse full-text and vector results with Reciprocal
Rank Fusion.

Vector storage is pluggable (`VectorStore` interface):

- **`sqlite` (default)** — brute-force cosine over BLOBs in the per-workspace
  index DB. Measured: ~120ms at 68k symbols; comfortable to ~300k per
  workspace. Zero moving parts.
- **`qdrant`** — external vector database over its REST API (works behind any
  HTTP proxy; no gRPC). One collection per workspace (`skopos-<slug>`),
  recreated automatically if the embedding model's dimensions change.
  For monorepo scale (millions of LOC):

```toml
[codeindex.embeddings]
url = "https://llm-router.example.com/v1"
model = "text-embedding-nomic-embed-text-v1.5"
vector_store = "qdrant"
qdrant_url = "https://qdrant.example.com"
# qdrant_api_key = ""
```

`skopos index drop-workspace` tears a workspace's index down completely —
the index DB and vectors in any backend, including the external collection.

## Languages

All 206 tree-sitter grammars are embedded (the binary grows by ~25MB).
Language detection is extension-based (linguist-style); mixed-language files
are handled per language. Unparseable or pathological files are indexed via
error recovery under a 2s per-file budget.
