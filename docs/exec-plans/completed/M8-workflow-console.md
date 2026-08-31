# M8 — Workflow DAG v0 + software-dev template + console

Goal (roadmap): the finale — demo "spec in → parallel harness workers
→ merged artifact out". ADR-004's guardrail applies in full: the
executor stays minimal and embedded (single binary); agent semantics
only; "do not rebuild Argo" is a design constraint, not a disclaimer.

## Design in one paragraph

A workflow is a versioned JSON definition: nodes are tasks bound to an
agent + harness with a prompt template, edges come from `depends_on`.
The executor is a topological scheduler: whenever a node's deps are all
completed it launches in parallel with its siblings — each node is a
fresh session driven through the M4 harness seam (third dividend of
the seam: workflows don't know what a harness is). Node outputs
propagate downstream: the node's final assistant text plus any
`output_files` (read from the node session's sandbox after it parks)
are injected into dependent prompts via `{{outputs.<node>}}` /
`{{files.<node>.<path>}}` template variables. Run state (node →
session, status, outputs) persists on `workflow_runs`; the run is
resumable in principle because every node IS a durable session
(M2's reentrancy), though v0 restart-resume is manual re-kick.

## What ships

1. **Migration 0003**: `workflow_runs (id, name, status, definition
   jsonb, node_states jsonb, created_at, updated_at)` — workflow-level
   bookkeeping only; the substance lives in the sessions.
2. **`internal/workflow`**: Definition/NodeDef types, validation
   (unknown deps, duplicate ids, cycles, empty graph), the Executor
   (parallel fan-out, join, per-node retry with the same
   retries-exhausted discipline as the loop), output propagation
   including `output_files`, and run persistence.
3. **API**: `POST /v1/workflows` (definition → 202, runs async),
   `GET /v1/workflows/{id}` (node states + session links). The
   executor rides the same dispatcher-style goroutine discipline.
4. **CLI**: `agentd-server workflow run -file templates/x.json` and
   `workflow status <id>` — the demo path.
5. **`templates/software-dev.json`**: coder → (reviewer ∥ tester) →
   merger. The coder's artifact travels via output_files; the merger
   applies review comments and materializes the merged artifact in its
   own session. **Honest v0 boundary**: nodes run in separate sandboxes
   (one workspace per session); artifacts travel by prompt injection,
   not shared worktree mounts — the mount-based flow lands with
   container-worker scheduling, behind the same definition.
6. **`console/`**: zero-dependency Node.js server (http + static files,
   no build step — the boring philosophy applies to consoles too) +
   vanilla JS frontend: agents list/create, session live view (SSE log
   frames + deltas), workflow runner with a node status board. Talks to
   the Go API directly; no BFF logic beyond static serving.

## Tests

- definition validation: cycles, unknown deps, duplicates rejected
- executor: dependency ordering enforced; PARALLEL fan-out proven
  (two ready nodes run concurrently — asserted via overlapping
  execution windows, not just completion order); node retry on failure;
  output propagation (text + files) lands in downstream prompts
- **done-when (mechanical)**: the software-dev template with scripted
  per-node models — coder writes the artifact, reviewer+tester fan out
  in parallel, the merger provably received the coder's file + the
  review + the test verdict, wrote the merged artifact in its sandbox,
  workflow status completed
- api: workflow endpoints (G4 replay test on the run status shape)

## Explicitly deferred

- shared worktree mounts across nodes (needs container-worker
  scheduling; the definition shape already carries the seam)
- workflow-level retry policies beyond per-node, cron/scheduled
  workflows, workflow versioning tables (definitions are files, like
  eval datasets — review artifacts live in git)
- resume-on-restart automation (every node is a durable session; a
  re-kick recovers — automating the detection is post-v0)
- console auth/multi-user (single-operator demo scope)

## Decision log

- 2026-08-31: artifacts travel by prompt injection, not shared
  worktrees — the v0 keeps every node an ordinary isolated session
  (the security story stays exactly as M5 proved it), and the
  definition already carries `output_files` so the mount-based upgrade
  changes the executor, not the templates.
- 2026-08-31: zero-dep console (no React build) — M8's console is an
  operator's demo surface, not a product; a build chain that needs a
  build chain is not boring. React arrives when there's a user.
- 2026-08-31: run state on one jsonb row per run — workflow-level
  bookkeeping is exactly as durable as it needs to be; the nodes'
  sessions are the real record (G3 applies: everything reconstructible).

## Progress log

- 2026-08-31: plan created. No code yet.
- 2026-08-31: **M8 done.** internal/workflow: Definition/NodeDef with
  validation (Kahn cycle check, unknown deps, duplicates), the Executor
  (wave-based topological scheduling — ready nodes launch as real
  goroutines, per-node retry, output propagation via
  {{outputs.<node>}}/{{files.<node>.<path>}} with unknown variables
  left visible), migration 0003, run persistence. API: POST/GET
  /v1/workflows. CLI: workflow / workflow-status. The software-dev
  template. Console: zero-dep Node.js server (static + streaming
  /api proxy so SSE flows and no CORS exists) + vanilla JS frontend
  (agents, live session view with SSE, workflow node board).
- 2026-08-31: **Done-when, twice**. Mechanically:
  TestDoneWhen_SoftwareDevFlow — pipelineModel plays all four roles;
  dependency order asserted from event timestamps; PARALLELISM proven
  from the logs (reviewer/tester overlap window, 400ms role sleeps);
  the merger's prompt provably contains the coder's file + review +
  verdict; MERGED.md read from the merger's sandbox. Live: real
  gateway (gemini-3.5-flash), spec "is_palindrome" — the reviewer
  suggested the two-pointer optimization, the merger INCORPORATED it
  into the final artifact; parallelism verified from session
  timestamps. The console demo ran through its own /api proxy (the
  browser tool was broken mid-demo — its own header bug, not ours;
  the console data path was exercised end-to-end instead).
