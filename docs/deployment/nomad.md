# Nomad

The repo's [`skopos.nomad`](../../skopos.nomad) runs skopos as a service with
its SQLite database on the allocation's local disk, and
[Litestream](https://litestream.io) streaming every commit to a shared volume.

## Keep SQLite off shared storage

Don't put `skopos.db` on NFS, SMB or another network filesystem:

- **It's slow.** Every commit becomes a network lock and flush. A request that
  writes three times takes 15 seconds instead of milliseconds, and every other
  request queues behind it.
- **It can corrupt.** SQLite's WAL mode relies on shared memory (the `-shm`
  file) and file locks that network filesystems don't reliably provide.

skopos logs a warning at startup when the database or the code index directory
is on a network filesystem.

A Nomad job can't count on any one node's disk, so the job spec keeps the
database on the allocation's own disk and replicates it:

- **`ephemeral_disk`** (`sticky`, `migrate`) holds `/alloc/data` on the node's
  local disk. Nomad keeps it when the allocation is replaced on the same node,
  and copies it when the job moves to another node.
- **Litestream** (a `poststart` sidecar) streams every commit to the shared
  volume, about a second behind. It writes plain files there, so the shared
  volume's locking doesn't matter.
- **The `restore` task** (`prestart`) runs before skopos. If the local disk
  still has the database, it's used as is. Otherwise Litestream restores it
  from the replica. That's the case when the node died and `migrate` had
  nothing to copy.

At most about a second of writes is lost if a node fails outright.

## The job spec

[`skopos.nomad`](../../skopos.nomad) has three tasks in one group:

| Task | Lifecycle | Does |
|---|---|---|
| `restore` | prestart | keeps the local database, or restores it from the replica |
| `skopos` | main (leader) | `DATABASE_PATH=/alloc/data/skopos.db`, `SKOPOS_INDEX_DIR=/alloc/data/indexes` |
| `litestream` | poststart sidecar | `litestream replicate` to `/replica/litestream/skopos` |

Adjust before running:

- **The shared volume.** The `volume "skopos-replica"` block is a CSI example.
  Use however your cluster mounts shared storage today. Only the `restore` and
  `litestream` tasks mount it, at `/replica`.
- **The user.** The Litestream tasks run as `1000:1000`, the skopos image's
  user, so the database files they restore are writable by skopos. The shared
  volume must be writable by that uid.
- **The disk size.** `ephemeral_disk.size` covers the database plus the code
  indexes.
- **The image and secrets.** Set `image`, and put `SKOPOS_API_KEY` in the
  `skopos` task's `env` (or a template from Vault).

Keep `count = 1`: SQLite has one writer.

## Moving from a database on the shared volume

If skopos currently keeps `skopos.db` on the shared volume:

1. **Stop the job.** `nomad job stop skopos` shuts skopos down cleanly, which
   merges the `-wal` file into `skopos.db`.
2. **Mount the same volume** as `skopos-replica`, so the old database is at
   `/replica/skopos.db` in the Litestream tasks.
3. **Run the new job.** With no replica yet, `restore` seeds the local disk from
   `/replica/skopos.db` (and its `-wal`, if one is left), then renames the old
   files to `*.migrated` so they're never used again. Litestream then writes
   the first full copy to `/replica/litestream/skopos`.
4. **Check the logs:** `restore` says `no replica yet: seeding from …`, and
   `litestream` reports the replica. From then on, `restore` says
   `local database kept` or `restored from the replica`.

Delete the `*.migrated` files once you're satisfied.

## Code indexes

The per-workspace code index databases (`SKOPOS_INDEX_DIR`) are derived data,
so they stay on local disk and aren't replicated. When an allocation lands on
a node without them, run `skopos index push` in each repository to rebuild.

## Restoring by hand

To inspect the replica, or restore it somewhere else:

```bash
litestream snapshots -config litestream.yml /alloc/data/skopos.db
```

```bash
litestream restore -config litestream.yml -o ./skopos.db /alloc/data/skopos.db
```

## Deploy

```bash
nomad job run skopos.nomad
```

## Notes

- Only port 8080 is exposed (HTTP, MCP at `/mcp`, SSE at `/api/events/stream`).
- The health check hits `/health` every 10s.
