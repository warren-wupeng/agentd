// agentd console frontend — vanilla JS, no build step. Talks to the Go
// API through the console server's /api proxy (no CORS anywhere).
"use strict";

const $ = (sel) => document.querySelector(sel);
const api = {
  async req(method, path, body) {
    const resp = await fetch("/api" + path, {
      method,
      headers: body !== undefined ? { "Content-Type": "application/json" } : {},
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
    const json = await resp.json().catch(() => ({}));
    if (!resp.ok) throw new Error(json.error ? json.error.message : resp.status);
    return json;
  },
};

// --- tabs ---
for (const btn of document.querySelectorAll("nav button")) {
  btn.addEventListener("click", () => {
    document.querySelectorAll("nav button").forEach((b) => b.classList.toggle("active", b === btn));
    for (const tab of document.querySelectorAll(".tab")) {
      tab.hidden = tab.id !== "tab-" + btn.dataset.tab;
    }
  });
}

// --- agents ---
async function refreshAgents() {
  const { agents } = await api.req("GET", "/v1/agents?limit=50");
  const tbody = $("#agents-table tbody");
  tbody.innerHTML = "";
  for (const a of agents) {
    const tr = document.createElement("tr");
    tr.innerHTML = `<td>${a.name}</td><td class="mono">${a.id}</td><td>v${a.latest_version}</td>`;
    tbody.appendChild(tr);
  }
  // workflow agent picker too
  const picker = $("#wf-agent");
  picker.innerHTML = "";
  for (const a of agents) {
    const opt = document.createElement("option");
    opt.value = a.id;
    opt.textContent = `${a.name} (${a.id.slice(0, 8)})`;
    picker.appendChild(opt);
  }
}

$("#agent-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const f = e.target;
  try {
    await api.req("POST", "/v1/agents", {
      name: f.name.value,
      config: { model: f.model.value, system_prompt: f.system_prompt.value },
    });
    f.reset();
    await refreshAgents();
  } catch (err) {
    alert(err.message);
  }
});

// --- sessions + live stream ---
let activeSession = null;
let activeES = null;

async function refreshSessions() {
  const { sessions } = await api.req("GET", "/v1/sessions?limit=50");
  const sel = $("#session-select");
  sel.innerHTML = "";
  for (const s of sessions) {
    const opt = document.createElement("option");
    opt.value = s.id;
    opt.textContent = `${s.id.slice(0, 8)} ${s.harness} ${s.state}${s.stop_reason ? "/" + s.stop_reason : ""}`;
    sel.appendChild(opt);
  }
  if (activeSession && sessions.some((s) => s.id === activeSession)) {
    sel.value = activeSession;
  }
}

$("#session-new").addEventListener("click", async () => {
  const { agents } = await api.req("GET", "/v1/agents?limit=1");
  if (!agents.length) return alert("create an agent first");
  const { session } = await api.req("POST", "/v1/sessions", { agent_id: agents[0].id });
  activeSession = session.id;
  await refreshSessions();
  openStream(session.id);
});

$("#session-select").addEventListener("change", (e) => openStream(e.target.value));

$("#message-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  if (!activeSession) return;
  const text = e.target.text.value;
  e.target.reset();
  await api.req("POST", `/v1/sessions/${activeSession}/events`, {
    payload: { content: [{ type: "text", text }] },
  });
});

function logLine(cls, seq, type, text) {
  const div = document.createElement("div");
  div.className = "log-line " + cls;
  div.innerHTML = `<span class="seq">${seq || ""}</span> <span class="type">${type}</span> ${text || ""}`;
  const log = $("#session-log");
  log.appendChild(div);
  log.scrollTop = log.scrollHeight;
}

function openStream(sessionId) {
  activeSession = sessionId;
  if (activeES) activeES.close();
  $("#session-log").innerHTML = "";
  logLine("", "", "replay", `connecting to session ${sessionId.slice(0, 8)}…`);

  // replay history first (the durable part), then live tail
  api.req("GET", `/v1/sessions/${sessionId}/events?limit=200`).then(({ events }) => {
    for (const ev of events) renderEvent(ev);
    const es = new EventSource(`/api/v1/sessions/${sessionId}/stream?after_seq=${lastSeq(events)}`);
    activeES = es;
    es.addEventListener("log", (e) => renderEvent(JSON.parse(e.data)));
    es.addEventListener("delta", (e) => {
      const d = JSON.parse(e.data);
      logLine("delta", "", "delta", (d.text || "").slice(0, 120));
    });
  });
  pollSessionState(sessionId);
}

let lastSeq = (events) => (events.length ? events[events.length - 1].seq : 0);

