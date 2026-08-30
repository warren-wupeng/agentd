# Harness landscape: deepseek-harness / OpenCode / pi (2026-08-30)

Source: comparative architecture study by Warren —
[dsh-architecture](https://github.com/warren-wupeng/dsh-architecture)
(4 reports: deepseek-harness, OpenCode, pi, cross-comparison; 2026-08
GitHub snapshots). Repo-local summary of what agentd took from it.

## The three, in one line each

- **deepseek-harness** (deepseek-ai, TS, Cordis plugin runtime): "everything
  is a plugin" — the agent-loop itself is a replaceable plugin on the
  service tree (`ctx.llm` / `ctx.tools` / `ctx.fs` / `ctx.sessions`).
- **OpenCode** (anomalyco, TS, Effect-TS): batteries-included server kernel;
  TUI/Web/Desktop/ACP/Slack are all clients of one HTTP+SSE server. Built-in
  allow/ask/deny permission engine; plugin hooks; shadow-git snapshots.
- **pi** (earendil-works, TS, layered npm packages): minimal kernel (4
  tools), self-extending via jiti-loaded TS extensions; session tree as
  append-only JSONL; no built-in permissions — isolation delegated to
  containers (Gondolin micro-VM / Docker / OpenShell).

## Consensus (validates agentd's core bets)

1. Append-only session record = single source of truth; all else is
   projection. → validates ADR-003.
2. Model context must be reconstructible from the persistence layer. dsh
   formalizes: **model-visible ⟺ logged**, runtime-asserted. → adopted as
   G3's sharpened form (previously applied in cfo-control-tower, issue #181).
3. Step loops are isomorphic across all three. → normalization to one event
   vocabulary is engineering, not research.

## Divergence (what adapters must absorb)

- How much lives in the kernel: pi (almost nothing) → OpenCode (batteries)
  → dsh (nothing is privileged).
- Governance: none (pi) → built-in rules + delegatable `permission.ask`
  (OpenCode) → plugin guard pipeline (dsh).
- Resume: session-tree fork (pi) → SQLite + Context Epoch (OpenCode) → log
  projection (dsh).

## What agentd did with it

ADR-004 (harness-agnostic runtime), `docs/design/harness-adapters.md`
(per-harness integration notes), G3 sharpened, roadmap M4 = adapter seam +
OpenCode adapter + conformance suite.
