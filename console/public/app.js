// agentd console frontend — vanilla JS, no build step. Talks to the Go
// API through the console server's /api proxy (no CORS anywhere).
//
// Views: overview · agents · sessions (chat transcript + SSE) · workflows (DAG).
"use strict";

/* ---------------- utilities ---------------- */

const $ = (sel, el = document) => el.querySelector(sel);
const $$ = (sel, el = document) => [...el.querySelectorAll(sel)];

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
const short = (id) => (id || "").slice(0, 8);

const api = {
  async req(method, path, body) {
    const resp = await fetch("/api" + path, {
      method,
      headers: body !== undefined ? { "Content-Type": "application/json" } : {},
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
    if (resp.status === 204) return null;
    const json = await resp.json().catch(() => ({}));
    if (!resp.ok) {
      const e = json.error || {};
      const err = new Error(e.message || `HTTP ${resp.status}`);
      err.remediation = e.remediation;
      err.code = e.code;
      throw err;
    }
    return json;
  },
};

function timeAgo(iso) {
  if (!iso) return "";
  const s = (Date.now() - new Date(iso).getTime()) / 1000;
  if (s < 60) return "刚刚";
  if (s < 3600) return Math.floor(s / 60) + " 分钟前";
  if (s < 86400) return Math.floor(s / 3600) + " 小时前";
  return Math.floor(s / 86400) + " 天前";
}

function toast(msg, kind = "") {
  const el = document.createElement("div");
  el.className = "toast " + kind;
  el.textContent = msg;
  $("#toast-root").appendChild(el);
  setTimeout(() => el.remove(), 4200);
}

function apiErr(err) {
  toast(err.message + (err.remediation ? ` — ${err.remediation}` : ""), "err");
}

/* ------- modal ------- */
function openModal(html) {
  const root = $("#modal-root");
  root.innerHTML = `<div class="modal-overlay"><div class="modal">${html}</div></div>`;
  const overlay = $(".modal-overlay", root);
  overlay.addEventListener("mousedown", (e) => { if (e.target === overlay) closeModal(); });
  return $(".modal", root);
}
function closeModal() { $("#modal-root").innerHTML = ""; }
document.addEventListener("keydown", (e) => { if (e.key === "Escape") closeModal(); });

/* ------- shared renderers ------- */

const HUES = [222, 262, 172, 22, 340, 200, 90, 300];
function avatarEl(name) {
  let h = 0;
  for (const c of name || "?") h = (h * 31 + c.charCodeAt(0)) | 0;
  const hue = HUES[Math.abs(h) % HUES.length];
  const d = document.createElement("div");
  d.className = "avatar";
  d.style.background = `linear-gradient(135deg, hsl(${hue} 55% 48%), hsl(${(hue + 40) % 360} 55% 40%))`;
  d.textContent = (name || "?")[0];
  return d;
}

const STATE_META = {
  rescheduling: ["blue", true], running: ["amber", true],
  idle: ["green", false], terminated: ["red", false],
  requires_action: ["purple", false],
};
function stateChip(state) {
  const [color, live] = STATE_META[state] || ["", false];
  return `<span class="chip ${color}">${live ? '<span class="pulse"></span>' : ""}${esc(state)}${state === "idle" ? " · 就绪" : ""}</span>`;
}
const STOP_LABEL = { end_turn: "完成", requires_action: "等待输入", retries_exhausted: "重试耗尽" };
function stopLabel(r) { return STOP_LABEL[r] || r; }

function mdLite(text) {
  // escape → fenced code → inline code → bold → paragraphs. Minimal on purpose.
  let s = esc(text);
  const blocks = [];
  s = s.replace(/```(\w*)\n([\s\S]*?)```/g, (_, lang, code) => {
    blocks.push(`<pre><code>${code.replace(/\n$/, "")}</code></pre>`);
    return `\x00${blocks.length - 1}\x00`;
  });
  s = s.replace(/`([^`\n]+)`/g, "<code>$1</code>").replace(/\*\*([^*\n]+)\*\*/g, "<b>$1</b>");
  s = s.split(/\n{2,}/).map((p) => "<p>" + p.replace(/\n/g, "<br>") + "</p>").join("");
  return s.replace(/\x00(\d+)\x00/g, (_, i) => blocks[+i]);
}

async function agentMap() {
  const { agents } = await api.req("GET", "/v1/agents?limit=100");
  return new Map(agents.map((a) => [a.id, a]));
}

const MODELS = ["glm-5.3-flash", "glm-5.3-highspeed", "glm-5.3", "glm-5.2", "glm-5.2-highspeed",
  "glm-5.1", "glm-5-turbo", "glm-5v-turbo", "glm-4.7", "glm-4.6v"];
const TOOLS = ["bash", "read", "write", "edit"];

/* ---------------- router ---------------- */

let routeSeed = 0; // bumps on every navigation so views can reset state

const routes = { overview: viewOverview, agents: viewAgents, sessions: viewSessions, workflows: viewWorkflows };

function navigate() {
  const hash = location.hash.replace(/^#\/?/, "") || "overview";
  const [name, arg] = hash.split("/");
  closeModal();
  teardownView();
  routeSeed++;
  const view = routes[name] || viewOverview;
  $("#main").innerHTML = `<div class="view" id="view"></div>`;
  $$("#nav a").forEach((a) => a.classList.toggle("active", a.dataset.route === "/" + (routes[name] ? name : "overview")));
  view($("#view"), arg).catch((e) => {
    $("#view").innerHTML = `<div class="card"><div class="empty"><div class="big">⚠</div><div class="t">${esc(e.message)}</div></div></div>`;
  });
}
window.addEventListener("hashchange", navigate);

/* per-view cleanup registry */
let cleanups = [];
function onTeardown(fn) { cleanups.push(fn); }
function teardownView() { cleanups.forEach((fn) => { try { fn(); } catch {} }); cleanups = []; }
function every(ms, fn) {
  const t = setInterval(fn, ms);
  onTeardown(() => clearInterval(t));
  return t;
}

/* ================= overview ================= */

async function viewOverview(root) {
  const [{ agents }, { sessions }, runs] = await Promise.all([
    api.req("GET", "/v1/agents?limit=100"),
    api.req("GET", "/v1/sessions?limit=100"),
    wfRuns(),
  ]);
  const running = sessions.filter((s) => s.state === "running" || s.state === "rescheduling").length;
  const agentsById = new Map(agents.map((a) => [a.id, a]));
  const recent = [...sessions].sort((a, b) => new Date(b.updated_at) - new Date(a.updated_at)).slice(0, 6);
  const wfActive = wfRunStatuses(runs).filter((r) => r === "running").length;

  root.innerHTML = `
    <div class="view-head">
      <h1>概览</h1><span class="sub">agentd 控制面运行状况</span>
      <span class="spacer"></span>
      <button class="btn ghost" id="ov-new-agent">新建 Agent</button>
      <button class="btn primary" id="ov-new-session">发起会话</button>
    </div>
    <div class="stats">
      <div class="card stat"><div class="label">Agents</div><div class="value">${agents.length}</div><div class="hintv">已注册的 agent 配置</div></div>
      <div class="card stat"><div class="label">运行中的会话</div><div class="value">${running}</div><div class="hintv">running / rescheduling</div></div>
      <div class="card stat"><div class="label">会话总数</div><div class="value">${sessions.length}</div><div class="hintv">全部 harness</div></div>
      <div class="card stat"><div class="label">Workflow 运行</div><div class="value">${runs.length}</div><div class="hintv">${wfActive ? wfActive + " 个进行中" : "最近 " + (runs.length ? timeAgo(runs[0].ts) : "—")}</div></div>
    </div>
    <div class="grid-2">
      <div class="card">
        <h3>最近会话</h3>
        <div class="rows" id="ov-sessions">
          ${recent.length ? "" : `<div class="empty"><div class="t">还没有会话 — 从右上角发起第一个</div></div>`}
        </div>
      </div>
      <div class="card">
        <h3>Agents</h3>
        <div class="rows" id="ov-agents">
          ${agents.length ? "" : `<div class="empty"><div class="t">还没有 agent — 先创建一个</div></div>`}
        </div>
      </div>
    </div>`;

  const slist = $("#ov-sessions");
  for (const s of recent) {
    const row = document.createElement("div");
    row.className = "rowitem clickable";
    row.innerHTML = `
      <span class="mono faint">${short(s.id)}</span>
      <span class="grow">${esc(agentsById.get(s.agent_id)?.name || "未知 agent")} · ${esc(s.harness)}</span>
      ${stateChip(s.state)}`;
    row.addEventListener("click", () => (location.hash = "#/sessions/" + s.id));
    slist.appendChild(row);
  }
  const alist = $("#ov-agents");
  for (const a of agents.slice(0, 6)) {
    const row = document.createElement("div");
    row.className = "rowitem clickable";
    row.append(avatarEl(a.name));
    const g = document.createElement("div");
    g.className = "grow";
    g.innerHTML = `<b>${esc(a.name)}</b><div class="faint" style="font-size:11.5px">v${a.latest_version} · ${timeAgo(a.created_at)}</div>`;
    row.append(g, Object.assign(document.createElement("span"), { className: "chip outline", textContent: "v" + a.latest_version }));
    row.addEventListener("click", () => (location.hash = "#/agents"));
    alist.appendChild(row);
  }

  $("#ov-new-agent").addEventListener("click", () => agentCreateModal(() => navigate()));
  $("#ov-new-session").addEventListener("click", () => sessionCreateModal());
}

/* ================= agents ================= */

async function viewAgents(root) {
  root.innerHTML = `
    <div class="view-head">
      <h1>Agents</h1><span class="sub">版本化的 agent 配置，更新即产生新版本</span>
      <span class="spacer"></span>
      <button class="btn primary" id="ag-new">＋ 新建 Agent</button>
    </div>
    <div class="agent-grid" id="ag-grid"></div>`;
  $("#ag-new").addEventListener("click", () => agentCreateModal(loadGrid));
  await loadGrid();

  async function loadGrid() {
    const { agents } = await api.req("GET", "/v1/agents?limit=100");
    const grid = $("#ag-grid");
    grid.innerHTML = "";
    if (!agents.length) {
      grid.innerHTML = `<div class="card" style="grid-column:1/-1"><div class="empty"><div class="big">🤖</div><div class="t">还没有 agent</div><button class="btn primary" id="ag-empty-new">创建第一个 agent</button></div></div>`;
      $("#ag-empty-new").addEventListener("click", () => agentCreateModal(loadGrid));
      return;
    }
    for (const a of agents) {
      const card = document.createElement("div");
      card.className = "card agent-card";
      card.innerHTML = `
        <div class="head"></div>
        <div class="desc">${esc(a.description || "（无描述）")}</div>
        <div class="meta" id="m-${a.id}"></div>
        <div class="versions" id="v-${a.id}" hidden></div>
        <div class="foot">
          <button class="btn primary sm" data-act="run">发起会话</button>
          <button class="btn ghost sm" data-act="ver">版本历史</button>
          <span style="flex:1"></span>
          <button class="btn danger-ghost sm" data-act="del">删除</button>
        </div>`;
      $(".head", card).append(avatarEl(a.name));
      $(".head", card).insertAdjacentHTML("beforeend",
        `<div><div class="name">${esc(a.name)}</div><div class="faint" style="font-size:11.5px">创建于 ${timeAgo(a.created_at)}</div></div>`);

      // model chip: fetch pinned latest version config lazily
      api.req("GET", `/v1/agents/${a.id}/versions/${a.latest_version}`).then(({ version }) => {
        const cfg = version.config || {};
        $(`#m-${a.id}`, card).innerHTML =
          `<span class="chip blue">${esc(cfg.model || "?")}</span>` +
          (cfg.tools || []).map((t) => `<span class="chip outline mono">${esc(t)}</span>`).join("");
      }).catch(() => {});

      $('[data-act="run"]', card).addEventListener("click", () => sessionCreateModal(a));
      $('[data-act="ver"]', card).addEventListener("click", async () => {
        const box = $(`#v-${a.id}`, card);
        if (!box.hidden) { box.hidden = true; return; }
        if (!box.dataset.loaded) {
          const { versions } = await api.req("GET", `/v1/agents/${a.id}/versions`);
          box.innerHTML = versions.map((v) =>
            `<div class="ver"><span class="v">v${v.version}</span><span class="m">${esc(v.config?.model || "")}</span><span class="faint">${esc((v.config?.system_prompt || "").slice(0, 46))}${(v.config?.system_prompt || "").length > 46 ? "…" : ""}</span><span class="faint" style="margin-left:auto">${timeAgo(v.created_at)}</span></div>`).join("");
          box.dataset.loaded = "1";
        }
        box.hidden = false;
      });
      $('[data-act="del"]', card).addEventListener("click", () => {
        if (!confirm(`删除 agent「${a.name}」？有关联会话时会被拒绝。`)) return;
        api.req("DELETE", `/v1/agents/${a.id}`)
          .then(() => { toast("已删除 " + a.name, "ok"); loadGrid(); })
          .catch(apiErr);
      });
      grid.appendChild(card);
    }
  }
}