function renderEvent(ev) {
  const cls = ev.type.startsWith("tool") ? "tool" : ev.type === "message.user" ? "user" : "";
  let text = "";
  try {
    const p = ev.payload || {};
    if (ev.type === "message.user" || ev.type === "message.assistant") {
      text = (p.content || []).map((b) => (b.text ? b.text.slice(0, 200) : `[${b.type}:${b.name || b.tool_use_id || ""}]`)).join(" ");
    } else if (ev.type === "tool.completed") {
      text = (p.output || "").slice(0, 160);
    } else if (ev.type === "turn.completed") {
      text = `stop_reason=${p.stop_reason}`;
    } else if (ev.type === "session.state_changed") {
      text = `${p.from} → ${p.to}`;
    } else {
      text = JSON.stringify(p).slice(0, 120);
    }
  } catch { /* payload oddities stay silent */ }
  logLine(cls, ev.seq, ev.type, text);
}

let stateTimer = null;
function pollSessionState(sessionId) {
  clearInterval(stateTimer);
  stateTimer = setInterval(async () => {
    if (activeSession !== sessionId) return clearInterval(stateTimer);
    try {
      const { session } = await api.req("GET", `/v1/sessions/${sessionId}`);
      $("#session-state").innerHTML =
        `state <b>${session.state}</b>` +
        (session.stop_reason ? ` stop_reason <b>${session.stop_reason}</b>` : "") +
        ` harness <b>${session.harness}</b>`;
      if (session.state === "idle" || session.state === "terminated") {
        clearInterval(stateTimer);
      }
    } catch { /* transient */ }
  }, 1500);
}

// --- workflow ---
const SOFTWARE_DEV = {
  name: "software-dev",
  nodes: [
    {
      id: "coder", agent: "AGENT_ID",
      prompt: "You are the coder in a software pipeline. Implement the following spec as one complete file named solution.py using the write_file tool. Then summarize your design in one paragraph.\n\nSPEC:\n{{spec}}",
      output_files: ["solution.py"],
    },
    {
      id: "reviewer", agent: "AGENT_ID", depends_on: ["coder"],
      prompt: "You are the code reviewer. Review this code for correctness and style. Name at most two concrete improvements, briefly.\n\nCODE:\n{{files.coder.solution.py}}",
    },
    {
      id: "tester", agent: "AGENT_ID", depends_on: ["coder"],
      prompt: "You are the test engineer. Write a minimal 3-bullet test plan for this code, then state PASS or FAIL with one sentence of rationale.\n\nCODE:\n{{files.coder.solution.py}}",
    },
    {
      id: "merger", agent: "AGENT_ID", depends_on: ["reviewer", "tester"],
      prompt: "You are the merger. Produce the final merged artifact: use the write_file tool to write MERGED.md containing (1) the final code, (2) each review note with incorporated/rejected + reason, (3) the test verdict. Then summarize in one sentence.\n\nORIGINAL CODE:\n{{files.coder.solution.py}}\n\nREVIEW:\n{{outputs.reviewer}}\n\nTEST VERDICT:\n{{outputs.tester}}",
    },
  ],
};

let wfTimer = null;
$("#wf-run").addEventListener("click", async () => {
  const agentId = $("#wf-agent").value;
  if (!agentId) return alert("create an agent first");
  const spec = $("#wf-spec").value || "demo spec";
  const def = JSON.parse(JSON.stringify(SOFTWARE_DEV));
  for (const n of def.nodes) {
    n.agent = agentId;
    n.prompt = n.prompt.replace("{{spec}}", spec);
  }
  const btn = $("#wf-run");
  btn.disabled = true;
  try {
    const { run } = await api.req("POST", "/v1/workflows", def);
    $("#wf-status").textContent = `run ${run.id.slice(0, 8)} started`;
    pollWorkflow(run.id);
  } catch (err) {
    $("#wf-status").innerHTML = `<span class="error">${err.message}</span>`;
    btn.disabled = false;
  }
});

function pollWorkflow(runId) {
  clearInterval(wfTimer);
  wfTimer = setInterval(async () => {
    let run;
    try {
      ({ run } = await api.req("GET", `/v1/workflows/${runId}`));
    } catch { return; }
    const board = $("#wf-nodes");
    board.innerHTML = "";
    for (const st of run.node_states) {
      const div = document.createElement("div");
      div.className = "node";
      div.innerHTML =
        `<span class="id">${st.id}</span>` +
        `<span class="status ${st.status}">${st.status}</span>` +
        `<span class="meta">${st.session_id ? "session " + st.session_id.slice(0, 8) : ""}${st.error ? " — " + st.error.slice(0, 100) : ""}</span>`;
      board.appendChild(div);
    }
    if (run.status !== "running") {
      clearInterval(wfTimer);
      $("#wf-run").disabled = false;
      $("#wf-status").innerHTML = `run <b>${run.status}</b>`;
    }
  }, 1500);
}

// --- boot ---
refreshAgents().catch((e) => console.error(e));
refreshSessions().catch((e) => console.error(e));
