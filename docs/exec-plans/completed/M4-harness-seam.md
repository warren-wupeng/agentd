# M4 — Harness adapter seam + OpenCode adapter + conformance suite

Goal: prove ADR-004's thesis. Done when (roadmap): the same task driven
by `native` and `opencode` produces the same normalized event stream —
asserted mechanically by a golden-transcript conformance test. Also
lands the escalation (`ask`) flow re-scoped here from M3: its killer
use is delegating OpenCode's `permission.ask` to the agentd policy
engine, so it belongs in the milestone that builds that bridge.

## What ships

1. **`internal/harness` — the seam** (ADR-004's interface, adapted to
   the event-log reality the first three milestones built):
   - `Harness`: `Name`, `Capabilities`, `Launch(WorkerSpec) (Handle)`,
     `Run(Handle)` (advances one turn to its park point, appending
     normalized events — synchronous; the dispatcher owns concurrency),
     `Checkpoint(Handle) (CheckpointToken)`, `Resume`, `Interrupt`.
   - `WorkerSpec`: session + pinned agent-version config (model, system
     prompt, tools) + optional ResumeFrom token.
   - `CheckpointToken`: opaque JSON — native: last seq; opencode:
     opencode session id (+ epoch when we can read it).
   - `CapabilitySet`: Hooks / Streaming / PermissionDelegate — honest
     asymmetry, schedulers can match tasks to harnesses.
   - `Dispatcher` implements the API's `Runner` contract (Kick),
     routing each session by its `harness` column; goroutine-per-active-
     session with the active-map discipline from M2's loop.Runner.
2. **Native harness** — the reference implementation behind the seam
   (the seam validator per ADR-004): Launch validates config, Run
   drives `loop.Step` until parked (the step-loop + transition
   discipline extracted from loop.Runner), Checkpoint = last seq,
   Resume = Run (the log IS the resume).
3. **Escalation (`ask`)** — policy engine gains the Ask verdict
   (`git push` → ask in the static rules); the loop records
   `escalation.requested` + `turn.completed{stop_reason:
   requires_action}` and parks idle. The human answer is an ordinary
   `message.user` — the NEXT turn starts with the full history
   (escalated tool result + answer) visible to the model. No
   projection special-casing: `requires_action` ends the turn; the
   answer continues the session.
4. **OpenCode adapter** — first external harness:
   - Drive: external HTTP server (`OPENCODE_URL`) — no subprocess
     management in M4, matching how workers will run it in containers.
     Launch creates an opencode session; the mapping is durable as a
     `harness.launched` event (replayable, no schema change).
   - Run: sends unprocessed user messages as the prompt, subscribes to
     the SSE event bus, normalizes: message parts → `message.assistant`,
     tool executions → `tool.requested/completed`, `permission.ask` →
     **delegated to the agentd policy engine** with the verdict returned
     (allow→once / deny→reject) and recorded — ADR-004's strongest
     governance surface — `session.idle` → `turn.completed`.
   - The wire contract is encoded in a fake-server test double. The
     adapter is "experimental" until validated against a live opencode
     (that validation is the gate to "supported"; conformance green is
     necessary, not sufficient).
5. **Validation layering fix**: store validates harness FORMAT
   (non-empty, bounded); the known-harness set moves to the API layer
   (injected from the harness registry via `WithHarnesses`) — mechanics
   in store, policy in API.

## Tests

- escalation: ask verdict parks at requires_action with
  escalation.requested; a posted answer starts a new turn that runs to
  end_turn; the model saw the escalation notice + the answer
- harness: native behind the seam = same behavior as M2/M3 (existing
  loop tests keep passing through the native adapter); checkpoint token
  round-trips
- dispatcher: routes by harness column; unknown harness kicks fail
  loudly in logs
- opencode: fake server (the wire contract) — launch mapping event,
  permission.ask delegation allow AND deny paths, normalization
- **conformance**: `TestConformance_NativeVsOpenCodeGoldenTranscript` —
  fixed scripted task (write → verify → done) through both harnesses;
  assert identical normalized event TYPE sequences, identical tool
  names in order, identical stop_reason; volatile fields (ids,
  timestamps, usage, free text) excluded by design

## Explicitly deferred

- spawning/managing the opencode server process (container workers
  make it unnecessary in the control plane)
- claude-code adapter (second external, gated on this one proving out)
- live opencode instance validation (environment-gated; until then the
  adapter reports `experimental` in Capabilities-adjacent metadata)
- cross-worker Resume (tokens are produced and validated in-process;
  real worker handoff lands with M5 sandbox/E2B)

## Decision log

- 2026-08-30: `Run` advances a whole turn synchronously instead of
  ADR-004's `Run(task) EventStream` sketch — the event log IS the event
  stream; returning one would double-represent truth. The dispatcher
  owns concurrency; harnesses own translation.
- 2026-08-30: escalation parks via `turn.completed{requires_action}`
  rather than the design doc's mid-turn park — the answer then starts
  the next turn with full history, which is protocol-clean (no orphan
  tool_use without results) and semantically equivalent.
- 2026-08-30: opencode session mapping lives in a `harness.launched`
  event, not a new table — replay gives the adapter its state for free
  (G3 discipline applies to harness bookkeeping too).
- 2026-08-30: OpenCode permission.ask ids and tool parts share no join
  key; permissions arrive immediately before their tool part, so the
  last verdict applies to the next tool.requested — recorded honestly
  in the code rather than pretending a correlation exists.
- 2026-08-30: a delegated Ask (opencode permission ask hitting our
  ask rules) answers reject and parks requires_action — the turn-key
  contract matches the native loop's ask, decided by the same engine.

## Progress log

- 2026-08-30: plan created. No code yet.
- 2026-08-30: **M4 done.** Escalation: policy Ask (git push), loop parks
  at requires_action with a protocol-balanced tool result +
  escalation.requested; the answer starts the next turn (tested: the
  model provably saw both the escalation notice and the answer).
  internal/harness: the seam, Native (the M2/M3 loop behind the
  interface), Dispatcher (api.Runner contract, routes by harness
  column), OpenCode adapter (external server via OPENCODE_URL, mapping
  durable as harness.launched, SSE normalization, permission.ask
  delegated to our engine — allow→once, deny/ask→reject with a
  requires_action park). Store validates harness format only;
  known-harness moves to the API (WithHarnesses). cmd swaps
  loop.Runner for the dispatcher.
- 2026-08-30: **Done-when met mechanically**:
  TestConformance_NativeVsOpenCodeGoldenTranscript — same task through
  both harnesses, identical normalized streams (asserted against the
  golden shape AND against each other), prompt delivered, permission
  delegated and answered with our engine's verdict, both sessions
  parked idle/end_turn. Adapter marked experimental pending live
  opencode validation — conformance green is necessary, not sufficient.
  All suites green, both linters clean, CI green.
