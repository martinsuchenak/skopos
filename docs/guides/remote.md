# Guide: Remote (central server) workflow

One skopos server holds the index for a team (or for all your machines).
Agents and people query the same central index.

## 1. Run the server (once, somewhere shared)

```sh
skopos serve --api-key mysecret
# binds 127.0.0.1:8080 by default; expose it with --server-host 0.0.0.0
# (a key is mandatory for non-loopback binds)
```

Optional: enable semantic search (see
[the concepts guide](../concepts/code-index.md#semantic-search-optional-off-by-default)):

```sh
skopos serve --api-key mysecret \
  --embeddings-url http://localhost:11434/v1 \
  --embeddings-model nomic-embed-text \
  --vector-store qdrant --qdrant-url https://qdrant.example.com   # monorepo scale
```

## 2. Connect a machine (per workstation)

From any repo checkout:

```sh
skopos setup
```

Choose **2) Remote**. The wizard asks for the server URL and API key, tests
the connection, saves them to `skopos-config.toml`, and offers to push this
repo's index. After that, every index/query command talks to the server
automatically — flags and `SKOPOS_SERVER_URL`/`SKOPOS_API_KEY` still override.

Manual equivalent (nothing hidden):

```sh
skopos index push --server-url https://skopos.internal --api-key mysecret
```

## 3. Getting indexes in

**Push from any checkout** (captures your working copy, including uncommitted
changes — usually what you want while developing):

```sh
cd ~/code/myproject
skopos index push            # workspace = git remote, branch = current branch
skopos index push            # again: uploads nothing new (content dedup)
```

**Or let the server index the repo itself** (committed state; good for the
default branch):

```sh
curl -X POST $SKOPOS/api/workspaces \
  -H 'Authorization: Bearer mysecret' \
  -d '{"id":"github.com/me/myproject","git_url":"git@github.com:me/myproject.git"}'

skopos index refresh --workspace github.com/me/myproject --wait
```

The server clones/pulls using its own git credentials. Only one build per
workspace runs at a time; `GET /api/codeindex/{ws}/refresh` reports progress.

## 4. Queries

Same commands as local — they route to the configured server:

```sh
skopos search send email          # keyword; add --semantic for vector search
skopos who-calls Handler
skopos impact Handler --depth 5
skopos branch-diff feat/search
skopos index status               # all indexed branches + freshness
```

Branch semantics: queries take `--branch`; an unindexed branch answers from
the default branch **with a fallback note** in the output.

## 5. Agents

Wire MCP clients (Claude Code, Codex, Gemini, …) to the server:

```sh
skopos install --url https://skopos.internal/mcp --api-key mysecret
```

Agents get the `code_*` tools: `code_search`, `code_symbol`, `code_outline`,
`code_callers`, `code_callees`, `code_impact`, `code_call_tree`, `code_dead`,
`code_cycles`, `code_branch_diff`, `code_index_status` — plus the blackboard,
plans, and status tools.

For Claude Code, `skopos install` also installs a hook suite that makes agents
actually use skopos (opt out with `--no-hooks`):

- **SessionStart** — project briefing: indexed branches, branch blackboard
  entries, active plans, and the tool mandate
- **UserPromptSubmit** — keyword extraction from the prompt; for
  exploration-shaped questions, pre-fetches matching symbols from the index
  (3-minute dedup cache) and injects them as context; every 10 turns a
  checkpoint reminds the agent to record findings to the blackboard
- **PreToolUse (Grep/Agent/Bash)** — nudges symbol-shaped greps and Explore
  dispatches toward `code_*` tools; literal-string and exhaustive greps are
  left alone
- **PostToolUse (Edit/Write)** — after source edits, a throttled (5 min)
  reminder to record the decision on the blackboard
- **Stop** — end-of-session reminder to extract durable facts (decisions,
  rejected approaches, contracts, conventions, bugs/debt) and archive plans

The behavioral prompt (CLAUDE.md block) carries the standing rules: skopos
tools before grep for code structure, proactive blackboard writes during the
session, status reporting cadence.

## 6. Housekeeping

```sh
skopos index drop --branch feat/merged   # remove a merged branch's index
skopos index drop-workspace              # remove a workspace's whole index
                                         # (including external vector collections)
skopos index export --out bundle.ndjson   # portable snapshot from the server
```

## Scripted / agent use

Every query command (and `skopos index status`) accepts `--json`, emitting
the exact shape served by the REST API and MCP tools — same fields, no
parsing ambiguity between surfaces:

```sh
skopos impact Handler --json | jq '.affected[].path'
```

## Notes

- Everything is behind the API key when one is set: REST, MCP, SSE, and the
  index endpoints alike.
- Index data lives in per-workspace SQLite files under the server's
  `--index-dir` (default `.skopos/indexes/`), separate from `skopos.db`.
- Push vs refresh: push sends your **working copy** from any machine; refresh
  builds the **committed state** on the server. Both can coexist — the last
  write per branch wins and the source is recorded in `skopos index status`.
