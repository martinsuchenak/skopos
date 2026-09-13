# Changelog

All notable changes to this project are documented here.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions follow [Semantic Versioning](https://semver.org/).

## [0.1.0] — 2026-09-13

The code-index release: skopos grows from agent coordination into a central,
branch-aware code intelligence server. First pre-1.0 **breaking** release.

### Breaking

- **`skopos init` is removed** — `skopos setup` covers all workflows
  (local index, remote client, run-server).
- **A configured API key now gates every endpoint** — reads, writes, MCP,
  SSE, and metrics. Previously read endpoints were open. Auth-less servers
  remain fully open but bind to loopback only.
- **Default bind is `127.0.0.1`** (was `0.0.0.0`). Exposing the server
  requires `--server-host 0.0.0.0` plus an API key, or the explicit
  `--insecure-no-api-key`.
- **Server-side refresh restricts `git_url`**: `file://`, absolute paths,
  `~`-prefixed, and `..`-traversing values are rejected (SSRF and
  arbitrary-local-repository read were possible before).
- Symbol search queries are capped at 256 bytes (FTS CPU abuse surface).

### Added — code index

- Central, per-workspace, per-branch symbol index over 206 embedded
  tree-sitter grammars; 15 language profiles with per-language extraction.
- Symbol cards carry **doc comments** (PHPDoc, JSDoc, Godoc, docstrings,
  Javadoc, C# XML — summary + `@throws`/`@deprecated`/`@see`; declaration
  data always wins over stale doc tags), **modifiers** (visibility, static,
  abstract, …), and **attributes** (PHP 8 `#[…]`, decorators, annotations).
- Call graph with labeled edges: calls, `new` instantiations, type
  **references** (params/returns/properties/`instanceof`/`catch`),
  inheritance (**extends/implements**, trait use, Go embedding, Ruby
  include, Rust `impl Trait for Type`), and file **imports**.
- Fields, properties, constants, and enum members are symbols.
- Qualified names (`Class::method`); `impact` reaches subclasses and lists
  **overrides**; `dead-code` respects usage kinds.
- Indexing modes: local build, incremental **push** to a central server
  (content-addressed, manifest-negotiated), and **server-side refresh**
  (clone/pull by the server, optional `--refresh-interval` polling).
- Optional **semantic search**: any OpenAI-compatible embeddings endpoint
  (local Ollama works), RRF fusion with `matched_by` provenance
  (`fts`/`vector`/`both`), vector storage embedded in SQLite or external
  **Qdrant**; embeddings are versioned per model+policy and rebuilt
  automatically on change.
- Query surfaces: CLI (`search`, `symbol`, `outline`, `who-calls`,
  `call-tree`, `impact`, `dead-code`, `cycles`, `branch-diff`, `deps` — all
  `--json`), 26 MCP tools, REST under `/api/codeindex/…` (OpenAPI spec
  kept in lockstep by a guard test).
- Path-prefix scoping (`--path` / `path` param) on search, who-calls,
  callees, dead-code, and dependencies; visibility/attributes searchable
  as text; `bm25` column weights so names outrank doc mentions.
- Embedding coverage and last-pass error in `index status`; daily index
  GC of unreferenced blobs/vectors; `skopos index cache-clean`.

### Added — integrations & UX

- `skopos install` for **seven agents** (Claude Code, Codex, Gemini CLI,
  GitHub Copilot, Kiro, opencode, ZCode) with each agent's documented
  config schema, a managed behavioral block in its instructions file, and
  hook suites for Claude Code and ZCode (session briefing, prompt-time
  code pre-fetch, search nudges, memory reminders — mode-aware via
  `skopos mode`).
- `/skopos` exploration skill; agent-builder artifact set.
- `skopos setup` wizard; real shell completion (bash/zsh/fish/PowerShell);
  clean terminal errors; dashboard **Index** view; docs overhaul
  (local/remote guides, configuration reference, per-agent integrations).

### Fixed

- Warm parse cache uploaded **empty indexes** to fresh servers after
  `drop-workspace` or a server switch (major data loss).
- Background embedding failures were silent; passes now log and surface
  in status.
- Semantic ranking pooled RRF credit across duplicate definitions (CSS
  selectors outranked true matches).
- Embedding text now includes split identifier tokens and doc summaries
  (camelCase names are opaque to embedding models).
- MCP `code_search` ignored its `semantic` parameter.
- Per-language profile definitions leaked across languages via a shared
  map alias.
- Rune-unsafe truncation could store invalid UTF-8.

### Security

- Two external penetration-test rounds remediated: index-directory
  traversal via crafted workspace/git URLs, SSRF and local-repo read via
  server-side clone, FTS5 CPU denial-of-service, file-descriptor
  exhaustion (index handles now LRU-bounded), LIKE-wildcard injection in
  blackboard search. Constant-time key comparison, browser-origin
  protection on `/mcp`, Host-header validation for loopback.

## [0.0.11] — 2026-09-09

Dependency refresh, auth improvements, documentation pass.

## [0.0.10] and earlier

See `git log v0.0.9..v0.0.11` — agent status, blackboard, plans,
workspaces, SSE dashboard, MCP server, Docker/Nomad deployment.
