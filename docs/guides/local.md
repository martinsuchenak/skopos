# Guide: Local-only workflow

Use skopos code search on one machine with **no server**. Everything — index
and queries — happens inside your repo.

## 1. One-time setup

From your repo root:

```sh
skopos setup
```

Choose **1) Local only**. The wizard detects the workspace ID from your git
remote (or asks), indexes the repo into `.skopos/indexes`, and prints
commands.

Prefer to do it by hand? The whole local setup is one command:

```sh
skopos index build            # or: skopos index build /path/to/repo
```

## 2. Daily use

All query commands run from the repo root and read `.skopos/indexes` automatically:

```sh
skopos search loadconfig        # symbol search; camelCase is split, so
                                # "loadconfig" finds LoadConfig
skopos symbol Handler           # exact-name definitions with file:line
skopos outline app/auth.go      # a file's definitions in source order
skopos who-calls Handler        # call sites of a symbol
skopos call-tree main           # what a symbol calls, recursively
skopos call-tree main --mermaid # same, as a Mermaid diagram
skopos impact Handler           # what breaks if Handler changes
skopos dead-code                # symbols nothing calls (verify before deleting)
skopos cycles                   # cycles in the call graph
```

Options every query accepts: `--workspace <id>` (default: the git remote),
`--branch <name>` (default: the default branch), `--index-dir <dir>`
(default: `.skopos/indexes`).

## 3. Keeping the index fresh

The index is a snapshot. Refresh after pulling, merging, or big edits:

```sh
skopos index build              # incremental: only changed files re-parse
```

Branch work:

```sh
git checkout -b feat/search
skopos index build              # indexes the branch you're on
skopos branch-diff feat/search  # your branch's symbols vs the default branch
skopos index drop --branch feat/search   # clean up after merging
```

## 4. Housekeeping

```sh
skopos index status                     # what's indexed, how fresh
skopos index export --out bundle.ndjson # portable snapshot
skopos index import bundle.ndjson       # restore elsewhere
skopos index drop-workspace             # delete the workspace's whole index
```

`.skopos/indexes` is plain SQLite — copy it or delete it freely. It is
a derived artifact: deleting it never loses source data, just re-run
`skopos index build`.

## Notes

- No server, no API key, nothing listens on any port.
- Agents can still use the index indirectly through you; for agents to query
  it themselves, run a server (see [remote.md](remote.md)) — you can point it
  at the same `indexes/` directory.
- Language support: 206 grammars are built in; detection is by file
  extension, `vendor/`-style directories and files over 1 MiB are skipped.
