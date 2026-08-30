# M1 — control plane CRUD + agent versioning

Goal: Postgres schema and the agents/sessions/events API surface exist and are
tested. Done when: roadmap M1 criterion — API tests pass against a real
Postgres, including version immutability and event replay.

Stack: Go 1.24 (go directive 1.25.0 via deps), pgx/v5 (hand-written queries —
see decision log), golang-migrate, net/http ServeMux (1.22+ patterns — no
framework), log/slog. Boring on purpose.

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
3. **Store layer.** pgx queries + thin wrappers enforcing G1 (state
   transitions only via event-emitting tx functions).
   *Accepts:* store tests against a real Postgres; no exported raw UPDATE on
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
8. **CI.** GitHub Actions: `go test ./...` (tests boot their own embedded
   Postgres — no service container), golangci-lint (from source, pinned),
   go-arch-lint enforcing the `internal/` layering from `AGENTS.md`.
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
- 2026-08-30: sqlc → hand-written pgx. The store is 13 queries; codegen
  ceremony (sqlc.yaml, generated packages, CI step) buys nothing at this
  size. Scans are explicit, SQL stays in `.go` files next to the logic.
  Revisit if query count triples.
- 2026-08-30: tests use embedded-postgres (fergusstrange), not a compose
  service — the build sandbox has no root/Docker, and CI gets hermeticity
  for free. compose.yml stays for real `make dev-up` development.
  Consequences discovered the hard way (three debugging rounds):
  (1) parallel package binaries each need their OWN postgres: per-binary
      TestMain lifecycle + lock-claimed port from a reserved range;
  (2) the port lock must record the owner PID and never steal a live
      owner's lock — "port not yet listening" is indistinguishable from
      "crashed run" by probing alone;
  (3) embedded-postgres defaults to one shared runtime dir wiped at start,
      and health-checks over /tmp/.s.PGSQL.<port> — per-port RuntimePath +
      unix_socket_directories are mandatory for parallel packages.
- 2026-08-30: golangci-lint installed from source in CI (pinned v1.64.8).
  Prebuilt release binaries are compiled with older Go and refuse a go1.25
  target. go-arch-lint v1.18.0 pinned for the same reason.
- 2026-08-30: parallel-test port locks must be claimed with link(2), not
  O_CREATE|O_EXCL + write — the latter has an empty-content window between
  create and write that a parallel scanner reads as "stale lock" and
  steals. Two binaries then share a port AND a data dir → postmaster.pid
  FATAL. Observed on CI, not locally; loaded 2-core runners widen the
  window. Lock content = owner PID; a lock with a live owner is never
  stolen (a fresh claimant's postgres simply isn't listening yet, which
  port probing cannot distinguish from a crashed run).

## Progress log

- 2026-08-30: plan created. No code yet.
- 2026-08-30: tasks 1–7 implemented and green locally. Migration 0001
  (4 tables + agent_versions immutability trigger), store (G1: the only
  session-state mutation path is TransitionSession, tx with its
  state_changed event), full agents/sessions/events API on stdlib ServeMux,
  agentderr remediations on every error path. `go vet`, `golangci-lint`,
  `go-arch-lint` clean; `go test ./...` green twice consecutively
  (replay/immutability/state-machine/delete-guard suites). Two real bugs
  caught by tests: agentderr.Internal wrapping nil on success paths
  (rows.Err/tx.Commit returned as error unconditionally) — fixed at 5
  call sites. Task 8 (CI) pending push.
- 2026-08-30: **M1 done.** Pushed to main (0cd55d1 + cdd247e + cd553b4);
  CI green (test 28s, lint 32s). CI taught us two things local didn't:
  (1) repo1.maven.org rate-limits CI egress → actions/cache on
  ~/.embedded-postgres-go; (2) the link(2) lesson in the decision log —
  the empty-content window in O_EXCL lock creation is real on a loaded
  2-core runner.