function agentCreateModal(done) {
  const m = openModal(`
    <h2>新建 Agent</h2>
    <form id="agent-form">
      <label class="field"><span>名称</span><input name="name" required placeholder="coder"></label>
      <label class="field"><span>描述</span><input name="description" placeholder="负责写代码的 agent（可选）"></label>
      <label class="field"><span>模型</span><input name="model" required list="model-list" placeholder="glm-5.3-flash">
        <datalist id="model-list">${MODELS.map((x) => `<option value="${x}">`).join("")}</datalist></label>
      <label class="field"><span>System prompt</span><textarea name="system_prompt" rows="4" placeholder="You are a careful coding agent…"></textarea></label>
      <div class="field"><span>工具</span><div class="checks">
        ${TOOLS.map((t) => `<label><input type="checkbox" name="tool" value="${t}" checked>${t}</label>`).join("")}
      </div></div>
      <div class="modal-foot">
        <button type="button" class="btn ghost" id="cancel">取消</button>
        <button type="submit" class="btn primary">创建</button>
      </div>
    </form>`);
  $("#cancel", m).addEventListener("click", closeModal);
  $("#agent-form", m).addEventListener("submit", async (e) => {
    e.preventDefault();
    const f = e.target;
    try {
      await api.req("POST", "/v1/agents", {
        name: f.name.value.trim(),
        description: f.description.value.trim(),
        config: {
          model: f.model.value.trim(),
          system_prompt: f.system_prompt.value,
          tools: $$('input[name="tool"]:checked', f).map((i) => i.value),
        },
      });
      closeModal();
      toast("Agent 已创建", "ok");
      done && done();
    } catch (err) { apiErr(err); }
  });
}

