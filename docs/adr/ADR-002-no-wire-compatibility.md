# ADR-002: No wire-compatibility with Anthropic Managed Agents

- **Status:** Accepted
- **Date:** 2026-08-30

## Context

Anthropic's Managed Agents (beta, `managed-agents-2026-04-01`) is the reference product in this space. The most visible open-source alternative chose 1:1 API compatibility, and "drop-in replacement" is superficially attractive: users of the hosted product could repoint their SDK at agentd and walk away.

So: do we clone the wire format?

## Decision

**Borrow the shapes that work; do not clone the wire format.**

We adopt, deliberately: the four primitives (Agent / Session / Environment / Sandbox), the event-sourced session protocol, `stop_reason` semantics, vault-proxy credential injection, and versioned agent configs.

We do **not** implement Anthropic's endpoint layout, event type names, or beta-header emulation. There is no `agentd` mode that pretends to be `api.anthropic.com`.

## Rationale

1. **Wire-compat means tracking a moving beta.** Their API is explicitly pre-GA and still changing. Cloning it means inheriting their mistakes on our timeline, and re-litigating every breaking change they make — forever.
2. **Legal/ToS gray zone we don't need.** Reimplementing a commercial service's proprietary wire protocol to intercept its customers is a fight with no upside for a young project.
3. **Our differentiators don't fit their shapes.** Anthropic's model has no tenancy, no budgets, no eval resources — governance and eval are exactly where agentd aims to be better. A compat layer would either hide our best ideas or become an incompatible superset: the worst of both worlds.
4. **Real migration cost is low anyway.** Actual user lock-in lives in agent *configs* — prompts, tool schemas, MCP URLs, skill files — which are portable documents, not a session protocol. We will ship a **config importer** (Anthropic agent JSON → agentd agent YAML) instead of a wire clone. That captures 90% of the migration value with 10% of the surface area.

## Consequences

**Positive:** full design freedom; an honest relationship to the reference product; no perpetual compat tax.

**Negative:** users must re-point SDKs, not just base URLs. Mitigation: agentd's TS/Python SDKs mirror the same mental model (agents → sessions → events) so porting is mechanical; and if wire-compat demand proves real later, a translation shim in front of our API is **additive** — it doesn't require unwinding anything decided here.

## Alternatives considered

- **1:1 wire compatibility (open-managed-agents' approach)** — rejected for the reasons above; noted that it also forced them into AGPL-adjacent positioning we'd rather avoid.
- **Compat shim from day one** — rejected: doubles the API surface before we have users, and freezes design decisions we haven't validated yet.
