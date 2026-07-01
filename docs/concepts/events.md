# Events (SSE)

The dashboard receives real-time updates via Server-Sent Events, eliminating the need for polling.

## Endpoint

```
GET /api/events/stream
Content-Type: text/event-stream
```

Open (read-only, no auth required — same as other GET endpoints). The response is a persistent stream. The server sends a `: connected` comment on open, then named events as mutations occur, with `: ping` keep-alives every 15 seconds.

## Event types

| Event | Triggered by | UI action |
|-------|-------------|-----------|
| `sessions` | `POST /api/reports`, `DELETE /api/sessions/{id}` | Full refresh |
| `blackboard` | Blackboard write/promote/delete | Re-fetch bundle (if on blackboard view) |
| `plans` | Plan/item create/update/delete/dependency | Re-fetch plans (if on plans view) |
| `workspaces` | Workspace create/delete | Re-fetch workspaces list |
| `change` | Any other mutation | Full refresh |

Each event is sent as:
```
event: plans
data: {"type":"plans"}
```

## Publishing model

An events middleware wraps the entire HTTP mux. On every successful mutation (POST/PATCH/PUT/DELETE with a 2xx response), it infers the event type from the request path and publishes it to the in-process hub. The hub fans out to all connected SSE clients (non-blocking — slow clients are dropped, not wedged).

## Client behavior

The dashboard uses `EventSource`:
1. On connect (`onopen`): stop polling (if active), full refresh to sync state.
2. On event: targeted re-fetch (e.g. `blackboard` event → `fetchBundle()`).
3. On error (`onerror`): close the EventSource (prevents browser auto-reconnect spam), start 5s polling as fallback, schedule SSE retry with exponential backoff (2s → 4s → 8s → … → 60s max).
4. On reconnect: stop polling, reset backoff, full refresh.

A connection status dot in the sidebar shows green (connected) or amber/pulsing (reconnecting).

## Per-instance limitation

The hub is in-process (single instance). For multi-instance deployments, a shared pub/sub bus (e.g. Redis/Valkey) would be needed to fan events across instances. This is not implemented — skopos is designed for single-instance SQLite deployments.
