# Golden principles

Opinionated, mechanical rules that keep this codebase legible to future agent
runs and future humans. Each rule states its enforcement. If a rule can't be
checked mechanically, it's a wish — fix the tooling or drop the rule.

Background cleanup agents scan for deviations and open targeted refactor PRs;
reviewers should be able to approve those in under a minute.

## G1. Every state transition emits an event

Session (and later agent/environment) lifecycle changes are only ever made by
appending the corresponding event in the same transaction as the state update.
No silent column flips, no "temporary" direct UPDATEs.

*Why:* the event log is the source of truth (ADR-003). A transition without an
event is a lie the log can't replay.
*Enforced by:* store-layer API shape — transitions are functions that open a tx,
update state, append event; raw UPDATE on `sessions.state` is not exported.
Structural test asserts every `state` value has a matching event type.

## G2. Secrets live only in internal/vault

No secret material (tokens, keys, provider credentials) in any other package,
in event payloads, in logs, or in sandbox environments. Sandboxes receive
references; the control-plane proxy injects values at the boundary.

*Why:* if agent-written code inside a sandbox can read a secret, the design has
failed (Design principle 3).
*Enforced by:* CI scan for high-entropy strings and known token shapes outside
`internal/vault` + test fixtures; `vault.SecretRef` is the only type allowed to
cross package boundaries.

## G3. State is derived from the event log

No goroutine-local or in-memory truth that can't be reconstructed by replaying
`events` for that session. In-memory projections are caches, never sources.

*Why:* the loop must survive `kill -9` at any point and resume correctly
(see `docs/design/agent-loop.md`).
*Enforced by:* replay tests — every stateful component has a test that rebuilds
state from a recorded event fixture and diffs against the live projection.

## G4. New endpoint → replay test

Any endpoint that mutates session state ships with a test that kills the flow
mid-operation, reconnects, and asserts the client sees a consistent, lossless
history.

*Why:* reconnect = replay + dedupe is the client contract; untested paths are
where sessions silently corrupt.
*Enforced by:* test-naming convention (`TestReplay_*`) checked in CI for new
files under `internal/`.

## G5. Errors are agent-readable

Error strings and deny messages state the remediation, not just the failure.
"tool X denied by policy P" → "tool X denied by policy P; allowed alternatives:
Y, Z; to override, request escalation with reason R".

*Why:* our primary consumer of an error message is an agent deciding what to do
next. A dead-end error turns into a dead-end loop.
*Enforced by:* linter on `errors.New`/`fmt.Errorf` literals in exported paths —
must be wrapped in the typed `agentderr` package with a `remediation` field.

## G6. ADRs precede dependent code

A settled decision gets an ADR in the same PR as (or before) the first code
that depends on it. Superseded ADRs are marked, never deleted.

*Why:* the reasoning is the asset; the code is replaceable.
*Enforced by:* human review. The only non-mechanical rule on this list — kept
honest by PR template checkbox.