/* ================= sessions ================= */

let currentDetail = null; // { id, es, pollState, maxSeq }

async function viewSessions(root, sessionId) {
  root.innerHTML = `
    <div class="view-head">
      <h1>会话</h1><span class="sub">每次运行都是一条 append-only 事件流</span>
      <span class="spacer"></span>
      <button class="btn primary" id="ss-new">＋ 发起会话</button>
    </div>
    <div class="sess-layout">
      <div class="card sess-list" id="ss-list"></div>
      <div class="card transcript-card" id="ss-detail">
        <div class="empty" style="flex:1"><div class="big">💬</div><div class="t">选择左侧会话，或发起一个新会话</div></div>
      </div>
    </div>`;
  $("#ss-new").addEventListener("click", () => sessionCreateModal());

  await refreshSessionList();
  every(5000, refreshSessionList);
  if (sessionId) selectSession(sessionId).catch((e) => toast("无法打开会话：" + e.message, "err"));

  async function refreshSessionList() {
    const [{ sessions }, byId] = await Promise.all([api.req("GET", "/v1/sessions?limit=60"), agentMap()]);
    const list = $("#ss-list");
    if (!list) return;
    list.innerHTML = sessions.length ? "" : `<div class="empty"><div class="t">暂无会话</div></div>`;
    for (const s of [...sessions].sort((a, b) => new Date(b.updated_at) - new Date(a.updated_at))) {
      const el = document.createElement("div");
      el.className = "sess-item" + (currentDetail?.id === s.id ? " sel" : "");
      el.dataset.sid = s.id;
      el.innerHTML = `
        <div class="t"><span class="id">${short(s.id)}</span>${stateChip(s.state)}</div>
        <div class="m"><span>${esc(byId.get(s.agent_id)?.name || "…")}</span><span>·</span><span>${esc(s.harness)}</span><span style="margin-left:auto">${timeAgo(s.updated_at)}</span></div>`;
      el.addEventListener("click", () => (location.hash = "#/sessions/" + s.id));
      list.appendChild(el);
    }
  }
}

