# ADR-003: Postgres as the session event store

- **Status:** Accepted
- **Date:** 2026-08-30

## Context

The session event log is agentd's single source of truth. Everything hangs off it:

- SSE streaming is a live **tail** of the log; reconnect = replay + dedupe by event id
- `processed_at` gates client acknowledgements (queued → processed)
- Session state transitions are derived from / accompanied by events
- Audit (governance) and Eval (trace → dataset) consume the same log

Requirements: per-session ordered append, point-in-log replay (`FROM seq N`), live tail with low latency, and retention bound to session lifecycle (delete session → its events go away).

The obvious alternatives are real event brokers: Kafka/Redpanda, NATS JetStream, Redis Streams.

## Decision

One Postgres table:

```sql
events (
  id           uuid PRIMARY KEY,
  session_id   uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  seq          bigserial,
  type         text NOT NULL,
  actor        text NOT NULL,          -- user | agent | system
  payload      jsonb NOT NULL,
  processed_at timestamptz,            -- null = queued, set = consumed
  created_at   timestamptz NOT NULL DEFAULT now()
);
```

- **Live tail:** `LISTEN/NOTIFY` on insert (with a polling fallback for gateways that can't hold a PG connection).
- **Replay:** `SELECT ... WHERE session_id = $1 AND seq > $2 ORDER BY seq`.
- **Retention:** `ON DELETE CASCADE` — session deletion takes its log with it.

## Rationale

1. **It eliminates the dual-write problem.** Session state transition and event emission commit in **one transaction**. With a separate broker you need an outbox table and a relay process on day one — two more failure modes before the first user.
2. **Throughput reality.** A session emits O(1–10) events/sec. Even 10k concurrent sessions is comfortably inside a single well-indexed Postgres instance's write capacity. We are not building a firehose; we're building a ledger.
3. **Lifecycle fit.** Broker retention is topic/time-based; ours is per-session. `DELETE FROM sessions` cascading to its events is exactly the semantics we want — Kafka fights this.
4. **One stateful system** to operate, back up, and reason about. Boring technology is a design principle, not an apology.

## Honest limitations

- **`LISTEN/NOTIFY` is not durable and does not survive failover.** Notifications missed during a disconnect are gone. This is acceptable *because the client protocol already mandates replay + dedupe on reconnect* — the notify path is a latency optimization, never the source of truth.
- **SSE gateway fan-out.** Many gateways holding PG connections gets heavy; the fix is a tailer service that fans out to gateways over Redis pub/sub — additive, behind the same replay contract.
- **Table growth.** When `events` gets hot: range-partition on `created_at`, or hash-partition on `session_id`. The `seq` column makes a later migration of the hot tail to Redis Streams additive rather than a rewrite.

## Consequences

**Positive:** transactional consistency between state and log; one system to run; backup/restore covers everything; new engineers already know the mental model.

**Negative:** Postgres becomes the scaling ceiling for event writes — accepted, with the partition/Redis escape hatches documented above. NOTIFY-based tail adds ~ms latency variance versus a broker push — irrelevant against model-latency (seconds).

## Alternatives considered

- **Kafka / Redpanda** — the right tool at 100× our write scale; the wrong operational weight now. Also: per-session retention fights topic semantics.
- **NATS JetStream** — good fit technically, still a second stateful system plus an outbox pattern for the dual-write problem.
- **Redis Streams** — durability guarantees don't match the audit use case; kept in the back pocket as the hot-tail accelerator.
