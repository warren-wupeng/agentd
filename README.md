# agentd

**Self-hosted, harness-agnostic managed agents platform.** Versioned agent configs, durable session orchestration, sandboxed execution — the open control plane for agent workers running *any* harness: our native loop, OpenCode, Claude Code, or yours.

> **Status: design stage (pre-alpha).** This README is the spec. Code lands milestone by milestone — see [Roadmap](#roadmap). Watch the repo or open an issue to follow along.

---

## Why this exists

Anthropic's Managed Agents showed what a production agent platform looks like: persisted, versioned agent configs; a per-session sandbox container where tools execute; an event-sourced session protocol; credentials that never touch the sandbox. It's also closed-source, single-vendor, and hosted-only.

The pieces to build your own already exist — [E2B](https://github.com/e2b-dev/E2B) for Firecracker sandboxing, [OpenHands](https://github.com/All-Hands-AI/OpenHands) for agent-runtime patterns, durable-execution engines for scheduling. What's missing is the layer that ties them together with the right semantics: **an open control plane**.

Meanwhile the agent loop itself is commoditizing: OpenAI open-sourced the Codex harness, and every lab ships a CLI agent (Claude Code, deepseek-harness, OpenCode, pi, …). The durable layer is *above* the loop — orchestration, governance, and state that work for **any** harness.

agentd is that control plane.

## The model

Six primitives:

| Primitive | What it is |
|---|---|
| **Agent** | Persisted, *versioned* config: model, system prompt, tools, MCP servers, skills. Updates create immutable versions; sessions pin to one. |
| **Harness** | Pluggable agent runtime (ADR-004): our `native` loop, or an external CLI harness (OpenCode, Claude Code, …) driven through an adapter that normalizes its protocol into the session event log. Fixed per worker at creation, with declared capability bits. |
| **Session** | One stateful run of an agent on a harness. An append-only **event stream**, not a chat log. |
| **Environment** | Template for provisioning workers: network policy (`unrestricted` / `allowed_hosts`), packages, isolation backend. |
| **Sandbox** | The per-session isolated worker where the harness and its tools execute. Disposable and rebuildable from session checkpoint + workspace volume. |
| **Workflow** | Versioned **DAG definition**: nodes are tasks bound to an agent + harness, edges are dependencies, with parallel fan-out/join and retry policy. (Post-v1 implementation; seam designed now.) |

The key separation: **the control plane never depends on any one loop.** The native loop runs in the orchestrator; external harnesses run inside the worker — both normalize to the same event log. Sandboxes stay disposable: destroy a worker, remount its workspace, resume from checkpoint.

```
Workflow (DAG) ──┐
Agent (config) ──┼─▶ Control plane: session supervision · DAG scheduling
                 │    event normalization · budgets · audit
                 │         │ Harness interface (ADR-004)
                 │         ├─▶ native loop (in orchestrator — reference impl)
                 │         └─▶ external harness per worker: OpenCode · Claude Code · …
                 │
Environment ─────┼─▶ Sandbox pool (E2B / Docker) — one worker per session:
                 │    harness + model + skills + checkpoint + workspace mount
Session ─────────┘    ├── Resources (files, git repos)
                      ├── Vault refs (proxy-injected — never enter sandbox)
                      └── Events (normalized → append-only log → SSE out)
```

## Design principles

1. **Agents are config; sessions are events.** Config is versioned and cheap to diff. Everything that happens in a run is an event in an append-only log. SSE is just a tail of that log — reconnect means replay + dedupe by event id, so a dropped client never deadlocks a session.
2. **The runtime is replaceable.** The default loop speaks the Anthropic API behind a `ModelProvider` interface — and the loop itself sits behind a `Harness` interface (ADR-004): Claude Code, OpenCode, or your own runtime can execute a session. Bring any model, any harness.
3. **Credentials never enter the sandbox.** MCP and git calls route through a control-plane proxy that injects tokens from the vault (AES-256-GCM at rest, write-only over the API). If agent-written code inside the sandbox can read your secret, the design has failed.
4. **Sandboxes are someone else's hard problem — on purpose.** We don't reinvent microVM orchestration. E2B (Firecracker) for production isolation, Docker+gVisor for cheap dev, your own infra via the `SandboxProvider` interface.
5. **Durable by default.** Sessions survive orchestrator restarts: `rescheduling → running ↔ idle → terminated`, with explicit `stop_reason` semantics (`requires_action` vs `end_turn` vs `retries_exhausted`) so clients can tell "waiting on you" from "done".
6. **Boring technology.** Postgres for the event store, SSE for streaming, containers for isolation. No custom consensus, no new protocols.

## Platform capabilities (designed, not yet built)

The core loop gets you a working agent. Running agents *inside a company* takes a second layer: tenancy, observability, evaluation, guardrails, interop. These are designed to the same depth as the core and land after M5, in roughly this order.

1. **Governance.** Multi-tenancy (`org → workspace → project`), RBAC (`admin` / `member` / `viewer`), per-tenant model-provider access control, and budgets (RPM + monthly token quota, enforced in the orchestrator, not the SDK). The audit log is append-only — just another event table, same machinery as sessions.
2. **Observability, OTel-native.** Every session is one OpenTelemetry trace; `model_request`, `tool_call`, and `sandbox_exec` are spans, following the GenAI semantic conventions. The event log and the trace are two views of the same truth — session IDs double as trace IDs.
3. **Agent Eval.** Traces become datasets; datasets get scored by rubric or LLM-judge; scorers run in CI as a **regression gate between agent versions**. "Why is v3 better than v2?" should produce a report, not a vibe. This is the payoff for versioning agents in the first place.
4. **Guardrails.** One policy engine, three layers: tool-level (`allow` / `ask` / `deny` per tool, per tenant), content-level (input/output guardrail hooks on the model path), network-level (sandbox egress policy from the Environment). Deny decisions are events too — auditable by construction.
5. **Protocols.** MCP for tools in. A2A and ACP for agents in *and* out: an agentd agent can be exposed as an A2A/ACP endpoint, and can consume an external A2A agent as a tool. One adapter layer, both directions.
6. **Sub-agents.** Main agent + sub-agent orchestration as **Workflow DAG nodes**: context-isolated workers (each with its own harness, quota, audit trail, and retry policy), shared workspace mounts, explicit cross-node artifacts. Cleaner governance than in-loop threads — a sub-agent is just another session the DAG schedules. Lands with the Workflow primitive, post-v1.

## Non-goals (v1)

- A general-purpose DAG/workflow engine — our Workflow is agent-semantics-only by design (ADR-004); Argo/Temporal already exist
- Rubric-graded outcome *loops* (Eval covers the scoring half; the iterate-until-satisfied loop is post-v1)
- A hosted SaaS — agentd is software *you* run
- Wire-compatibility with Anthropic's API. We borrow the shapes that work; we don't clone the wire format.

## Roadmap

| Milestone | Scope | Done when |
|---|---|---|
| **M1** | Postgres schema; agents/sessions/events CRUD; agent versioning | API tests pass |
| **M2** | Native agent loop (`harness: native`); bash/read/write/edit tools; single-Docker sandbox | One session runs read → edit → exec end-to-end |
| **M3** | SSE event stream + history replay + `processed_at` + idle/`stop_reason` state machine | `kill -9` the client mid-run, reconnect, nothing lost |
| **M4** | `Harness` adapter seam + first external adapter (OpenCode) + golden-transcript conformance suite | Same task driven by native and OpenCode produces the same normalized event stream |
| **M5** | E2B sandbox backend + network policy enforcement | Sandbox escape test suite passes |
| **M6** | Vault + MCP credential proxy + minimal TS/Python SDK | Agent calls an MCP server with zero secrets in the sandbox |
| **M7** | Eval harness v0: trace → dataset → rubric scorer → version-compare report | Two agent versions scored on the same dataset, diff printed |
| **M8** | Workflow DAG v0 + software-dev flow template (code → review → test → merge) + web console (Node.js) | Demo: spec in → parallel harness workers → merged artifact out |

Explicitly later: general-purpose workflow features, more adapters (deepseek-harness, pi, Codex), memory stores, webhooks — gated on what the first real users actually hit, in that order.

## Planned layout

Control plane in **Go**; console in Node.js + React; SDKs in TypeScript and Python. Agents working in this repo start at [`AGENTS.md`](./AGENTS.md).

```
cmd/agentd-server/     # control-plane API
internal/orchestrator/ # session supervision, state machine, DAG scheduler
internal/harness/      # Harness interface + adapters (native, opencode, ...) + conformance suite
internal/sandbox/      # e2b + docker SandboxProvider implementations
internal/vault/        # credential store + injection proxy
internal/governance/   # tenancy, RBAC, budgets, audit log
internal/policy/       # guardrail engine (tool / content / network)
internal/eval/         # datasets, scorers, version regression gate
internal/telemetry/    # OTel tracing
sdk/ts/  sdk/python/   # client SDKs (stream-first by default)
console/               # web console (Node.js BFF + React)
deploy/compose/  deploy/helm/
docs/                  # system of record: adr/, design/, research/, exec-plans/, GOLDEN.md
```

## Contributing

At design stage the most valuable contribution is critique: open an issue if a design decision above looks wrong, under-specified, or like it's solving a problem nobody has. Settled decisions live in [`docs/adr/`](./docs/adr/) — read them before proposing a competing design. Working designs live in [`docs/design/`](./docs/design/); how we build (agent-first, harness engineering) is in [`docs/design/harness-engineering.md`](./docs/design/harness-engineering.md). Code contributions open up at M1 — see [`docs/exec-plans/active/`](./docs/exec-plans/active/).

## License

[Apache 2.0](./LICENSE). Chosen over copyleft so agentd can be embedded in commercial products without friction; patent grant included.

## Acknowledgments

Design points unapologetically borrowed from the public docs of Anthropic Managed Agents (the four-primitive model, event-sourced sessions, vault-proxy credentials). Built on the shoulders of [OpenHands](https://github.com/All-Hands-AI/OpenHands) (MIT) and [E2B](https://github.com/e2b-dev/E2B) (Apache 2.0). The harness-agnostic runtime (ADR-004) is grounded in a comparative architecture study of [deepseek-harness, OpenCode, and pi](https://github.com/warren-wupeng/dsh-architecture) — three harnesses that independently converge on append-only session logs and isomorphic step loops. Related prior art: [open-managed-agents](https://github.com/rogeriochaves/open-managed-agents) (AGPL).
