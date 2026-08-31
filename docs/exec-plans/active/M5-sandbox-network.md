# M5 — sandbox network policy + escape suite + E2B backend

Goal (roadmap): E2B sandbox backend + network policy enforcement.
Done when: **the sandbox escape test suite passes** — an adversarial
corpus run against each provider, with expected outcomes driven by what
that provider actually guarantees (ADR-001's honest-tiering applied to
testing itself).

## What ships

1. **Network policy in the sandbox contract** (ADR-001's "egress is the
   floor that applies to all"):
   - `sandbox.Policy{Egress: EgressAllow | EgressNone}` — v0 is the two
     modes an enterprise review asks about first ("can it phone home?");
     domain allowlists are a post-M5 additive extension of the same type.
   - docker provider: `--network none` unless egress is explicitly
     allowed — real kernel-level enforcement, testable.
   - exec provider: CANNOT enforce (no root in the dev sandbox, no
     namespaces). It keeps reporting zero isolation; the escape suite
     encodes that honestly instead of pretending.
   - Process-level default from `SANDBOX_EGRESS` (default: none for
     providers that can enforce it). Per-agent sandbox policy lands with
     the config surface when a real user needs it — not before.
2. **The escape suite** (`internal/sandbox/escape_test.go`): one
   adversarial corpus, per-provider expected outcomes:
   - tool-path traversal (must fail on ALL providers — our guard, not
     the kernel's)
   - output flood → capped everywhere
   - host-file read via bash (`cat /etc/passwd`-shaped): blocked by
     docker (container root, not host root); allowed by exec (documented
     dev tier — the assertion EXPECTS the read to succeed, so a future
     "fix" that silently changes exec semantics turns CI red)
   - network egress (`curl`/DNS): blocked by docker under EgressNone,
     allowed under EgressAllow; allowed by exec (documented)
   - docker tier runs where a daemon exists: `AGENTD_DOCKER=1` gate,
     enabled in CI (ubuntu runners ship a working Docker)
3. **E2B provider** (`internal/sandbox/e2b.go`) — ADR-001's production
   tier: a minimal client for E2B's documented API (create sandbox from
   template, exec, file write/read, kill), wire contract pinned by a
   fake server, **experimental until live-validated** — same discipline
   as the M4 OpenCode adapter. Config: `SANDBOX_PROVIDER=e2b`,
   `E2B_API_KEY`, `E2B_TEMPLATE`. If live validation finds wire drift,
   the fix is one file.

## Explicitly deferred

- domain-level egress allowlists (extends Policy; needs a DNS/iptables
  story per backend)
- gVisor/Kata providers (ADR-001's escape hatches; the interface stays
  the cap on exposure)
- live E2B validation (environment-gated; experimental until then)
- per-agent-version sandbox policy in the agent config

## Decision log

- 2026-08-31: the escape suite asserts the DEV tier's weaknesses as
  EXPECTED behavior — a test that documents "exec allows host reads"
  is worth more than omitting the test; silent semantic drift becomes a
  red build instead of a surprise.
- 2026-08-31: E2B client is hand-rolled against the documented REST
  surface instead of the e2b-go SDK: the SDK owns transport + auth and
  cannot be pointed at a fake for the wire-contract tests; one localized
  file absorbs any drift found at live-validation time.

## Progress log

- 2026-08-31: plan created. No code yet.
