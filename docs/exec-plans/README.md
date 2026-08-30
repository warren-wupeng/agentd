# Execution plans

Plans are first-class artifacts. Small changes use ephemeral lightweight plans
(a few bullets in the PR description). Complex work gets a plan file here,
checked into the repo, kept current as work proceeds.

## Layout

- `active/` — work in progress. One file per milestone or large task.
- `completed/` — finished plans, moved here untouched. They are the decision
  history; never edit them after completion except to fix factual errors.
- `tech-debt-tracker.md` — known debt, each entry with origin and severity.

## Plan format

```markdown
# <title>

Goal: one sentence. Done when: observable acceptance.

## Tasks
Numbered, small enough for one focused agent run, each with its own
acceptance criterion. Order matters; note parallelizable tasks.

## Decision log
Dated, append-only. Every "we chose X over Y" made while executing —
especially the small ones that would otherwise live only in a PR thread.

## Progress log
Dated, append-only. What's done, what's blocked, what surprised us.
```

## Rules

- A plan with no acceptance criteria per task is not a plan; it's a wish.
- When reality diverges from the plan, update the plan the same day. A stale
  plan is worse than none (it teaches agents to distrust the repo).
- Completed plans move to `completed/` in the PR that finishes the work.
