"""agentd Python SDK — stdlib only, on purpose (M6).

A thin, readable client over the public REST + SSE surface:

    from agentd import AgentdClient
    client = AgentdClient("http://localhost:8080")
    agent = client.create_agent("coder", {"model": "google/gemini-3.5-flash"})["agent"]
    session = client.create_session(agent["id"])["session"]
    client.post_message(session["id"], "write a haiku to haiku.txt and cat it")
    client.wait_for_idle(session["id"])
    events = client.list_events(session["id"])["events"]

The SSE stream helper (stream_events) is a generator over log frames;
reconnect by calling again with the last received seq.
"""

import json
import time
import urllib.error
import urllib.request


class AgentdError(Exception):
    """Carries the API's structured error: code, message, remediation."""

    def __init__(self, status, detail):
        super().__init__(
            "%s: %s — %s" % (detail.get("code"), detail.get("message"), detail.get("remediation"))
        )
        self.status = status
        self.detail = detail


class AgentdClient:
    def __init__(self, base_url):
        self.base_url = base_url.rstrip("/")

    def _req(self, method, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(
            self.base_url + path,
            data=data,
            method=method,
            headers={"Content-Type": "application/json"} if data else {},
        )
        try:
            with urllib.request.urlopen(req, timeout=60) as resp:
                raw = resp.read().decode()
                return json.loads(raw) if raw else {}
        except urllib.error.HTTPError as e:
            raw = e.read().decode()
            detail = json.loads(raw).get("error", {}) if raw else {}
            raise AgentdError(e.code, detail) from None

    # --- agents ---

    def create_agent(self, name, config, description=""):
        return self._req("POST", "/v1/agents",
                         {"name": name, "description": description, "config": config})

    def update_agent(self, agent_id, config):
        """Appends a new immutable version; old versions stay pinned."""
        return self._req("PUT", "/v1/agents/%s" % agent_id, {"config": config})

    # --- sessions ---

    def create_session(self, agent_id, version=0, harness="native"):
        return self._req("POST", "/v1/sessions", {
            "agent_id": agent_id, "agent_version": version, "harness": harness,
        })

    def get_session(self, session_id):
        return self._req("GET", "/v1/sessions/%s" % session_id)

    def post_message(self, session_id, text):
        """Appends a user message; the session's actor is kicked."""
        return self._req("POST", "/v1/sessions/%s/events" % session_id, {
            "payload": {"content": [{"type": "text", "text": text}]},
        })

    def list_events(self, session_id, after_seq=0, limit=100):
        return self._req(
            "GET",
            "/v1/sessions/%s/events?after_seq=%d&limit=%d" % (session_id, after_seq, limit),
        )

    def wait_for_idle(self, session_id, interval=0.5, timeout=120.0):
        deadline = time.monotonic() + timeout
        while True:
            session = self.get_session(session_id)["session"]
            if session["state"] in ("idle", "terminated"):
                return session
            if time.monotonic() > deadline:
                raise TimeoutError("waiting for idle (state=%s)" % session["state"])
            time.sleep(interval)

    # --- streaming (SSE) ---

    def stream_events(self, session_id, after_seq=0, timeout=300):
        """Yields ("log", seq, event_dict) and ("delta", None, delta_dict).

        Reconnect = call again with the last log seq you received.
        """
        url = "%s/v1/sessions/%s/stream?after_seq=%d" % (self.base_url, session_id, after_seq)
        req = urllib.request.Request(url, headers={"Accept": "text/event-stream"})
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            event, event_id, data = None, None, ""
            for raw_line in resp:
                line = raw_line.decode().rstrip("\n")
                if line == "":
                    if event == "log" and data:
                        yield ("log", event_id, json.loads(data))
                    elif event == "delta" and data:
                        yield ("delta", None, json.loads(data))
                    event, event_id, data = None, None, ""
                    continue
                if line.startswith("event: "):
                    event = line[7:]
                elif line.startswith("id: "):
                    event_id = int(line[4:])
                elif line.startswith("data: "):
                    data += line[6:]
