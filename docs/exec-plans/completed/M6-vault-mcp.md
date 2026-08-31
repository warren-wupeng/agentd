# M6 — vault + MCP credential proxy + minimal SDKs

Goal (roadmap): an agent calls an MCP server with **zero secrets in the
sandbox**. This is the moat story's first load-bearing wall (README:
credential injection is agent-specific semantics generic orchestrators
lack); M4/M5 set event normalization and sandbox governance, M6 sets
credentials.

## Design in one paragraph

Secrets live in Postgres, encrypted with AES-256-GCM under
`VAULT_MASTER_KEY` (envelope-style: the master key never touches the
DB). Values are write-only through the API: PUT stores, GET lists NAMES
ONLY, nothing ever echoes a value. When a session needs an MCP server,
there are two paths and BOTH keep the sandbox clean: the **native
`mcp` tool** runs in the control plane — the model asks for
`mcp_call{server, path}`, agentd injects the credential into the
outbound request and returns the response as tool data (the sandbox
sees literally nothing, not even a token); external harness workers use
the **session proxy** `POST /v1/sessions/{id}/mcp/{server}` with a
derived session token (HMAC(master, sessionID) — no storage, dies on
key rotation, revocable by terminating the session). The token is a
scoped, short-lived, low-value derived credential — the design intent
of "zero secrets" is zero long-lived provider credentials in any
sandbox; the native path holds the stronger line (zero everything).

## What ships

1. **Migration 0002**: `vault_secrets (name unique, ciphertext, nonce,
   created_at, updated_at)` + `mcp_servers (name unique, base_url,
   secret_name, auth_header, created_at)`.
2. **`internal/vault`** (G2: secrets only here): AES-GCM seal/open,
   Put/Get/Delete/ListNames, session-token issue/verify (HMAC-SHA256).
   Own pgxpool — connection-level separation from the event store.
3. **`internal/mcp`**: server registry CRUD + `Proxy.Call` — outbound
   HTTP with the credential injected from the vault, 10s timeout,
   256KB response cap, upstream status passed through as data.
4. **Native `mcp` tool** (tools registry, `WithMCP` option): the
   model-visible surface; every call is a normal tool.requested/
   completed event (audit) and the credential never enters the sandbox.
5. **API**: `PUT/GET/DELETE /v1/vault/secrets` (values write-only),
   `POST/GET /v1/mcp/servers`, and the session proxy endpoint
   (X-Session-Token; terminated sessions rejected).
6. **SDKs** (`sdk/typescript`, `sdk/python`): zero-dependency minimal
   clients — createAgent / createSession / postMessage / streamEvents
   (SSE). Plain JS with JSDoc types and stdlib Python: syntax-checked
   in CI (`node --check`, `py_compile`), no toolchain sprawl.

## Tests

- vault: seal/open roundtrip, wrong-key failure, list shows names not
  values, token verify (+tamper rejection), delete
- mcp: registry CRUD; proxy injects the credential (fake upstream
  asserts it), missing secret → agentderr with remediation, upstream
  500 → data not crash, response cap
- tools: mcp tool happy path + error-as-data
- api: vault value never echoed; proxy endpoint accepts a valid session
  token, rejects garbage and terminated sessions
- **done-when (the milestone criterion, mechanical)**: fake MCP
  upstream requires `Authorization: Bearer <real-secret>`; vault holds
  it; a scripted model calls the mcp tool through harness.Run; the
  upstream PROVABLY received the real credential, and the real secret
  appears NOWHERE — not in any event payload, not in the sandbox
  workdir, not in tool outputs.

## Explicitly deferred

- MCP streaming transports (SSE/stdio MCP protocols) — v0 proxies
  plain HTTP request/response; the MCP wire protocols land when a real
  server needs them, behind the same registry
- per-session credential scoping policies (which agent may use which
  secret) — the registry row is the single seam to add it
- external-harness worker integration for the session token path
  (OpenCode workers fetching through the proxy) — post-M6
- secret rotation/versioning; envelope encryption with per-secret DEKs
  (the single master key is documented honestly as the M6 ceiling)
- SDK publishing (npm/PyPI) — repo-local until someone installs them

## Decision log

- 2026-08-31: native tool over proxy-only: the strongest version of the
  claim ("zero" means zero) is testable today in-process; the proxy
  exists for external workers, not as the primary path.
- 2026-08-31: session tokens are derived (HMAC), not stored — no token
  table, no revocation race: rotate the master key or terminate the
  session and they are gone.
- 2026-08-31: SDKs are dependency-free on purpose — a thin readable
  client over the public REST/SSE surface IS the SDK story at this
  stage; frameworks come with users.

## Progress log

- 2026-08-31: plan created. No code yet.
- 2026-08-31: **M6 done.** vault (AES-256-GCM, write-only values,
  derived session tokens, own pool), mcp registry + credential-injecting
  proxy (no redirects, capped responses), native `mcp` tool
  (control-plane execution), API endpoints (vault CRUD write-only,
  registry, session proxy with token enforcement + terminated-session
  rejection), migration 0002, and the two zero-dependency SDKs
  (node --check + py_compile in CI). All suites green, linters clean.
- 2026-08-31: **Done-when, mechanically**:
  TestDoneWhen_MCPCallWithZeroSecretsInSandbox — a scripted agent calls
  an upstream that rejects unauthenticated requests; the upstream
  provably received the vault credential; grep across every event
  payload and every file under the sandbox finds ZERO occurrences of
  the secret; the session parks end_turn with the upstream data as the
  tool result. The claim holds in its strongest form: the native path
  holds zero secrets AND zero tokens in the sandbox.
