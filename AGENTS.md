# AGENTS

## Project

- Name: `skopos`
- Module: `github.com/martinsuchenak/skopos`

## Generated Structure

- CLI is always enabled.
- API feature is enabled.
- MCP feature is enabled.
- UI feature is enabled under `web/`.
- DB feature is enabled with `sqlite` and `xdal`.
- Cache feature is enabled with `valkey`.
- Docker support is enabled.
- Nomad support is enabled.

## Working Rules

- Prefer `task build`, `task test`, and `task lint` before custom commands.
- Keep generated layering intact: handler -> service -> storage for API code.
- Add new CLI commands, API endpoints, or MCP tools with `go-scaffolder add` where possible.
- Do not remove marker comments beginning with `go-scaffolder:` unless you also update the patching flow.

## Common Commands

```sh
task build
task test
task lint
```
Frontend build:

```sh
task frontend-build
```