function sessionCreateModal(presetAgent) {
  (async () => {
    const { agents } = await api.req("GET", "/v1/agents?limit=100");
    if (!agents.length) { closeModal(); toast("请先创建一个 agent", "err"); location.hash = "#/agents"; return; }
    const m = openModal(`
      <h2>发起会话</h2>
      <label class="field"><span>Agent</span><select id="ns-agent">
        ${agents.map((a) => `<option value="${a.id}" ${presetAgent?.id === a.id ? "selected" : ""}>${esc(a.name)} · v${a.latest_version}</option>`).join("")}
      </select></label>
      <label class="field"><span>Harness</span><select id="ns-harness">
        <option value="native">native（内置循环）</option>
        <option value="opencode">opencode（实验）</option>
      </select></label>
      <div class="modal-foot">
        <button type="button" class="btn ghost" id="cancel">取消</button>
        <button class="btn primary" id="go">开始</button>
      </div>`);
    $("#cancel", m).addEventListener("click", closeModal);
    $("#go", m).addEventListener("click", async () => {
      try {
        const { session } = await api.req("POST", "/v1/sessions", {
          agent_id: $("#ns-agent").value,
          harness: $("#ns-harness").value,
        });
        closeModal();
        toast("会话已创建", "ok");
        location.hash = "#/sessions/" + session.id;
      } catch (err) { apiErr(err); }
    });
  })();
}

/* ------- transcript ------- */

async function selectSession(sessionId) {
  teardownDetail();
  currentDetail = { id: sessionId, es: null, poll: null, maxSeq: 0, toolCards: new Map(), ephemeral: null };

  const detail = $("#ss-detail");
  detail.innerHTML = `
    <div class="transcript-head" id="tr-head"><span class="muted">加载中…</span></div>
    <div class="transcript" id="tr"></div>
    <div class="composer">
      <div class="box">
        <textarea id="cp-input" rows="1" placeholder="给 agent 发送消息…"></textarea>
        <button class="btn primary" id="cp-send">发送</button>
      </div>
      <div class="hint">Enter 发送 · Shift+Enter 换行 · 运行中不可发送</div>
    </div>`;
  const tr = $("#tr");

  // history replay (the durable part), then live tail
  const { events } = await api.req("GET", `/v1/sessions/${sessionId}/events?limit=300`);
  if (currentDetail?.id !== sessionId) return;
  if (!events.length) {
    tr.innerHTML = `<div class="empty"><div class="t">会话还是空的 — 发第一条消息开始</div></div>`;
  }
  for (const ev of events) renderEvent(ev);
  tr.scrollTop = tr.scrollHeight;

  const es = new EventSource(`/api/v1/sessions/${sessionId}/stream?after_seq=${currentDetail.maxSeq}`);
  currentDetail.es = es;
  es.addEventListener("log", (e) => {
    renderEvent(JSON.parse(e.data));
    scrollBottom();
  });
  es.addEventListener("delta", (e) => {
    const d = JSON.parse(e.data);
    if (d.type === "restart") { dropEphemeral(); return; }
    if (!d.text) return;
    appendEphemeral(d.text);
    scrollBottom();
  });
  es.onerror = () => { /* EventSource auto-reconnects with Last-Event-ID */ };

  pollSessionMeta(sessionId);
  bindComposer(sessionId);
}

