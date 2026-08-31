# agentd SDKs

Minimal, dependency-free clients over the public REST + SSE surface.
No frameworks, no wrappers around wrappers — if you can read HTTP you
can read these.

- `typescript/agentd.js` — Node 18+ (global fetch). CommonJS.
- `python/agentd.py` — Python 3.8+, stdlib only.

Both cover the same surface: agents (immutable versioning), sessions
(harness selection), message posting with auto-kick, durable event
replay (`after_seq` cursor), the live SSE stream (log frames carry the
reconnect cursor; deltas are ephemeral), and `wait_for_idle`.

Publishing to npm/PyPI is deliberately deferred until someone installs
them (see the M6 exec plan).
