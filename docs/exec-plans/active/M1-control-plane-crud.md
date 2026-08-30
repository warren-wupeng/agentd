# M1 — control plane CRUD + agent versioning

Goal: Postgres schema and the agents/sessions/events API surface exist and are
tested. Done when: roadmap M1 criterion — API tests pass against a real
Postgres, including version immutability and event replay.

Stack: Go 1.24, pgx/v5, sqlc (typed queries), golang-migrate, net/http ServeMux
(1.22+ patterns — no framework), log/slog. Boring on purpose.

## Tasks

1. **Scaffold.** `go.mod`, `cmd/agentd-server` (config from env, `/healthz`),
   `Makefile` (`dev-up`/`test`/`lint`), compose file with Postgres 16.
   *Accepts:* `make dev-up && make test` green from a clean clone.
2. **Schema.** Migration 0001: `agents`, `agent_versions`, `sessions`,
   `events` per ADR-003. `agent_versions` carries the full config snapshot
   (model, system prompt, tools, mcp_servers) + monotonically increasing
   version number per agent. `sessions` carries `harness text NOT NULL
   DEFAULT 'native'` (ADR-004 — avoids a retrofit migration later).
   *Accepts:* `migrate up && migrate down && migrate up` clean; constraints
   verified (FK cascade on events, unique (agent_id, version)).
3. **Store layer.** sqlc queries + thin wrappers enforcing G1 (state
   transitions only via event-emitting tx functions).
   *Accepts:* store tests against compose Postgres; no exported raw UPDATE on
   `sessions.state`.
4. **Agents API.** `POST /v1/agents` (creates v1), `GET /v1/agents[/{id}]`,
   `PUT /v1/agents/{id}` (creates **new** version, old versions untouched),
   `GET /v1/agents/{id}/versions/{v}`.
   *Accepts:* API test proves immutability — update agent, old version's
   config still retrievable byte-identical; sessions pinned to it unaffected.
5. **Sessions API.** `POST /v1/sessions` (pins `agent_id` + `version`),
   `GET /v1/sessions[/{id}]`. Delete policy: agent with live sessions → 409.
   *Accepts:* session rows always resolve to a concrete version snapshot.
6. **Events API.** `POST /v1/sessions/{id}/events` (actor=user),
   `GET /v1/sessions/{id}/events?after_seq=N&limit=M` (replay).
   *Accepts:* replay test — 50 events, read from seq 20, strictly ordered,
   idempotent re-reads; `processed_at` set via separate claim endpoint.
7. **Session state machine.** `rescheduling → running ↔ idle → terminated`
   as a column updated only inside G1 tx functions, with
   `session.state_changed` events.
   *Accepts:* structural test — every observed state value has a matching
   event; invalid transitions rejected.
8. **CI.** GitHub Actions: `go test ./...` (with service Postgres),
   golangci-lint, go-arch-lint enforcing the `internal/` layering from
   `AGENTS.md`.
   *Accepts:* green required-check on the M1 PR.

Parallelizable: 4+5+6 can proceed together once 3 lands; 8 any time after 1.

## Decision log

- 2026-08-30: Go confirmed for control plane (console stays Node.js per
  README). Chosen over TypeScript: single-binary deploy fits self-hosted
  positioning; goroutine-per-session actor model maps cleanly; pgx/sqlc is
  the least-magic data stack.
- 2026-08-30: sqlc over an ORM — the schema IS the product (ADR-003); SQL
  should be visible, not generated. net/http ServeMux over chi — one less
  dependency, patterns cover our routing.
- 2026-08-30: Agent delete policy = 409 with live sessions (no cascade) —
  deletion is a governance act, not an accident.
- 2026-08-30: ADR-004 landed (harness-agnostic runtime; six primitives).
  M1 unaffected — the CRUD/schema layer has no harness coupling; `sessions`
  gains a nullable `harness` column (default `native`) in the 0001 migration
  so no retrofit migration is needed later.

## Progress log

- 2026-08-30: plan created. No code yet.