function teardownDetail() {
  if (!currentDetail) return;
  currentDetail.es?.close();
  clearInterval(currentDetail.poll);
  currentDetail = null;
}
onTeardown(teardownDetail);

function scrollBottom() {
  const tr = $("#tr");
  if (tr) tr.scrollTop = tr.scrollHeight;
}

async function pollSessionMeta(sessionId) {
  const update = async () => {
    if (currentDetail?.id !== sessionId) return;
    let s;
    try { ({ session: s } = await api.req("GET", `/v1/sessions/${sessionId}`)); } catch { return; }
    if (currentDetail?.id !== sessionId) return;
    renderHead(s);
    const busy = s.state === "running" || s.state === "rescheduling";
    $("#cp-send").disabled = busy;
    $("#cp-input").disabled = busy && s.state === "running";
    $("#cp-input").placeholder = busy ? "agent 正在运行…" : "给 agent 发送消息…";
  };
  await update();
  currentDetail.poll = setInterval(update, 1500);
}

async function renderHead(s) {
  const head = $("#tr-head");
  if (!head) return;
  const headKey = `${s.agent_id}:${s.agent_version}`;
  if (currentDetail.headInfoKey !== headKey) {
    currentDetail.headInfoKey = headKey;
    currentDetail.headInfo = api.req("GET", `/v1/agents/${s.agent_id}`)
      .then(({ agent }) =>
        api.req("GET", `/v1/agents/${s.agent_id}/versions/${s.agent_version}`)
          .then(({ version }) => ({ name: agent.name, model: version.config?.model || "" })))
      .catch(() => ({ name: "未知 agent", model: "" }));
  }
  const { name: agentName, model } = await currentDetail.headInfo;
  if (currentDetail?.id !== s.id) return;
  head.innerHTML = `
    <span class="title">${esc(agentName)}</span>
    <span class="sid" title="点击复制完整 ID">${short(s.id)}</span>
    <span class="chip outline">${esc(s.harness)}</span>
    ${model ? `<span class="chip blue">${esc(model)}</span>` : ""}
    ${stateChip(s.state)}
    ${s.stop_reason ? `<span class="chip ${s.stop_reason === "end_turn" ? "green" : "amber"}">${esc(stopLabel(s.stop_reason))}</span>` : ""}
    <span class="spacer"></span>
    <span class="faint" style="font-size:11.5px">agent v${s.agent_version}</span>`;
  $(".sid", head).addEventListener("click", () => {
    navigator.clipboard?.writeText(s.id);
    toast("会话 ID 已复制", "ok");
  });
}

function toolCardEl(tc) {
  const d = document.createElement("details");
  d.className = "toolcard";
  d.innerHTML = `
    <summary><span class="tw">▶</span><span>🛠</span><span class="tn">${esc(tc.name)}</span>
      <span class="tr chip amber"><span class="pulse"></span>运行中</span></summary>
    <div class="tbody">
      <div class="tl">输入</div><pre class="tin">${esc(JSON.stringify(tc.input, null, 2))}</pre>
      <div class="tl" style="display:none">输出</div><pre class="tout" style="display:none"></pre>
    </div>`;
  return d;
}

