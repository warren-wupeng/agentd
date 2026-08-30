# M3 — SSE event stream + history replay

Goal: a client can watch a session live and survive its own death.
Done when (roadmap): `kill -9` the client mid-run, reconnect, nothing
lost. Protocol per ADR-003: SSE is a live **tail** of the log; reconnect
= replay + dedupe by event id; NOTIFY is a latency optimization, never
the source of truth. Streaming design per `docs/design/agent-loop.md`:
provider deltas are ephemeral — fanned out to SSE clients, never written
to the log; only the assembled `message.assistant` persists.

## What ships

1. **Store tail plumbing** (ADR-003):
   - `AppendEvent` runs `pg_notify('agentd_events', <session_id>)` inside
     its transaction — the notification fires exactly when the event
     becomes visible (commit), so no wake can race ahead of the read.
   - `store.EventListener` — one dedicated pgx connection per process,
     `LISTEN agentd_events`, `WaitForNotification` loop, per-session demux:
     `Subscribe(sessionID) (<-chan struct{}, cancel)`. Missed
     notifications during listener restart are acceptable BY DESIGN — the
     SSE handler re-queries on every wake and the client protocol
     mandates replay + dedupe on reconnect.

2. **Model streaming** (`internal/model`):
   - `Delta{Type: "text"|"restart", Text}` and the optional capability
     interface `Streamer.Stream(ctx, req, onDelta) (*CompletionResponse, error)`.
     Providers that don't stream keep working through `Complete`.
   - OpenAI provider: `stream: true` + `stream_options.include_usage`,
     SSE chunk parsing including incremental tool_calls fragment
     assembly (id/name/arguments concatenated by index).

3. **Delta fan-out** (`internal/hub`): per-session subscriber channels,
   buffered, drop-on-full — deltas are ephemeral by design; a slow
   client misses tokens but never misses the durable event that follows.

4. **Loop integration**: Step prefers the provider's Stream and forwards
   deltas to the hub; a model retry emits a `restart` delta so live
   clients see the boundary instead of silently duplicated tokens.

5. **SSE endpoint** — `GET /v1/sessions/{id}/stream?after_seq=N`:
   - frames: `event: log` (carries `id: <seq>`, data = full event JSON —
     SSE's built-in Last-Event-ID reconnect semantics), `event: delta`
     (no id — ephemeral), `: keep-alive` comments every 15s.
   - flow: subscribe to hub + listener FIRST, replay durable history
     after after_seq (or the Last-Event-ID header), then select-loop:
     deltas, listener wakes (re-query events), heartbeats, ctx done.
   - without hub/listener wired (CRUD-only process): 409 with
     remediation, same contract as the run endpoint.

## Tests

- store: notify fires on append, listener wakes the right session,
  cancel unsubscribes
- model: OpenAI SSE parser against a canned chunked body — text deltas,
  tool_calls fragments across chunks, usage, [DONE]
- api (G4): `TestReplay_StreamSurvivesClientKill` — stream with a live
  loop turn, abort the client mid-run, reconnect with Last-Event-ID,
  assert the full event history arrives with strict seq order and no
  gaps; deltas seen pre-kill are not re-delivered after reconnect

## Explicitly deferred (re-scoped from M2's notes)

- **`ask` verdict + `escalation.requested` moves to M4**, not M3:
  escalation's killer app is delegating OpenCode's `permission.ask` to
  the agentd policy engine (ADR-004's strongest governance surface).
  Building it right before the adapter that needs it is better
  sequencing than bolting it onto the streaming milestone. The
  stop_reason plumbing (`requires_action`) already exists from M1.
- Polling fallback for gateways that can't hold a PG connection
  (ADR-003's escape hatch) — single-process M3 doesn't need it; the
  listener interface keeps it additive.
- multi-replica tailer service / Redis fan-out (ADR-003's scale note).

## Decision log

- 2026-08-30: notify INSIDE the AppendEvent tx (fires at commit) rather
  than after — a notification that races ahead of visibility wakes the
  reader to see nothing; commit-bound notify cannot.
- 2026-08-30: deltas carry no SSE id and are never re-delivered — the
  durable `message.assistant` event is the truth; a reconnected client
  sees the assembled message exactly once.
- 2026-08-30: escalation re-scoped to M4 (see above).
- 2026-08-30 (found in the live demo): the gateway's streamed tool_calls
  come in a cumulative-final dialect — `function.partial: false` marks
  the closing fragment which REPEATS the complete arguments. Two
  dialects now supported: absent `partial` (OpenAI incremental, append)
  vs present (final fragment replaces). Concatenating the repeated
  value produced invalid JSON that `mustJSON` silently swallowed into
  an empty `{}` event payload — five of them per turn. Fixes: dialect
  handling in the parser, json.Valid on assembled arguments in BOTH
  Stream and Complete (invalid → retryable provider error, not garbage
  events), and the loop marshals the assistant payload explicitly so a
  marshal failure parks loudly. Regression tests pin the gateway's
  exact chunk shapes.
- 2026-08-30: middleware must re-implement http.Flusher — requestLogger's
  statusRecorder hid the interface from the SSE handler and every stream
  died with a 500 before the first frame.

## Progress log

- 2026-08-30: plan created. No code yet.
- 2026-08-30: **M3 done.** Store: pg_notify in the AppendEvent tx +
  EventListener (dedicated conn, per-session demux, reconnect with
  backoff; missed notifications tolerable by design). Model: Delta +
  Streamer + OpenAI.Stream (SSE parsing, dual tool_call dialects,
  argument validation). Hub: per-session fan-out, drop-on-full.
  Loop: streams when the provider can, publishes deltas, restart marker
  on retry. API: GET /v1/sessions/{id}/stream — log frames (id: seq),
  delta frames, keep-alive comments; Last-Event-ID reconnect. Tests:
  listener wake/leak/coalesce, SSE parser incl. the gateway dialect
  regressions, and the G4 `TestReplay_StreamSurvivesClientKill`
  (abort mid-stream → reconnect with cursor → contiguous, ordered, no
  duplicates, turn.completed seen). All green, linters clean.
- 2026-08-30: **Done-when met live**: real gateway, real model — SSE
  capture showed 11 log frames + 2 live text deltas + keep-alives for a
  haiku-writing turn (write_file → cat → summary, no garbage events);
  client killed mid-run reconnected with Last-Event-ID: 62 and received
  exactly seq 63 (the trailing running→idle state_changed). Nothing
  lost, nothing re-delivered.
