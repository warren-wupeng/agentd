# agentd

**Self-hosted managed agents platform.** Versioned agent configs, durable session orchestration, and sandboxed tool execution — the open-source answer to Anthropic's Managed Agents.

> **Status: design stage (pre-alpha).** This README is the spec. Code lands milestone by milestone — see [Roadmap](#roadmap). Watch the repo or open an issue to follow along.

---

## Why this exists

Anthropic's Managed Agents showed what a production agent platform looks like: persisted, versioned agent configs; a per-session sandbox container where tools execute; an event-sourced session protocol; credentials that never touch the sandbox. It's also closed-source, single-vendor, and hosted-only.

The pieces to build your own already exist — [E2B](https://github.com/e2b-dev/E2B) for Firecracker sandboxing, [OpenHands](https://github.com/All-Hands-AI/OpenHands) for agent-runtime patterns, durable-execution engines for scheduling. What's missing is the layer that ties them together with the right semantics: **an open control plane**.

agentd is that control plane.

## The model

Four primitives:

| Primitive | What it is |
|---|---|
| **Agent** | Persisted, *versioned* config: model, system prompt, tools, MCP servers, skills. Updates create immutable versions; sessions pin to one. |
| **Session** | One stateful run of an agent. An append-only **event stream**, not a chat log. |
| **Environment** | Template for provisioning tool-execution sandboxes: network policy (`unrestricted` / `allowed_hosts`), packages, isolation backend. |
| **Sandbox** | The per-session isolated container where *tools* execute (bash, files, code). The agent loop never runs here. |

The key separation: **the agent loop runs in the orchestrator; only tool execution runs in the sandbox.** Intelligence on the control plane, hands and feet in the sandbox.

```
                 ┌──────────────────────────────────────────────┐
 Agent (config) ─▶│  Orchestrator (agent loop)                   │
                 │  model calls · tool routing · compaction     │
                 └──────────────────┬───────────────────────────┘
                                    │ tool calls
                                    ▼
 Environment (template) ─▶  Sandbox pool (E2B / Docker+gVisor)
                                    │
                 Session ───────────┤
                                    ├── Resources (files, git repos, memory)
                                    ├── Vault refs (credentials injected by
                                    │    control-plane proxy — never enter sandbox)
                                    └── Event stream (SSE out, events in)
```

## Design principles

1. **Agents are config; sessions are events.** Config is versioned and cheap to diff. Everything that happens in a run is an event in an append-only log. SSE is just a tail of that log — reconnect means replay + dedupe by event id, so a dropped client never deadlocks a session.
2. **The loop is replaceable.** The default orchestrator loop speaks the Anthropic API, but sits behind a `ModelProvider` interface. Bring Claude, GPT, or a local model.
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
6. **Sub-agents.** Main agent + sub-agent orchestration: context-isolated threads, shared sandbox filesystem, explicit cross-thread messages. Threads are just namespaced event streams, so the design survives contact with the event model; implementation is post-v1.

## Non-goals (v1)

- Sub-agent threads (designed — see Platform capabilities; implementation is post-v1)
- Rubric-graded outcome *loops* (Eval covers the scoring half; the iterate-until-satisfied loop is post-v1)
- A hosted SaaS — agentd is software *you* run
- Wire-compatibility with Anthropic's API. We borrow the shapes that work; we don't clone the wire format.

## Roadmap

| Milestone | Scope | Done when |
|---|---|---|
| **M1** | Postgres schema; agents/sessions/events CRUD; agent versioning | API tests pass |
| **M2** | Orchestrator loop; bash/read/write/edit tools; single-Docker sandbox | One session runs read → edit → exec end-to-end |
| **M3** | SSE event stream + history replay + `processed_at` + idle/`stop_reason` state machine | `kill -9` the client mid-run, reconnect, nothing lost |
| **M4** | E2B sandbox backend + network policy enforcement | Sandbox escape test suite passes |
| **M5** | Vault + MCP credential proxy + minimal TS/Python SDK | Agent calls an MCP server with zero secrets in the sandbox |
| **M6** | Eval harness v0: trace → dataset → rubric scorer → version-compare report | Two agent versions scored on the same dataset, diff printed |
| **M7** | Sample agent (RAG code-review bot) + web console (Node.js) | Demo: question in, cited review out |

Explicitly later: sub-agent threads, outcome loops, memory stores, webhooks — gated on what the first real users actually hit, in that order.

## Planned layout

```
cmd/agentd-server/     # control-plane API
internal/orchestrator/ # agent loop, session state machine
internal/sandbox/      # e2b + docker SandboxProvider implementations
internal/vault/        # credential store + injection proxy
internal/governance/   # tenancy, RBAC, budgets, audit log
internal/policy/       # guardrail engine (tool / content / network)
internal/eval/         # datasets, scorers, version regression gate
internal/telemetry/    # OTel tracing
sdk/ts/  sdk/python/   # client SDKs (stream-first by default)
console/               # web console (Node.js BFF + React)
deploy/compose/  deploy/helm/
docs/adr/              # architecture decision records
```

## Contributing

At design stage the most valuable contribution is critique: open an issue if a design decision above looks wrong, under-specified, or like it's solving a problem nobody has. Settled decisions live in [`docs/adr/`](./docs/adr/) — read them before proposing a competing design. Code contributions open up at M1.

## License

[Apache 2.0](./LICENSE). Chosen over copyleft so agentd can be embedded in commercial products without friction; patent grant included.

## Acknowledgments

Design points unapologetically borrowed from the public docs of Anthropic Managed Agents (the four-primitive model, event-sourced sessions, vault-proxy credentials). Built on the shoulders of [OpenHands](https://github.com/All-Hands-AI/OpenHands) (MIT) and [E2B](https://github.com/e2b-dev/E2B) (Apache 2.0). Related prior art: [open-managed-agents](https://github.com/rogeriochaves/open-managed-agents) (AGPL).
