# Architecture Decision Records

Settled design decisions for agentd. Each ADR records context, the decision, rationale, and — just as importantly — what would make us revisit it. Read these before proposing a competing design; superseded ADRs are marked, never deleted.

| # | Title | Status |
|---|---|---|
| [ADR-001](./ADR-001-sandbox-backend.md) | Sandbox backend — E2B (Firecracker) for production, Docker for dev | Accepted |
| [ADR-002](./ADR-002-no-wire-compatibility.md) | No wire-compatibility with Anthropic Managed Agents | Accepted |
| [ADR-003](./ADR-003-postgres-event-store.md) | Postgres as the session event store | Accepted |

Format: Context → Decision → Rationale → Consequences (positive *and* negative) → Alternatives considered. Honest limitations are mandatory — an ADR without them is marketing.