function renderEvent(ev) {
  if (!currentDetail || ev.seq <= currentDetail.maxSeq) return;
  currentDetail.maxSeq = ev.seq;
  const tr = $("#tr");
  if (!tr) return;
  tr.querySelector(".empty")?.remove();
  const p = ev.payload || {};

  if (ev.type === "session.created" || ev.type === "session.state_changed") {
    if (ev.type === "session.created") return;
    const d = document.createElement("div");
    d.className = "divider";
    d.textContent = `${p.from || "…"} → ${p.to || "…"}`;
    tr.appendChild(d);
    return;
  }
  if (ev.type === "turn.completed") {
    const d = document.createElement("div");
    d.className = "divider";
    d.textContent = p.error ? `turn 结束 · ${stopLabel(p.stop_reason || "")} · ${p.error}`
      : p.stop_reason && p.stop_reason !== "end_turn" ? `turn 结束 · ${stopLabel(p.stop_reason)}`
      : "turn 完成";
    tr.appendChild(d);
    return;
  }
  if (ev.type === "message.user") {
    dropEphemeral();
    const blocks = p.content || [];
    const text = blocks.filter((b) => b.type === "text").map((b) => b.text).join("\n");
    const m = document.createElement("div");
    m.className = "msg user";
    m.innerHTML = `<div class="bubble">${mdLite(text) || "<p>（空消息）</p>"}</div>`;
    tr.appendChild(m);
    return;
  }
  if (ev.type === "message.assistant") {
    dropEphemeral();
    const m = document.createElement("div");
    m.className = "msg agent";
    const bubble = document.createElement("div");
    bubble.className = "bubble";
    for (const b of p.content || []) {
      if (b.type === "text" && b.text?.trim()) bubble.insertAdjacentHTML("beforeend", mdLite(b.text));
      if (b.type === "tool_use") {
        const card = toolCardEl({ name: b.name, input: b.input });
        currentDetail.toolCards.set(b.id, card);
        bubble.appendChild(card);
      }
    }
    if (!bubble.childNodes.length) bubble.innerHTML = "<p class='faint'>(空回复)</p>";
    m.insertAdjacentHTML("afterbegin", `<div class="who">a</div>`);
    m.appendChild(bubble);
    tr.appendChild(m);
    return;
  }
  if (ev.type === "tool.requested") {
    let card = currentDetail.toolCards.get(p.tool_use_id);
    if (!card) {
      card = toolCardEl({ name: p.name, input: p.input });
      currentDetail.toolCards.set(p.tool_use_id, card);
      tr.appendChild(card);
    } else {
      const v = card.querySelector(".tr");
      if (p.verdict === "deny") { v.className = "tr chip red"; v.textContent = "denied"; }
    }
    return;
  }
  if (ev.type === "tool.completed" || ev.type === "tool.failed") {
    const card = currentDetail.toolCards.get(p.tool_use_id);
    if (!card) return;
    const v = card.querySelector(".tr");
    const failed = ev.type === "tool.failed" || p.is_error;
    v.className = "tr chip " + (failed ? "red" : "green");
    v.textContent = failed ? "失败" : "完成";
    const out = card.querySelector(".tout");
    out.textContent = typeof p.output === "string" ? p.output : JSON.stringify(p.output, null, 2);
    out.style.display = "";
    card.querySelector(".tl:last-of-type").style.display = "";
    return;
  }
  if (ev.type === "escalation.requested") {
    const d = document.createElement("div");
    d.className = "divider";
    d.textContent = "⚠ 需要人工确认（requires_action）";
    tr.appendChild(d);
  }
}

/* ephemeral streaming bubble (deltas are never persisted) */
function appendEphemeral(text) {
  const tr = $("#tr");
  if (!tr || !currentDetail) return;
  if (!currentDetail.ephemeral) {
    currentDetail.ephemeral = { el: null, buf: "" };
    const m = document.createElement("div");
    m.className = "msg agent typing";
    m.innerHTML = `<div class="who">a</div><div class="bubble"><span class="buf"></span><span class="caret"></span></div>`;
    tr.appendChild(m);
    currentDetail.ephemeral.el = m;
  }
  currentDetail.ephemeral.buf += text;
  $(".buf", currentDetail.ephemeral.el).textContent = currentDetail.ephemeral.buf;
}
function dropEphemeral() {
  if (currentDetail?.ephemeral) { currentDetail.ephemeral.el.remove(); currentDetail.ephemeral = null; }
}

function bindComposer(sessionId) {
  const input = $("#cp-input");
  const send = $("#cp-send");
  const doSend = async () => {
    const text = input.value.trim();
    if (!text || send.disabled) return;
    input.value = "";
    input.style.height = "auto";
    send.disabled = true;
    try {
      await api.req("POST", `/v1/sessions/${sessionId}/events`, {
        type: "message.user",
        payload: { content: [{ type: "text", text }] },
      });
      // wake the actor; 409 (already running) is fine
      await api.req("POST", `/v1/sessions/${sessionId}/run`).catch(() => {});
    } catch (err) { apiErr(err); }
    input.focus();
  };
  send.addEventListener("click", doSend);
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); doSend(); }
  });
  input.addEventListener("input", () => {
    input.style.height = "auto";
    input.style.height = Math.min(input.scrollHeight, 160) + "px";
  });
  setTimeout(() => input.focus(), 50);
}

/* ================= workflows ================= */

const WF_TEMPLATE = {
  name: "software-dev",
  nodes: [
    { id: "coder", prompt: "You are the coder in a software pipeline. Implement the following spec as one complete file named solution.py using the write_file tool. Then summarize your design in one paragraph.\n\nSPEC:\n{{spec}}", output_files: ["solution.py"] },
    { id: "reviewer", depends_on: ["coder"], prompt: "You are the code reviewer. Review this code for correctness and style. Name at most two concrete improvements, briefly.\n\nCODE:\n{{files.coder.solution.py}}" },
    { id: "tester", depends_on: ["coder"], prompt: "You are the test engineer. Write a minimal 3-bullet test plan for this code, then state PASS or FAIL with one sentence of rationale.\n\nCODE:\n{{files.coder.solution.py}}" },
    { id: "merger", depends_on: ["reviewer", "tester"], prompt: "You are the merger. Produce the final merged artifact: use the write_file tool to write MERGED.md containing (1) the final code, (2) each review note with incorporated/rejected + reason, (3) the test verdict. Then summarize in one sentence.\n\nORIGINAL CODE:\n{{files.coder.solution.py}}\n\nREVIEW:\n{{outputs.reviewer}}\n\nTEST VERDICT:\n{{outputs.tester}}" },
  ],
};

