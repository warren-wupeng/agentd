# Agent loop design (orchestrator core)

Scope: M2 delivers the minimal version of this; M3 hardens durability and
streaming. This document describes the target shape so M2 code is written
against the right seams. Related: ADR-001 (where tools execute), ADR-003
(event store), `docs/GOLDEN.md` G1/G3.

## The one-sentence design

**The loop is a reentrant state machine over the session's event log — not an
in-memory coroutine.** Each iteration is a pure-ish step:

```
step(session_id) → read events → project state → maybe act → append events (1 tx)
```

Any process can run `step` for any session at any time. Crash anywhere, rerun
the step, get the same outcome. There is no loop-local state that the log
cannot rebuild (G3). "The agent is running" is not a thread somewhere — it is
the fact that the log's tail says the turn is unfinished.

## Event vocabulary (minimum set)

| type | actor | payload | notes |
|---|---|---|---|
| `message.user` | user | content blocks | queue gate via `processed_at` (ADR-003) |
| `message.assistant` | agent | content blocks (text, tool_use) | persisted **before** any tool executes |
| `tool.requested` | system | tool_use_id, name, input | policy verdict attached |
| `tool.completed` / `tool.failed` | system | tool_use_id, output / error | dedupe key = tool_use_id |
| `turn.completed` | system | stop_reason, usage | `end_turn` / `requires_action` / `retries_exhausted` |
| `context.compacted` | system | summary, covers_seq_range | replay treats as checkpoint |
| `session.state_changed` | system | from → to | G1: only way state changes |
| `escalation.requested` | system | reason, context refs | parks the turn at `requires_action` |

## The step sequence

1. **Project.** Replay the session's events into an in-memory projection:
   message history, pending tool calls, current turn state.
2. **Drain user input.** If unprocessed `message.user` events exist and the
   turn is parked (`requires_action`), mark `processed_at` and continue.
   Mid-turn user messages queue — they are injected at the next turn boundary,
   never mid-tool-batch.
3. **Budget check.** Governance: tenant RPM/token budget, session max-turns.
   Exceeded → `escalation.requested`, park. No silent truncation.
4. **Compact if needed.** Projected history near context budget → compaction
   (below) before the model call.
5. **Model call.** Build request from projection (system prompt from the
   agent's pinned version, tool schemas, history) → `ModelProvider.Complete`.
   Append `message.assistant` **immediately, in its own transaction**.
6. **Dispatch tools.** For each `tool_use` block, in order:
   - **Policy check** (`allow` / `ask` / `deny`):
     - `deny` → `tool.completed` with the denial + remediation as the tool
       result (G5). The model sees the denial as data and adapts; the loop
       never crashes on policy.
     - `ask` → `escalation.requested`, set session idle with
       `stop_reason=requires_action`, stop the step. A human (or another
       agent) answers via API; that answer is a `message.user` and rule 2
       resumes the turn.
     - `allow` → execute.
   - **Execute** against the session's sandbox via `SandboxProvider`
     (ADR-001). Append `tool.completed` / `tool.failed`. A tool failure is
     *data for the model*, not a loop error — the loop only retries model
     calls and infrastructure, never tool semantics.
7. **Loop or park.** If the assistant message had tool calls → step ends with
   the turn unfinished; the scheduler picks the session up again immediately.
   If `end_turn` → `turn.completed`, session → idle. Transient model errors →
   backoff retry with jitter; N failures → `turn.completed` with
   `retries_exhausted`.

## The idempotency rule (the part everyone gets wrong)

Model calls are safe to redo; **tool calls are not.** A model call is a read —
crash after receiving the response but before persisting, and redoing it costs
tokens but corrupts nothing. A tool call may `rm`, `git push`, or charge a
credit card.

So the ordering in step 5–6 is load-bearing:

1. Persist `message.assistant` (containing the provider's stable `tool_use`
   ids) **before** executing any tool from it.
2. Before executing a tool, check the log for an existing
   `tool.completed`/`tool.failed` with that `tool_use_id`. Present → skip to
   the result, never re-execute.

This gives exactly-once tool execution with at-least-once stepping — the
durable-execution pattern (Temporal-style) without a workflow-engine
dependency. The price we pay: a crash between persisting the assistant message
and executing tools leaves a persisted message whose tools never ran — which
rule 1's projection handles naturally on the next step (pending tool calls
have no results yet, so they get dispatched).

