# Harness engineering — what we adopted and why

Source: OpenAI, *"Harness engineering: leveraging Codex in an agent-first
world"* (Ryan Lopopolo, 2026-02-11,
<https://openai.com/index/harness-engineering/>). Their experiment: empty repo
in late August 2025, 3 engineers (later 7), five months, ~1M lines of
agent-written code, ~1,500 merged PRs, zero manually-written code, product in
daily internal use. Humans steer; agents execute.

Two caveats we keep in mind: they state plainly this **should not be assumed to
generalize without similar investment** — the payoff comes from per-repo
infrastructure, not from the model. And their merge philosophy (minimal
blocking gates) only holds when verification is mechanical and corrections are
cheap.

This doc records the five practices we adopt for building agentd itself, and —
the larger point — how each practice maps to an agentd *product* capability.
Their per-repo infrastructure is our platform layer; that mapping is the
product thesis.

## Practice 1 — Agent legibility (make everything readable)

*Theirs:* app bootable per git worktree; Chrome DevTools Protocol, DOM
snapshots, screenshots wired into the agent runtime; ephemeral observability
stack per worktree (LogQL/PromQL queries by the agent); objective acceptance
("startup < 800ms"). Repository as the only system of record: ~100-line
AGENTS.md as table of contents, structured `docs/`, progressive disclosure,
doc-gardening agents, mechanical freshness checks. Anything not accessible
in-context effectively doesn't exist.

*How we build agentd:* `AGENTS.md` + `docs/` layout in this repo follows their
shape (map-not-manual, progressive disclosure, doc-gardening planned). Research
and designs live in the repo, not in chat. M1 ships `make dev-up` so any change
is verifiable with one command.

*As agentd product:* this is the core thesis. The **Environment** primitive is
their per-worktree ephemeral stack, generalized. The session event log + OTel
traces are the legibility substrate. Design consequence: an agent running on
agentd can query its **own** logs/traces/events through the API —
observability is closed-loop, not human-only.

## Practice 2 — Depth-first decomposition

*Theirs:* break large goals into small building blocks; when something fails,
never "try harder" — ask what capability is missing and make it legible and
enforceable.

*How we build agentd:* milestones M1–M7 carry "Done when" acceptance criteria;
each milestone is decomposed into execution plans under `docs/exec-plans/`
with per-task acceptance criteria.

*As agentd product:* execution plans become first-class **session artifacts** —
plans are events in the stream, versioned and replayable. They store plans in a
repo directory; we store them in the protocol.

## Practice 3 — Enforce architecture up front

*Theirs:* fixed layering (Types → Config → Repo → Service → Runtime → UI),
Providers as the single cross-cutting entry, custom linters whose **error
messages contain remediation instructions**, boundary parsing enforced (shape
validation at edges), taste invariants as static checks. Normally postponed
until hundreds of engineers; with agents it's an early prerequisite.

*How we build agentd:* the `internal/` layering in `AGENTS.md` is enforced by
go-arch-lint in CI from M1, not "when the codebase grows". Errors are
agent-readable by rule (G5).

*As agentd product:* the **guardrail policy engine is runtime architecture
enforcement**. Tool-level allow/ask/deny mirrors their linters; deny messages
carry agent-readable remediation, copied directly from their linter design.

## Practice 4 — Continuous garbage collection

*Theirs:* golden principles encoded in-repo; background agents scan for
deviations on a cadence, update quality grades, open targeted refactor PRs that
automerge. Manual Friday cleanups (20% of the week) did not scale.

*How we build agentd:* `docs/GOLDEN.md` is our golden-principles file, each
rule with mechanical enforcement. Once M2 gives us a working loop, a doc/code
gardening agent runs against this repo on a schedule.

*As agentd product:* **scheduled/background sessions are a first-class
workload** — doc-gardening and GC agents are exactly sessions with a cron
trigger and a repo-write sandbox. The Eval harness (M6) productizes their
QUALITY_SCORE grading.

## Practice 5 — Maximal autonomy with explicit escalation

*Theirs:* one prompt drives the full loop — validate, reproduce, record video,
fix, re-validate, open PR, respond to review, remediate build failures, merge —
escalating to a human only when judgment is required. Works only because
testing, validation, review, and recovery are all encoded in the system.

*How we build agentd:* this is the target state for developing agentd itself.
Prerequisites we track: fast CI, one-command verification, explicit escalation
rules in each exec plan.

*As agentd product:* our session state machine already encodes this —
`stop_reason = requires_action` *is* "escalate when judgment is required",
protocolized. The policy engine's `ask` layer is the escalation path. The
article is external validation that this state-machine design is the right
shape.

## What we deliberately do NOT copy

- **Minimal blocking merge gates** — correct at their verification maturity,
  irresponsible before it. Ours arrives only after replay tests and lint
  coverage make corrections genuinely cheap.
- **Agent-generated-everything as a hard constraint** — a useful forcing
  function for their experiment, a vanity metric for us. Humans may write code
  here; the *environment* must still be agent-first.
- **Their specific tooling** (Codex, their observability stack) — the practices
  are model- and vendor-neutral; agentd keeps them that way.