function wfRunCache() {
  try { return JSON.parse(localStorage.getItem("agentd.wf.runs") || "[]"); } catch { return []; }
}
function rememberWorkflowRun(run) {
  const ts = run.created_at || run.ts;
  if (!ts) return;
  const cache = wfRunCache().filter((r) => r.id !== run.id);
  cache.unshift({ id: run.id, name: run.name, ts, status: run.status });
  localStorage.setItem("agentd.wf.runs", JSON.stringify(cache.slice(0, 20)));
}
async function wfRuns(limit = 20) {
  try {
    const { runs } = await api.req("GET", `/v1/workflows?limit=${limit}`);
    runs.forEach(rememberWorkflowRun);
    return runs.map((run) => ({ ...run, ts: run.created_at || run.ts })).filter((run) => run.ts);
  } catch {
    return wfRunCache();
  }
}
function wfRunStatuses(runs) { return runs.map((r) => r.status || "?"); }

async function viewWorkflows(root) {
  root.innerHTML = `
    <div class="view-head">
      <h1>Workflows</h1><span class="sub">DAG 编排：spec 进，产物出</span>
    </div>
    <div class="wf-layout">
      <div style="display:flex; flex-direction:column; gap:16px;">
        <div class="card">
          <h3>运行 software-dev 流水线</h3>
          <label class="field"><span>Agent（各节点共用）</span><select id="wf-agent"></select></label>
          <label class="field"><span>Spec</span><textarea id="wf-spec" rows="5" placeholder="Write a Python function fib(n) (iterative) in solution.py…"></textarea></label>
          <button class="btn primary" id="wf-run" style="width:100%">运行流水线</button>
          <div class="faint" style="font-size:11.5px;margin-top:10px">coder → (reviewer ∥ tester) → merger</div>
        </div>
        <div class="card runlist" id="wf-runs"><h3>运行历史</h3></div>
      </div>
      <div class="card dag-wrap" id="wf-board">
        <div class="empty" style="padding:80px 20px"><div class="big">🗂</div><div class="t">发起一次运行，或从左侧历史中选择</div></div>
      </div>
    </div>`;

  const { agents } = await api.req("GET", "/v1/agents?limit=100");
  const sel = $("#wf-agent");
  sel.innerHTML = agents.map((a) => `<option value="${a.id}">${esc(a.name)}</option>`).join("") || "<option value=''>（无 agent）</option>";
  if (agents.length) sel.disabled = false;

  $("#wf-run").addEventListener("click", async () => {
    const agentId = sel.value;
    const spec = $("#wf-spec").value.trim();
    if (!agentId) return toast("请先创建 agent", "err");
    if (!spec) return toast("请填写 spec", "err");
    const btn = $("#wf-run");
    btn.disabled = true;
    try {
      const def = JSON.parse(JSON.stringify(WF_TEMPLATE));
      for (const n of def.nodes) {
        n.agent = agentId;
        n.prompt = n.prompt.replace("{{spec}}", spec);
      }
      const { run } = await api.req("POST", "/v1/workflows", def);
      rememberWorkflowRun(run);
      toast("流水线已启动 " + short(run.id), "ok");
      selectRun(run.id);
    } catch (err) { apiErr(err); }
    btn.disabled = false;
  });

  let wfSelected = null, wfPoll = null;

  await renderRunList();
  const initialRuns = await wfRuns();
  if (initialRuns[0]) selectRun(initialRuns[0].id);

  async function renderRunList() {
    const box = $("#wf-runs");
    const runs = await wfRuns();
    box.innerHTML = "<h3>运行历史</h3>" + (runs.length ? "" : "<div class='faint' style='font-size:12.5px;padding:4px 2px'>暂无运行记录</div>");
    for (const r of runs) {
      const el = document.createElement("div");
      el.className = "runitem" + (r.id === wfSelected ? " sel" : "");
      el.innerHTML = `<div class="rid">${short(r.id)} <span class="muted">${esc(r.name)}</span></div><div class="rmeta">${timeAgo(r.ts)}</div>`;
      el.addEventListener("click", () => selectRun(r.id));
      box.appendChild(el);
    }
  }

  onTeardown(() => clearInterval(wfPoll));
  async function selectRun(runId) {
    clearInterval(wfPoll);
    wfSelected = runId;
    renderRunList();
    const draw = async () => {
      let run;
      try { ({ run } = await api.req("GET", `/v1/workflows/${runId}`)); } catch { return; }
      rememberWorkflowRun(run);
      if (wfSelected !== runId) return;
      renderDag(run);
      renderRunList();
      if (run.status !== "running") clearInterval(wfPoll);
    };
    await draw();
    wfPoll = setInterval(draw, 1500);
  }

  function renderDag(run) {
    const board = $("#wf-board");
    const nodes = run.node_states || [];
    if (!nodes.length) { board.innerHTML = "<div class='empty'>无节点状态</div>"; return; }

    // level = longest path from roots; group columns
    const defs = new Map(run.definition.nodes.map((n) => [n.id, n]));
    const level = {};
    const lv = (id, seen = new Set()) => {
      if (level[id] !== undefined) return level[id];
      if (seen.has(id)) return 0;
      seen.add(id);
      const deps = defs.get(id)?.depends_on || [];
      return (level[id] = deps.length ? Math.max(...deps.map((d) => lv(d, seen))) + 1 : 0);
    };
    nodes.forEach((n) => lv(n.id));
    const cols = {};
    for (const n of nodes) (cols[level[n.id]] ||= []).push(n);

    const W = 178, GX = 84, H = 86, GY = 22, PAD = 22;
    const maxCol = Math.max(...Object.values(cols).map((c) => c.length));
    const width = PAD * 2 + (Object.keys(cols).length) * W + (Object.keys(cols).length - 1) * GX;
    const height = PAD * 2 + maxCol * H + (maxCol - 1) * GY;

    // layout first: positions must exist before edges are drawn
    const pos = {};
    for (const k of Object.keys(cols).sort((a, b) => a - b)) {
      cols[k].forEach((n, i) => {
        pos[n.id] = { x: PAD + k * (W + GX), y: PAD + i * (H + GY) };
      });
    }

    let html = `<div style="display:flex;align-items:center;gap:10px;margin-bottom:14px">
      <h3 style="margin:0">${esc(run.name)} · <span class="mono">${short(run.id)}</span></h3>
      ${runChip(run.status)}<span class="spacer" style="flex:1"></span></div>
      <div class="dag-canvas" style="width:${width}px;height:${height}px">`;
    html += `<svg width="${width}" height="${height}">`;
    for (const n of run.definition.nodes) {
      for (const dep of n.depends_on || []) {
        const a = pos[dep], b = pos[n.id];
        if (!a || !b) continue;
        const x1 = a.x + W, y1 = a.y + H / 2, x2 = b.x, y2 = b.y + H / 2;
        const mx = (x1 + x2) / 2;
        const st = nodes.find((x) => x.id === dep)?.status;
        html += `<path d="M${x1},${y1} C${mx},${y1} ${mx},${y2} ${x2},${y2}" fill="none"
          stroke="${st === "completed" ? "rgba(67,192,122,.55)" : "#2b3546"}" stroke-width="1.6"
          marker-end="url(#arrow)"/>`;
      }
    }
    html += `<defs><marker id="arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto">
      <path d="M0,0 L8,4 L0,8 z" fill="#2b3546"/></marker></defs></svg>`;

    for (const k of Object.keys(cols).sort((a, b) => a - b)) {
      cols[k].forEach((n, i) => {
        const { x, y } = pos[n.id];
        const meta = [n.session_id ? "sess " + short(n.session_id) : "", n.attempts > 1 ? "重试 " + n.attempts : ""].filter(Boolean).join(" · ");
        html += `<div class="dag-node st-${esc(n.status)}" data-sid="${esc(n.session_id || "")}" style="left:${x}px;top:${y}px">
          <div class="nid">${n.status === "running" ? '<span class="spin"></span>' : ""}${esc(n.id)}</div>
          <div style="margin-top:5px">${nodeChip(n.status)}</div>
          ${meta ? `<div class="nmeta">${esc(meta)}</div>` : ""}
          ${n.error ? `<div class="nerr" title="${esc(n.error)}">${esc(n.error)}</div>` : ""}
        </div>`;
      });
    }
    html += `</div>`;
    board.innerHTML = html;
    $$(".dag-node", board).forEach((el) => {
      el.addEventListener("click", () => {
        if (el.dataset.sid) location.hash = "#/sessions/" + el.dataset.sid;
      });
    });
  }

  function runChip(st) {
    const c = { running: "amber", completed: "green", failed: "red" }[st] || "";
    return `<span class="chip ${c}">${st === "running" ? '<span class="pulse"></span>' : ""}${esc(st)}</span>`;
  }
  function nodeChip(st) {
    const label = { pending: "等待", running: "运行中", completed: "完成", failed: "失败" }[st] || st;
    const c = { running: "amber", completed: "green", failed: "red" }[st] || "";
    return `<span class="chip ${c}">${label}</span>`;
  }
}

/* ---------------- boot ---------------- */

async function healthPoll() {
  const dot = $("#conn-dot"), txt = $("#conn-text");
  const tick = async () => {
    try {
      const controller = new AbortController();
      const timeout = setTimeout(() => controller.abort(), 3000);
      try {
        await fetch("/api/healthz", { signal: controller.signal });
      } finally {
        clearTimeout(timeout);
      }
      dot.className = "dot ok";
      txt.textContent = "API 已连接";
    } catch {
      dot.className = "dot bad";
      txt.textContent = "API 不可达";
    }
  };
  await tick();
  setInterval(tick, 10000);
}

healthPoll();
navigate();
