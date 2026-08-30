# M2 — native agent loop

Goal: a `harness: native` session runs end-to-end — user message in, model
turns with tool calls (bash/read_file/write_file/edit_file) executing in a
sandbox, `turn.completed` out, session parked at `idle` with a stop_reason.
Design of record: `docs/design/agent-loop.md` (status note included).

Done when: one session runs read → edit → exec end-to-end against a real
model (manual demo against the OpenAI-compatible gateway), with the loop
tests below green in CI.

## What ships

1. **Event vocabulary** (store): `message.assistant` (actor agent),
   `tool.requested` / `tool.completed` / `tool.failed` (actor system),
   `turn.completed` (actor system). Schema unchanged — payloads are jsonb.
2. **`internal/model`** — `ModelProvider` with provider-neutral blocks
   (text / tool_use / tool_result) and one implementation:
   OpenAI-compatible chat completions (works with any such endpoint —
   OpenAI, OpenRouter, our gateway). Anthropic-native provider later;
   the seam is what matters now.
3. **`internal/sandbox`** — `SandboxProvider` (ADR-001) with two
   implementations: `exec` (os/exec in a per-session workdir; dev and
   no-Docker environments) and `docker` (CLI-based, per-exec
   `docker run --rm` with the workdir bind-mounted). File tools operate
   on the workdir directly with a path-traversal guard; bash goes through
   the provider.
4. **`internal/policy`** — verdict (allow/deny) computed per tool call and
   recorded on `tool.requested`. Deny returns the denial + remediation as
   the tool result (G5) — the model adapts, the loop never crashes.
5. **`internal/tools`** — `bash`, `read_file`, `write_file`, `edit_file`
   behind a `Tool` interface (schema, policy default, execute vs sandbox
   handle). Enabled set comes from the agent version config `tools` field.
6. **`internal/loop`** — the reentrant `Step` and a `Runner`:
   - projection: events → message history + pending tool calls + turn state
   - idempotency rule: assistant message persisted before any tool runs;
     tool results deduped by `tool_use_id` (exactly-once tools,
     at-least-once stepping)
   - model-call retries with backoff (3) → `retries_exhausted`;
     per-turn step cap (40) → `retries_exhausted` — no runaway sessions
   - Runner = one goroutine per active session, `Kick(sessionID)` from the
     API (auto on `message.user` append, manual via
     `POST /v1/sessions/{id}/run` for crash recovery), state transitions
     rescheduling → running → idle(+stop_reason) only via
     `TransitionSession` (G1)
7. **Config** — `MODEL_BASE_URL`, `MODEL_API_KEY` (optional: server runs
   CRUD-only without them; a native turn without provider config parks with
   `retries_exhausted` and a remediation in the event log), `SANDBOX_PROVIDER`
   (`exec`|`docker`, default `exec`), `SANDBOX_BASE`, `LOOP_MAX_STEPS`,
   `LOOP_MODEL_RETRIES`.

## Tests (fake ModelProvider, exec sandbox — deterministic, no network)

- happy path: user → write_file → bash → end_turn; assert event ORDER
  (assistant before its tool events), final state idle/end_turn
- crash recovery: assistant event with tool_use but no tool events
  (simulated crash mid-step) → next Step executes the tools exactly once;
  re-Step is a no-op (dedupe by tool_use_id)
- policy deny: model asks for a denied bash command → tool.completed
  carries the denial; no sandbox execution
- model errors: 3 failures → turn.completed retries_exhausted, idle
- step cap: model that always emits tool calls → cap → retries_exhausted
- API: `POST /v1/sessions/{id}/run` (G4 replay test) and auto-kick on
  message append

## Explicitly deferred (recorded, not forgotten)

- `ask` verdict + `escalation.requested` (needs the human-answer flow; M3)
- compaction / context budget (M3; projection handles full history now)
- streaming deltas (M3), sub-agents, budgets beyond the step cap
- multi-replica sharding (`SKIP LOCKED` claim) — single-process Runner in
  M2; the Step design keeps it a drop-in later

## Decision log

- 2026-08-30: dev sandbox = `exec` provider, not Docker-on-every-machine:
  ADR-001 keeps `docker` for real dev/prod-ish use, but the build sandbox
  and CI have no Docker daemon — tests must run hermetically. The
  `SandboxProvider` interface is the point; `exec` is honest about being
  the no-isolation dev fallback and the config default, `docker` is
  opt-in where a daemon exists.
- 2026-08-30: OpenAI-compatible provider before Anthropic-native — one
  wire format covers OpenAI/OpenRouter/gateway/vLLM and lets the E2E demo
  run against the internal gateway today. The neutral block types keep an
  Anthropic adapter a mapping exercise, not a rewrite.
- 2026-08-30: step cap maps to `retries_exhausted` for now; the design's
  budget→escalation path lands with `ask`/escalation in M3.
- 2026-08-30 (found during the E2E demo): assistant messages carrying
  tool_calls MUST serialize an explicit `"content": null` — the field
  being absent 500s on some OpenAI-compatible backends (our gateway's
  google-vertex route; OpenAI itself tolerates absence). The wire struct
  therefore has NO omitempty on content. Regression test pinned in
  `internal/model/openai_test.go`. Also learned: this gateway's
  anthropic/claude routes 500 on ANY request with tools — model choice
  for demos: gemini-3.5-flash or gpt-5.4.
- 2026-08-30: go-arch-lint's deepScan records a wiring-level edge when a
  `loop.Runner` value flows into `api.WithRunner` in cmd — declared as
  `api mayDependOn loop` with a comment rather than fighting the tool;
  the real import graph stays consumer-side (api defines the Runner
  interface).

## Progress log

- 2026-08-30: plan created. No code yet.
- 2026-08-30: **M2 done.** All packages landed: model (neutral blocks +
  OpenAI-compatible provider), sandbox (exec + docker providers, path
  traversal guard, output caps), policy (allow/deny recorded on
  tool.requested), tools (bash/read_file/write_file/edit_file), loop
  (projection + Step + Runner), API wiring (auto-kick on message.user,
  POST /v1/sessions/{id}/run, 409 when the loop isn't configured).
  Tests: loop suite (happy path ordering, crash-recovery exactly-once,
  policy deny as data, retries exhausted, step cap, runner state trail)
  + unit tests for policy/tools/model + 3 new API tests — all green,
  golangci-lint and go-arch-lint clean.
- 2026-08-30: **Done-when met for real**: demo session vs the live
  gateway (google/gemini-3.5-flash) — user asks for fib.py: write →
  verify (fib(10)=55) → edit_file adds docstring → re-verify → text
  summary → end_turn, session parked idle. 18 events, assistant-before-
  tools invariant holds on the real trace, artifact in the sandbox
  workdir. First demo run failed with retries_exhausted (the content:null
  issue above) — which incidentally exercised the failure path
  end-to-end: three attempts, honest stop_reason, full detail in the log.
