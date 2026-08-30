# Harness adapters — integration notes per runtime

Grounded in the comparative study
([dsh-architecture](https://github.com/warren-wupeng/dsh-architecture),
repo-local summary in `docs/research/2026-08-30-harness-landscape.md`).
Interface and rationale: ADR-004. This page is what an implementer reads
before writing a specific adapter. Keep it factual; when a harness's
behavior changes, fix this page in the same PR as the adapter.

## What all three studied harnesses agree on

- Append-only session record is the single source of truth; everything else
  (model context, UI, resume, telemetry) is a projection.
- The step loop is isomorphic: derive context → stream request → settle
  tool calls → append → continue while `finish == tool-calls`.
- Durable payloads must be serializable and validatable.

This is why normalization works: the LCD event model is real, not aspirational.

## OpenCode (first external adapter, M4)

- **Drive:** it is an HTTP server (Effect HttpApi + OpenAPI); all UIs are
  just clients of the event stream (SSE/WebSocket). We run it headless in
  the worker and act as one more client — no forks, no patches.
- **Events:** SessionEvent bus → SSE. Map to `message.*` / `tool.*` /
  `turn.completed`.
- **Governance (strongest surface):** built-in allow/ask/deny rule engine,
  per-agent defaults. The `permission.ask` plugin hook can *delegate
  approval* — we ship an agentd plugin that forwards `permission.ask` to our
  policy engine and returns its verdict. `tool.execute.before/after` hooks
  give us input/output observation for audit.
- **Resume gotcha:** Context Epoch — compaction rotates the provider cache
  baseline. Checkpoint tokens must pin the epoch; resuming across a
  compaction boundary is allowed but must not pretend the baseline survived.
- **Bonus:** shadow-git Snapshot service tracks file changes per turn —
  maps naturally to our per-turn artifact diffs.

## deepseek-harness

- **Drive:** headless bundle; behavior changes via the Cordis composition
  layer (`cordis.patch.yml` overlays, `--dump-config` to verify what
  booted). Integration = ship an agentd provider plugin + patch file, not a
  fork.
- **Governance:** `ctx.tools` guard execution pipeline. Waterfall hooks must
  call `next()` or the chain short-circuits — our guard plugin denies by
  short-circuit with a remediation payload (G5).
- **Invariant worth stealing:** dsh runtime-asserts *model-visible ⟺
  logged* — anything entering a model request must be reconstructible from
  the log. Adopted into agentd as G3's sharpened form.
- **Replaceability proof:** their agent-loop is itself a replaceable plugin
  — existence proof that "the loop is a plugin" is a viable architecture,
  not just our bet.

## pi

- **Drive:** headless trio — `pi-protocol` (CBOR frames) + `pi-server` +
  `pi-client`. Events via `pi.on(...)` (30+ event types: `tool_call`,
  `tool_result`, `before_provider_request`, `session_*`, `turn_*`).
- **Steering:** mid-turn user messages are a first-class queue in the outer
  loop — maps 1:1 to our rule-2 queue semantics.
- **Truncation detail:** `stopReason == length` → the whole tool-call batch
  is failed (arguments may be truncated). Adapters must replicate this —
  never execute a possibly-truncated tool call.
- **Governance:** none by design ("trust boundary is the OS/container").
  With pi, enforcement = launch config + sandbox egress + audit. Do not
  promise more.
- **Resume:** session tree (lane/branch), fork at branch or tree scope;
  compaction entries carry branch summaries.

## claude-code (second external adapter candidate)

- **Drive:** headless `--output-format stream-json` (NDJSON), `--resume`
  for session continuation.
- **Governance:** PreToolUse hooks can approve/deny tool calls — agentd
  ships a hook command that calls the policy engine.
- **Note:** not covered by the comparative study; verify behaviors against
  the live CLI before promising anything in the adapter.

## native (reference implementation)

- `docs/design/agent-loop.md`. In-orchestrator loop, finest-grain policy
  hooks, event-log replay resume. Exists to (a) validate the Harness
  interface from the inside, (b) cover local models, (c) demo policy-in-loop.

## Conformance suite (the adapter contract's teeth)

Per adapter, a golden-transcript test: run a fixed scripted task
(read → edit → exec → done), record the normalized event stream, diff
against the checked-in golden stream. Harness upgrades that change protocol
behavior turn CI red before users notice. An adapter is "supported" only
while its conformance suite is green.
