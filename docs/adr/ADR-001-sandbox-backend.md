# ADR-001: Sandbox backend — E2B (Firecracker) for production, Docker for dev

- **Status:** Accepted
- **Date:** 2026-08-30

## Context

agentd sessions execute untrusted, LLM-generated code: arbitrary bash, package installs, file writes. The threat model is not "the user is malicious" — it's "the model can be prompt-injected or simply wrong," so **all generated code is treated as hostile**.

Isolation options form a spectrum:

| Approach | Isolation | Cold start | Ops burden |
|---|---|---|---|
| Plain Docker | Shared kernel — namespace escapes are a when-not-if for hostile code | ~100ms | Low |
| Docker + gVisor | Userspace kernel intercepts syscalls | ~100ms | Medium |
| Kata containers | VM boundary, full guest kernel | seconds | Medium-high |
| Firecracker microVM | Dedicated kernel per VM, minimal device model | ~125ms | **High — this is an infra product** |

Cold start matters because sandbox provisioning sits on the session's critical path: the user is staring at a spinner while it happens.

The other half of the context: building a Firecracker fleet — image baking, pooling, scheduling, snapshot/restore, networking — is a standalone infrastructure product with its own roadmap, not a feature of ours.

## Decision

Define a `SandboxProvider` interface and ship two reference implementations:

- **`docker`** — development and trusted-ish workloads. Fast, zero external deps.
- **`e2b`** — production. Firecracker microVMs, Apache-2.0, self-hostable, mature SDKs.

We do **not** build our own microVM fleet orchestration.

## Rationale

- **Security story must be honest from day one.** "We run hostile code in plain Docker" fails any enterprise review; the JD-level question "what's your isolation model?" needs a real answer.
- **E2B already paid the Firecracker tuition** and licensed it permissively. Rebuilding it would delay M1 by months for zero differentiation — sandboxing is someone else's hard problem *on purpose* (Design principle 4).
- **The interface caps our exposure.** If E2B's license, pricing, or self-hosting story degrades, swapping providers is a one-package change, not a rewrite.

## Consequences

**Positive:** production-grade isolation from the first deployment; dev loop stays fast (Docker); the interface documents exactly what a sandbox must provide (exec, fs, network policy, lifecycle).

**Negative:** self-hosting E2B means operating *their* control plane — non-trivial (Nomad, Firecracker hosts, image registry). Managed E2B has session limits (24h) that long-running sessions must handle via checkpoint/resume. Mitigation: `docker` provider for dev/local, and the provider interface keeps Kata/gVisor/BYOC implementations possible later.

**Revisit if:** E2B changes license; our scale makes a self-owned fleet a cost differentiator; or a provider appears with materially better snapshot/restore semantics.

## Alternatives considered

- **Plain Docker everywhere** — rejected: shared-kernel isolation is insufficient for hostile code, full stop.
- **gVisor-only** — a respectable middle ground, but userspace-kernel syscall gaps break arbitrary code in surprising ways, and the ops burden stays ours.
- **Kata** — real VM boundary but heavier and slower to cold-start; no mature managed control plane to lean on.
- **Self-built Firecracker fleet** — rejected: months of infra work before a single session runs. We'd be building E2B instead of agentd.
