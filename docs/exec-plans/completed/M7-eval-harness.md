# M7 — Eval harness v0

Goal (roadmap): trace → dataset → rubric scorer → version-compare
report. Done when: **two agent versions scored on the same dataset,
diff printed** — asserted mechanically in the integration test, and
invocable as `agentd-server eval`.

## Design in one paragraph

An eval run IS a set of ordinary sessions (G3 discipline: auditable
traces like any other run), each pinned to a specific agent version,
driven through the same `Harness` seam the product uses. After a case
parks, its trace is projected into a `RunTrace` (final text, tools
used, stop_reason, turn count, sandbox artifacts) and scored against a
rubric — **deterministic criteria only in v0**: the scorer must be
reproducible in CI without a live model. The compare report joins two
version reports on case id and prints the table with per-case
PASS/FAIL, criterion-level flips, and aggregate score deltas. LLM-judge
criteria are a post-M7 extension behind the same `Criterion` type;
deferring them is a correctness decision, not a scope cut — a scorer
that needs a model to judge needs held-out discipline first (and its
own eval).

## What ships

1. **`internal/eval` — types**: `Dataset{Name, Cases}`,
   `Case{ID, Input, Harness, Rubric []Criterion}`,
   `Criterion{Kind, Arg, Path, Weight}` with kinds:
   `contains`, `not_contains` (final assistant text),
   `tool_used`, `tool_not_used` (from tool.requested events),
   `artifact_contains` (Path read via the sandbox handle, Arg substring),
   `stop_reason`, `max_turns`.
   Datasets are JSON FILES — versioned artifacts that live in git next
   to the agent config, not DB rows; eval results are reports (stdout +
   JSON), not tables.
2. **Scorer**: per-criterion pass/fail with reasons; case score =
   weighted fraction; version score = mean over cases. Deterministic,
   unit-tested per kind.
3. **Runner**: per case — fresh session pinned to the version, post
   input, `harness.Run` to park, project the event log into a
   `RunTrace`, score. Reuses everything the product runs on (M4's seam
   pays its second dividend).
4. **Compare**: `agentd-server eval -dataset ds.json -agent <id>
   -versions 1,2` prints the version table with Δ per case and
   criterion-level flips; JSON report alongside.
5. **trace → dataset**: `agentd-server eval-export -session <id>`
   mines a case stub (input = the session's first user message, empty
   rubric to author) — the honest v0 of the mining leg; deeper trace
   distillation waits for real users' traces.

## Tests

- scorer: each criterion kind, pass and fail, weights
- export: session events → case stub
- **done-when (mechanical)**: one agent, two versions whose configs
  steer a scripted model differently; the same dataset runs against
  both; the printed diff contains the expected flip (v1 fails a case
  v2 passes) and the aggregate delta — asserted, not eyeballed

## Explicitly deferred

- LLM-as-judge criteria (needs held-out + anti-Goodhart discipline; the
  Criterion type is the seam)
- DB-persisted datasets/runs, regression gates in CI on every agent
  version bump (needs the above first)
- multi-turn cases with scripted follow-ups (v0 cases are one turn)
- cost/latency metrics in the report (usage is in the events; surface
  it when someone asks)

## Decision log

- 2026-08-31: deterministic-only rubric v0 — reproducibility in CI is
  the scorer's whole value; a flaky judge is worse than no judge.
- 2026-08-31: eval runs are real sessions on the real harness seam, not
  a parallel simulator — the thing being evaluated is the thing users
  run.
- 2026-08-31: datasets as files — they are review artifacts (rubrics
  get argued about in PRs), and git is where review artifacts live.

## Progress log

- 2026-08-31: plan created. No code yet.
- 2026-08-31: **M7 done.** internal/eval: Dataset/Case/Criterion types
  (7 deterministic kinds), rubric Scorer (weighted, per-criterion
  reasons, weight normalization in both parse and score paths), Runner
  (fresh pinned session per case through the M4 harness seam, trace
  projected from the real event log, artifact reads via the M5 file
  API), Compare printer (per-case PASS/FAIL, criterion-level flip
  naming, aggregate delta), ExportCase (trace → dataset stub). CLI:
  `agentd-server eval -dataset ... -agent ... -versions 1,2` and
  `eval-export -session ...`. Tests: scorer per-kind + weights,
  dataset validation, export mining, and the done-when integration
  (versionedModel dispatches on the version's system prompt — one
  dataset, two behaviors, diff asserted).
- 2026-08-31: **Done-when, live**: `agentd-server eval` against the
  real gateway (gemini-3.5-flash) on the smoke dataset — v1 (terse
  prompt) FAILed "pineapple", v2 (instructed to pick pineapple) PASSed;
  the printed diff: `IMPROVEMENT  [contains "pineapple" now passing]`,
  aggregate +0.25, 1 case flip. eval-export mined a case stub from the
  finished session's trace.
