# ADR-004: Harness-agnostic runtime — the loop is a plugin, not the product

- **Status:** Accepted (supersedes the "loop is the differentiator" stance in `docs/design/agent-loop.md`'s final section)
- **Date:** 2026-08-30

## Context

Three facts converged:

1. **The harness layer is commoditizing.** OpenAI open-sourced the Codex
   harness (2026-08-19); every lab ships a CLI agent (Claude Code, Codex,
   deepseek-harness, OpenCode, pi, …). Each improves faster than any small
   team's own loop can.
2. **Our own research already concluded the moat is not the loop** — it is
   sandbox infrastructure, the durable event stream, and the credential
   proxy (see `docs/research/2026-08-30-managed-agents-landscape.md`).
3. **A comparative architecture study** of deepseek-harness, OpenCode, and pi
   (see `docs/research/2026-08-30-harness-landscape.md`) shows all three
   converge on our core bets: append-only session log as the single source
   of truth, model context reconstructible from persistence, and an
   isomorphic step loop (stream → tool settlement → append → continue). They
   diverge only on *where the seams are*.

Meanwhile the target users run heterogeneous harnesses and want one control
plane: DAG-scheduled ephemeral workers, each running a chosen harness + model
+ skills, with session checkpoints, credential injection, and workspace
mounts — workers disposable and rebuildable.

## Decision

agentd becomes **harness-agnostic**. Sessions execute through a `Harness`
interface; external CLI harnesses are first-class runtimes. Two primitives
join the original four:

- **Harness** — a pluggable agent runtime. `native` (our own loop,
  `docs/design/agent-loop.md`) becomes the *reference implementation*, no
  longer the only one.
- **Workflow** — a versioned DAG definition: nodes are tasks bound to an
  agent + harness, edges are dependencies, with parallel fan-out/join and
  retry policy. Implementation is post-v1; the seam is designed now.

```go
type Harness interface {
    Name() string                         // "native" | "opencode" | "claude-code" | ...
    Capabilities() CapabilitySet          // {Hooks, Resume, Streaming, PermissionDelegate}
    Launch(ctx context.Context, spec WorkerSpec) (Handle, error)
    Run(ctx context.Context, h Handle, task TaskSpec) (EventStream, error)
    Checkpoint(ctx context.Context, h Handle) (CheckpointToken, error)   // opaque
    Resume(ctx context.Context, h Handle, tok CheckpointToken) (EventStream, error)
    Interrupt(ctx context.Context, h Handle) error
}

type WorkerSpec struct {                   // fixed at worker creation
    Harness, Model string
    Skills     []string
    ResumeFrom *ResumePoint                // {SessionID, CheckpointToken}
}
```

Adapters translate harness-native protocols into the existing agentd event
vocabulary (`message.*`, `tool.*`, `turn.completed`, usage). The event log
stays canonical and unchanged; the harness is a black-box producer.

## Grounded integration surfaces (from the harness study)

| Harness | Drive via | Governance hook | Resume semantics |
|---|---|---|---|
| OpenCode | HTTP server + SSE/WS event stream | **Strongest:** built-in allow/ask/deny engine; `permission.ask` hook can be delegated to agentd's policy engine | SQLite + Context Epoch (compaction rotates the cache baseline) |
| deepseek-harness | headless bundle; ship an agentd guard plugin | Guard pipeline on `ctx.tools` (waterfall — not calling `next()` short-circuits = deny) | log projection fork/resume |
| pi | `pi-protocol` (CBOR) headless client; events via `pi.on` | None by design — container/network only | session-tree fork (branch or tree scope) |
| native | in-process (orchestrator) | in-loop policy hooks (finest grain) | event-log replay (ADR-003) |

Governance is therefore **tiered**: launch-time config (all) → hook-time
deny (capability-dependent) → network egress (all, the floor) → full audit
(all). Real-time deny degrades gracefully by capability; containment and
accountability never do.

Adapter order: `native` (M2) → **OpenCode first external** (M4 — cleanest
server-native surface, permission delegation, validates the seam on someone
else's protocol early) → claude-code (market weight; stream-json +
PreToolUse hooks) → codex / dsh / pi as demand proves out.

## Rationale

1. **Bet on the layer above the commodity.** K8s doesn't care about the
   container runtime; agentd doesn't care about the loop. The durable value
   is agent-specific semantics generic orchestrators lack: credential
   injection, cross-harness checkpoints, event normalization, budgets, eval.
2. **The least-common-denominator event model demonstrably exists** — three
   independent harnesses converge on it. Normalization is engineering, not
   research.
3. **Native loop as seam validator.** If our own loop can't sit behind the
   interface, the interface is wrong. It also covers local models and
   policy-in-loop demos. It is a component, not the product.
4. **Sub-agents get a better model for free.** Sub-agents are Workflow DAG
   nodes — isolated workers with their own quota, audit, and retry — not
   in-loop threads. This replaces the thread model in Platform capabilities.

## Honest limitations

- **Adapter maintenance treadmill.** N harnesses × evolving protocols.
  Mitigation: LCD events only; ≤3 adapters in v1; a conformance suite of
  golden event streams per adapter — harness protocol drift turns CI red.
- **Capability asymmetry is permanent.** We will not pretend all harnesses
  are equal; `CapabilitySet` exposes differences honestly and scheduling can
  match tasks to harness capabilities.
- **Do not rebuild Argo.** The Workflow executor stays minimal and embedded
  (single-binary self-hosting). Agent semantics only; general-purpose
  workflow features are an explicit non-goal.
- **Control planes commoditize too.** Our answer: vendor planes are
  single-harness by construction; the neutral, self-hosted, multi-harness
  layer is the gap. If that gap closes, this ADR gets revisited.

## Consequences

**Positive:** any harness improvement in the ecosystem is instantly
inheritable; enterprise heterogeneity is a feature not a porting project;
M4's conformance criterion ("same task, two harnesses, same normalized event
stream") is a crisp, falsifiable test of the whole design.

**Negative:** M2's native loop is no longer demo-complete on its own — the
story now requires one external adapter (M4) to prove the thesis. Governance
real-time deny is capability-dependent (accepted; network + audit floor
documented above).

**Revisit if:** a single harness captures >80% of real usage (the seam's
cost exceeds its value), or adapter maintenance exceeds one engineer-quarter
per year.
