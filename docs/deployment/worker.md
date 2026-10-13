# Deploying a worker

A worker is the execution plane: it polls skopos for run stages
(plan/implement/review/qa) and executes them with an agent CLI against a local
clone. It is the same binary as the server (`skopos worker`) and needs only:

- skopos over HTTP (URL + a worker-scoped API key)
- a local clone per served workspace
- the agent CLI(s) on the host (`claude`, ...)

Run one worker per VM, one key per worker. On the skopos side, route stages
per workspace with `--run-stage <workspace>:<stage>=<runner>`; the claim only
hands a worker the runs whose preferred runner it offers.

## Quick start

```bash
# 1. Scaffold the config (annotated [worker] section + skopos-worker.env)
skopos worker init --server-url https://skopos.example.com \
                   --workspace github.com/you/repo

# 2. Mint a worker key (root key, against the server)
skopos key create --name worker-vm1 --workspace github.com/you/repo \
                  --server-url https://skopos.example.com --api-key <root-key>

# 3. Fill in the config (repos, accounts) and the secrets file
#    - skopos-config.toml: clone paths, push remote, account env
#    - skopos-worker.env: SKOPOS_API_KEY and the push token_env values

# 4. Validate the config (what install checks before starting)
skopos worker check

# 5. Install and start the service (launchd on macOS, systemd on Linux)
#    A config that fails the checks gets the unit written but NOT started.
skopos worker install

# 6. Watch the first item: queue one and follow
#    queued -> planning -> awaiting_approval (you approve) -> implementing -> in_review
```

## Service management

`skopos worker install` writes the platform service and starts it:

- **macOS**: a LaunchAgent at `~/Library/LaunchAgents/skopos-worker.plist`
  (`KeepAlive`, logs in `<state-dir>/service.log`). Restart with
  `launchctl kickstart -k gui/$(id -u)/skopos-worker`.
- **Linux**: a systemd **user** unit at
  `~/.config/systemd/user/skopos-worker.service` (no root), with linger
  enabled so it survives logout. `--system` installs a system unit under
  `/etc/systemd/system` instead (requires root, `multi-user.target`).
  Restart with `systemctl --user restart skopos-worker`; logs via
  `journalctl --user -u skopos-worker`.

Flags: `--label <name>` (service name), `--dry-run`, `--no-start`.
`skopos worker uninstall` stops and removes the service; the config, env file
and state dir are kept.

Re-running `install` overwrites the service definition (it is derived from the
config) and restarts — safe.

## Secrets

No secrets live in the service definition. The daemon loads
`skopos-worker.env` (next to the config) before parsing the config:

- `KEY=VALUE` lines, `#` comments, values never quoted-parsed beyond `=`
- the real environment always wins over the file
- put `SKOPOS_API_KEY` and each repo's `push.token_env` value there

The TOML holds no token values by design; `token_env` names the variable.

## macOS note: Local Network permission

macOS gates processes that connect to LAN-private addresses, and
launchd-spawned processes cannot show the consent prompt. A worker against a
public HTTPS endpoint is unaffected. If your skopos server resolves to a LAN
address (a split-horizon domain counts), know the hard-won lessons from the
agent-trial predecessor:

- the permission attaches to the exact binary, so **every upgrade resets it**
  (each Homebrew Cask version is a new file) — the service will silently fail
  to connect after `brew upgrade` until the permission is re-granted in
  System Settings → Privacy & Security → Local Network;
- a bare CLI binary sometimes never gets the grant at all — agent-trial ended
  up with a small stable **launcher** binary (embedded Info.plist, ad-hoc
  codesigned, never rebuilt) that holds the permission and execs the real
  binary; that pattern applies here too if you hit it.

The robust setups, in order: run workers on Linux VMs against the public
endpoint; front the server with a public endpoint; or use the launcher
pattern for a Mac worker against a LAN server.

## Unattended operation

Before leaving a worker unattended, run one item supervised end to end and
check:

- the run rows (`GET /api/runs`) show `succeeded`, not repeated `interrupted`
- quota pauses recover via `resume_after` (the run flips back to claimed on
  the owner's next poll after the reset)
- pushes land on the intended remote with the configured `branch_prefix`

Worker ids must start with `worker-` — the config refuses anything else. The
prefix marks worker-created plans so the server's orphan cleanup (empty,
unlinked, a day old) never deletes a plan a human made.
