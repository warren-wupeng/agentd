# AGENTS.md

Map, not manual. ~100 lines. Everything deeper lives in `docs/` — follow the
pointers instead of asking for a bigger file.

## What this is

agentd — self-hosted managed agents platform. Versioned agent configs, durable
session orchestration, sandboxed tool execution. The open-source answer to
Anthropic's Managed Agents. Apache-2.0. Status: pre-alpha; design is settled,
code is landing milestone by milestone.

Read first: `README.md` (the spec), then `docs/adr/` (settled decisions).
Code that contradicts an ADR is a bug; changing an ADR requires a new ADR that
supersedes it.

## Stack

- Control plane: **Go** (`cmd/`, `internal/`)
- Console: Node.js BFF + React (`console/`)
- SDKs: TypeScript, Python (`sdk/`)
- Infra: Postgres (event store, ADR-003), Docker + E2B (sandboxes, ADR-001)

## Repo layout

```
cmd/agentd-server/     control-plane API entrypoint
internal/orchestrator/ agent loop, session state machine
internal/sandbox/      SandboxProvider impls (docker dev, e2b prod)
internal/vault/        credential store + injection proxy
internal/governance/   tenancy, RBAC, budgets, audit log
internal/policy/       guardrail engine (tool / content / network)
internal/eval/         datasets, scorers, version regression gate
internal/telemetry/    OTel tracing
sdk/ts  sdk/python     client SDKs (stream-first)
console/               web console
deploy/                compose + helm
docs/                  system of record — see below
```

## docs/ — the system of record

If it isn't in this tree, it doesn't exist. Decisions made in chat, PRs, or
people's heads must land here or be treated as undecided.

```
docs/adr/            settled decisions (001 sandbox, 002 no-wire-compat, 003 event store)
docs/design/         working designs (agent loop, harness-engineering adoption)
docs/research/       external landscape research
docs/exec-plans/     active/ + completed/ execution plans, tech-debt tracker
docs/GOLDEN.md       mechanical invariants, enforced by lint + CI
```

Progressive disclosure: start here, open only the doc your task touches.

## Working rules

The short list — full versions with rationale and enforcement in
`docs/GOLDEN.md`:

1. Every session state transition emits an event. No silent mutations.
2. Secrets live only in `internal/vault`. No secret material anywhere else.
3. Session state is derived from the event log. No goroutine-local truth.
4. New endpoint → replay test (kill mid-run, reconnect, assert nothing lost).
5. Errors are agent-readable: say what to do, not just what failed.
6. Settled decisions get an ADR before the code that depends on them.

## Commands

(These land with M1; the repo is docs-only until then.)

```
make dev-up     # postgres via compose
make test       # go test ./...
make lint       # golangci-lint + arch rules
```

## When a task fails

Do not retry harder. Identify the missing capability — tool, doc, guardrail,
test — add it to the repo, then proceed. See
`docs/design/harness-engineering.md` for why we work this way.
