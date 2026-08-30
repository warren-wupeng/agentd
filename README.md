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

## Non-goals (v1)

- Multi-agent coordinator threads
- Rubric-graded outcome loops
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

Explicitly after v1: multiagent threads, outcome grading, memory stores, webhooks.

## Planned layout

```
cmd/agentd-server/     # control-plane API
internal/orchestrator/ # agent loop, session state machine
internal/sandbox/      # e2b + docker SandboxProvider implementations
internal/vault/        # credential store + injection proxy
sdk/ts/  sdk/python/   # client SDKs (stream-first by default)
deploy/compose/  deploy/helm/
```

## Contributing

At design stage the most valuable contribution is critique: open an issue if a design decision above looks wrong, under-specified, or like it's solving a problem nobody has. Code contributions open up at M1.

## License

[Apache 2.0](./LICENSE). Chosen over copyleft so agentd can be embedded in commercial products without friction; patent grant included.

## Acknowledgments

Design points unapologetically borrowed from the public docs of Anthropic Managed Agents (the four-primitive model, event-sourced sessions, vault-proxy credentials). Built on the shoulders of [OpenHands](https://github.com/All-Hands-AI/OpenHands) (MIT) and [E2B](https://github.com/e2b-dev/E2B) (Apache 2.0). Related prior art: [open-managed-agents](https://github.com/rogeriochaves/open-managed-agents) (AGPL).
