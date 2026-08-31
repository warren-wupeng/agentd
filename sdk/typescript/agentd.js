// agentd TypeScript/JavaScript SDK — zero dependencies on purpose.
// A thin, readable client over the public REST + SSE surface; that IS
// the SDK story at this stage (M6). Node 18+ (global fetch).
//
//   const client = new AgentdClient("http://localhost:8080");
//   const { agent } = await client.createAgent("coder", { model: "google/gemini-3.5-flash" });
//   const { session } = await client.createSession(agent.id);
//   await client.postMessage(session.id, "write a haiku to haiku.txt and cat it");
//   for await (const frame of client.streamEvents(session.id)) { ... }

/** @typedef {{id: string, name: string, description: string}} Agent */
/** @typedef {{id: string, agent_id: string, agent_version: number, harness: string, state: string, stop_reason: string|null}} Session */
/** @typedef {{seq: number, type: string, actor: string, payload: object}} LogEvent */

class AgentdError extends Error {
  /**
   * @param {number} status
   * @param {{code: string, message: string, remediation: string}} detail
   */
  constructor(status, detail) {
    super(`${detail.code}: ${detail.message} — ${detail.remediation}`);
    this.status = status;
    this.detail = detail;
  }
}

class AgentdClient {
  /**
   * @param {string} baseUrl e.g. http://localhost:8080
   */
  constructor(baseUrl) {
    this.baseUrl = baseUrl.replace(/\/$/, "");
  }

  /**
   * @param {string} path
   * @param {object} [body]
   * @param {object} [init]
   * @returns {Promise<{status: number, json: object}>}
   * @private
   */
  async _req(method, path, body, init = {}) {
    const resp = await fetch(this.baseUrl + path, {
      method,
      headers: body !== undefined ? { "Content-Type": "application/json" } : {},
      body: body !== undefined ? JSON.stringify(body) : undefined,
      ...init,
    });
    const text = await resp.text();
    const json = text ? JSON.parse(text) : {};
    if (!resp.ok && json.error) throw new AgentdError(resp.status, json.error);
    return { status: resp.status, json };
  }

  /**
   * Create an agent (version 1).
   * @param {string} name
   * @param {{model: string, system_prompt?: string, tools?: string[]}} config
   * @param {string} [description]
   * @returns {Promise<{agent: Agent, version: object}>}
   */
  createAgent(name, config, description = "") {
    return this._req("POST", "/v1/agents", { name, description, config })
      .then((r) => r.json);
  }

  /**
   * Append a new immutable agent version (config snapshot).
   * @param {string} agentId
   * @param {object} config
   * @returns {Promise<{version: object}>}
   */
  updateAgent(agentId, config) {
    return this._req("PUT", `/v1/agents/${agentId}`, { config }).then((r) => r.json);
  }

  /**
   * @param {string} [agentId] pin to an agent; omit to list sessions
   * @returns {Promise<{sessions: Session[]}>}
   */
  listSessions(agentId) {
    const q = agentId ? `?agent_id=${agentId}` : "";
    return this._req("GET", `/v1/sessions${q}`).then((r) => r.json);
  }

  /**
   * @param {string} agentId
   * @param {number} [version] 0 = latest
   * @param {string} [harness] "native" (default) or a registered external harness
   * @returns {Promise<{session: Session}>}
   */
  createSession(agentId, version = 0, harness = "native") {
    return this._req("POST", "/v1/sessions", {
      agent_id: agentId, agent_version: version, harness,
    }).then((r) => r.json);
  }

  /**
   * Post a user message; the session's actor is kicked automatically.
   * @param {string} sessionId
   * @param {string} text
   * @returns {Promise<{event: LogEvent}>}
   */
  postMessage(sessionId, text) {
    return this._req("POST", `/v1/sessions/${sessionId}/events`, {
      payload: { content: [{ type: "text", text }] },
    }).then((r) => r.json);
  }

  /**
   * Replay the durable log after a cursor.
   * @param {string} sessionId
   * @param {number} [afterSeq]
   * @param {number} [limit]
   * @returns {Promise<{events: LogEvent[], next_after_seq: number|null}>}
   */
  listEvents(sessionId, afterSeq = 0, limit = 100) {
    return this._req("GET", `/v1/sessions/${sessionId}/events?after_seq=${afterSeq}&limit=${limit}`)
      .then((r) => r.json);
  }

  /**
   * Live tail over SSE: replays durable log frames (id = seq), then
   * streams log frames and ephemeral model deltas until the reader
   * stops. Reconnect = call again with the last received seq.
   *
   * @param {string} sessionId
   * @param {number} [afterSeq]
   * @param {AbortSignal} [signal]
   * @returns {AsyncGenerator<{kind: "log"|"delta", seq?: number, data: object}>}
   */
  async *streamEvents(sessionId, afterSeq = 0, signal) {
    const resp = await fetch(
      `${this.baseUrl}/v1/sessions/${sessionId}/stream?after_seq=${afterSeq}`,
      { headers: { Accept: "text/event-stream" }, signal },
    );
    if (!resp.ok || !resp.body) throw new Error(`stream: ${resp.status}`);
    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    for (;;) {
      const { done, value } = await reader.read();
      if (done) return;
      buf += decoder.decode(value, { stream: true });
      let idx;
      while ((idx = buf.indexOf("\n\n")) >= 0) {
        const frame = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        let event = "message", id = undefined, data = "";
        for (const line of frame.split("\n")) {
          if (line.startsWith("event: ")) event = line.slice(7);
          else if (line.startsWith("id: ")) id = Number(line.slice(4));
          else if (line.startsWith("data: ")) data += line.slice(6);
        }
        if (event === "log") {
          yield { kind: "log", seq: id, data: JSON.parse(data) };
        } else if (event === "delta") {
          yield { kind: "delta", data: JSON.parse(data) };
        }
      }
    }
  }

  /**
   * Wait until the session parks (idle/terminated), polling gently.
   * @param {string} sessionId
   * @param {{intervalMs?: number, timeoutMs?: number}} [opts]
   * @returns {Promise<Session>}
   */
  async waitForIdle(sessionId, opts = {}) {
    const interval = opts.intervalMs ?? 500;
    const deadline = Date.now() + (opts.timeoutMs ?? 120_000);
    for (;;) {
      const { session } = await this._req("GET", `/v1/sessions/${sessionId}`).then((r) => r.json);
      if (session.state === "idle" || session.state === "terminated") return session;
      if (Date.now() > deadline) throw new Error(`timeout waiting for idle (state=${session.state})`);
      await new Promise((r) => setTimeout(r, interval));
    }
  }
}

module.exports = { AgentdClient, AgentdError };