## Concurrency model

One goroutine per *active* session, actor-style, fed by a mailbox (channel).
The actor owns no truth — it just runs `step` in a loop until the turn parks.
Idle sessions hold no goroutine: they are rows whose tail says "waiting", and
any event append (user message, escalation answer, scheduler tick) re-spawns
the actor. This is what `rescheduling → running ↔ idle` means physically:
the state machine is in the database; goroutines are an execution detail.

Multiple orchestrator replicas shard sessions by consistent hash on
`session_id`; a `SELECT ... FOR UPDATE SKIP LOCKED` claim on the session row
is the fencing mechanism, so two replicas can never run `step` for the same
session concurrently.

## Context management (compaction)

Two phases, cheapest first:

1. **Tool-result truncation.** Large historical tool outputs are replaced with
   a stub (`{truncated: true, original_bytes, ref}`). Lossless enough: the
   sandbox filesystem still holds the artifact.
2. **Turn summarization.** Oldest turns are summarized by the model into a
   `context.compacted` event carrying the summary and the `seq` range it
   covers. Replay after compaction = the latest compaction event + all events
   after its covered range.

Compaction is lossy and recorded — the `context.compacted` event is part of
the auditable history, so Eval and debugging can see exactly what the model
stopped being able to see.

## Streaming (M3)

Provider deltas (token chunks) are **ephemeral** — fanned out to SSE clients,
never written to the log. Only the assembled `message.assistant` is persisted.
A client connects → replays from `events` (durable history) → tails live
deltas (transient). This keeps the log small and makes replay deterministic:
you replay facts, not fragments.

## Interfaces (Go sketch)

```go
type ModelProvider interface {
    Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error)
    Stream(ctx context.Context, req *CompletionRequest) (<-chan Delta, error) // M3
}

type Tool interface {
    Name() string
    Schema() jsonschema.Schema      // what the model sees
    Policy() policy.DefaultVerdict  // static allow/ask hint; engine can override
    Execute(ctx context.Context, sb sandbox.Handle, input json.RawMessage) (Result, error)
}

// The loop itself, in one signature:
func Step(ctx context.Context, s *store.Store, sessionID uuid.UUID) (StepOutcome, error)
```

M2 ships `ModelProvider` for Anthropic first (OpenAI-compatible second), and
four tools — `bash`, `read_file`, `write_file`, `edit_file` — executing in a
single local Docker sandbox.

## What the loop never does

- Never executes tools outside a `SandboxProvider` handle.
- Never mutates session state without appending the event in the same tx (G1).
- Never blocks a turn on unprocessed user input mid-tool-batch (rule 2).
- Never retries a tool execution — only model calls and store operations.
- Never holds unlogged decisions: policy verdicts, budget denials, compaction
  boundaries, and retries are all events.

## Honest limitations

- **Replay cost grows with history.** Long sessions pay O(events) projection
  per step. Mitigation: snapshot projections alongside `context.compacted`
  checkpoints (additive, post-M3).
- **SKIP LOCKED fencing is Postgres-scoped.** Fine per ADR-003's throughput
  reality; a multi-region control plane would need real leader election per
  session — out of scope by design.
- **Compaction quality is model-dependent.** A bad summary silently narrows
  what the agent knows. The compaction event makes this inspectable, not
  preventable; Eval (M6) is where we catch regressions from it.

## Why not a framework

No langchaingo, no workflow engine, no agent SDK. The core loop is a few
hundred lines we must understand perfectly — it is where our differentiators
(policy hooks, budgets, replay semantics) physically live. Boring technology
(Design principle 6) means owning the parts that define us and renting the
parts that don't (sandboxes, models). The `ModelProvider`/`Tool` seams keep
external runtimes pluggable if that ever changes.
