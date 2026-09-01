# Claude Managed Agents 调研 & 开源版可行性分析

> Kira, 2026-08-30

---

## 1. Managed Agents 是怎么做的

### 1.1 核心心智模型：四个对象，一条铁律

| 对象 | 端点 | 是什么 |
|---|---|---|
| **Agent** | `/v1/agents` | 持久化、版本化的配置（model / system / tools / MCP / skills）。每次 update 生成不可变新版本 |
| **Session** | `/v1/sessions` | 一次有状态运行实例，引用 agent id（可 pin version）+ environment。产出事件流 |
| **Environment** | `/v1/environments` | 容器供给模板（网络策略 unrestricted/limited、包管理、cloud/self_hosted） |
| **Container** | 无端点 | 每会话一个隔离计算实例，**只有工具在这里执行**（bash、文件、代码） |

铁律：**Agent 建一次，Session 每次跑都引用它**。`model/system/tools` 永远挂在 agent 上，不在 session 上。版本化的意义是复现性和回滚——session 可以 pin 到 `{id, version}`。

### 1.2 架构的关键分离

```
                 ┌──────────────────────────────────────┐
Agent (config) ─▶│ Anthropic 编排层（agent loop：模型    │
                 │ 推理 + 决定调哪个工具 + 上下文管理）   │
                 └──────────────┬───────────────────────┘
                                │ tool call（协议化）
                                ▼
Environment (template) ─▶ Container（工具执行工作区：bash/文件/代码）
                                │
                 Session ───────┤
                                ├── Resources（文件/GitHub 仓库/memory store，启动时挂载）
                                ├── Vault IDs（MCP 凭证引用）
                                └── 事件流（SSE 出，events.send 进）
```

**agent loop 不在容器里跑**。容器只是工具的手和脚。这是整个设计的分水岭：智能在编排层，执行在沙箱。

### 1.3 值得抄的 10 个设计点

1. **控制面 / 数据面分离**：agent、environment 是相对静态的资源，用 YAML + CLI（`ant`）管理，进版本控制、走 CI；session 是动态的，由应用代码驱动。同一套 API，按调用频率和归属分层。
2. **事件溯源的会话协议**：会话里发生的一切都是事件（`user.message`、`agent.tool_use`、`agent.custom_tool_use`、`session.status_idle`…）。客户端发送事件、SSE 流回事件、历史可 `events.list` 分页重放。
3. **`processed_at` 门闸**：客户端发出的事件先以 `processed_at: null` 出现（已入队），被 agent 消费后再出现一次（带时间戳）。同一个事件流上天然支持 pending → acked 的 UI 状态。
4. **正确的 idle 语义**：`session.status_idle` 不等于"结束"。要看 `stop_reason`：`requires_action`（等你回 tool 确认/自定义工具结果）必须继续处理，`end_turn`/`retries_exhausted` 才是终止。这是客户端最容易写错的地方。
5. **断流重连 = 历史 + 增量去重**：SSE 无重放。重连后先拉 `events.list` 全量历史，再 tail 实时流，按 event id 去重。否则 pending 的 tool_use 会死锁会话。
6. **凭证永不进沙箱（vault 代理模式）**：MCP 调用和 git push/pull 在**离开沙箱后**由 Anthropic 侧代理注入凭证。容器里的代码（包括 agent 自己写的）物理上读不到 secret。这是安全模型的精髓，也是最容易被开源版偷懒掉的地方。
7. **self-hosted sandbox 是官方留的后门**：`config: {type: "self_hosted"}` 后，工具执行搬到你的 infra，loop 仍在 Anthropic。你的 worker **出站长轮询** work queue（`EnvironmentWorker.run()` / `ant beta:worker poll`），Anthropic 从不主动拨入你的网络。——做开源版时，这套 worker 协议就是现成的接口参考。
8. **多 agent = 协调者 + 线程**：coordinator 的 `multiagent.agents` 声明花名册，每个子 agent 一个 context 隔离的 thread，但**共享同一个容器文件系统**。协调靠显式消息，不靠共享上下文。
9. **Memory 是 FUSE 挂载的目录**，不是专用 API：memory store 挂到 `/mnt/memory/<store>/`，agent 用普通文件工具读写，每次变更产生不可变 `memver_` 版本（审计 + 回滚 + redact）。
10. **Outcome = rubric 评分的 iterate 循环**：`user.define_outcome` 带上可逐条评分的 rubric，独立 grader 上下文打分，结果驱动 satisfied / needs_revision / max_iterations_reached。把"对话"升级成"可验收的工作"。

### 1.4 平台内置、你白拿的能力

- **Context compaction**：接近上下文上限时服务端自动压缩历史（`agent.thread_context_compacted` 事件）
- **Prompt caching**：历史前缀自动缓存
- **Extended thinking**：默认开
- **Session 状态机**：会话 born `idle`，有工作时进入 `running`，在 `running ⇄ idle` 间往返并最终 `terminated`；`rescheduling` 只表示 kick 在途的瞬时态，kick 扑空时会诚实停回 `idle`，可重试错误再重新调度
- **Webhooks**：thin payload（只有类型 + 资源 ID）+ HMAC 签名 + 重试去重，免轮询
- **Permission policies**：`always_allow` / `always_ask`（后者把 tool call 挂起等客户端确认）
- **限流**：控制面按 org RPM 限速（创建 300 RPM、其他 600 RPM、环境 60 RPM / 5 并发）

---

## 2. 开源生态盘点

### 2.1 直接对标 CMA 的项目

| 项目 | 定位 | 许可证 | 成熟度 | 备注 |
|---|---|---|---|---|
| [rogeriochaves/open-managed-agents](https://github.com/rogeriochaves/open-managed-agents) | 1:1 复刻 CMA：agents/sessions/environments/vaults + 治理层 | AGPL-3.0 | **早期**（~110 stars，150 commits，387 tests，无 release） | Hono + React + Vercel AI SDK（7 家 LLM），SQLite/Postgres，Docker Compose + Helm。治理层（org/team/RBAC、RPM 限额、审计日志）是差异化。sandbox 隔离实现 README 未披露，需读 `packages/server/src/engine/` 源码确认 |
| [TrueForge (TrueFoundry)](https://thenewstack.io/truefoundry-trueforge-claude-managed-agents/) | 企业级 agent harness，任意模型 + 任意 MCP | 开源 | 新发布 | 偏企业生产部署，公司背书 |
| [Multica](https://faun.pub/the-open-source-claude-managed-agents-alternative-is-here-meet-multica-7035cca69a5d) | CMA 类比物，强调人机同一工作流（任务看板/指派/活动流） | 开源 | 早期 | 产品形态更接近"agent 版 Linear" |

### 2.2 架构同构的成熟项目（拼装开源版的积木）

| 项目 | 对应 CMA 的哪一层 | 许可证 | 说明 |
|---|---|---|---|
| [OpenHands](https://github.com/All-Hands-AI/OpenHands)（原 OpenDevin） | **agent loop + per-session sandbox 的同构实现** | MIT | 最像 CMA 的成熟开源架构：agent controller 通过事件流驱动 Docker runtime（bash/IPython/browser），action/observation 事件溯源。约 60k+ stars |
| [E2B](https://e2b.dev) | **Environment / Container 层** | Apache 2.0 | Firecracker microVM 沙箱，冷启动 ~150ms，可完全自托管。是开源沙箱的事实标准 |
| [Suna (Kortix)](https://github.com/kortix-ai/suna) | 产品级整机参考 | Apache 2.0 | 通用 agent 产品：FastAPI + Supabase + Redis + Daytona 沙箱 + LiteLLM 多模型。想"直接用"而不是"自己造"时的参考 |
| [LangGraph](https://github.com/langchain-ai/langgraph) | 编排层（自己写 loop 时） | MIT | 状态图编排 + checkpoint 持久化。CMA 那种"自由 loop"用它表达不如直接手写 tool-use loop |
| [Letta](https://github.com/letta-ai/letta) / mem0 | Memory 层 | Apache 2.0 | 状态化 agent + 记忆块管理。CMA 的 memory store 更简单（文件 + 版本表），自己做不难 |
| Temporal / Restate | durable execution（session 重调度、断点续跑） | MIT / 各自 | CMA 的 `rescheduling` 状态本质就是 durable execution 问题 |
| [claude-agent-sdk (Python)](https://github.com/anthropics/claude-agent-sdk-python) / [TS](https://github.com/anthropics/claude-agent-sdk-typescript) | **agent loop 现成品** | **源码可见，专有许可**（demos 是 MIT） | Claude Code 同款 harness：loop + 内建工具 + hooks + subagents + MCP。注意不是 OSI 开源 |

### 2.3 Anthropic 官方已经开放的部分

- SDK（MIT）里自带 **EnvironmentWorker / WorkPoller**（Python/TS/Go）——self-hosted sandbox 的 worker 实现，轮询 → 派发 → 执行工具的完整循环
- `ant` CLI 的 `beta:worker poll/run`——固定工具集的 worker
- bash / text-editor 工具的**参考实现**（公开文档）

也就是说：工具执行侧基本全开放了，**只有编排层的 loop 和平台工程是闭源的**——这正是开源版要替代的部分。

---

## 3. 做一个开源版：可行性评估

### 3.1 诚实的前置判断

1. **这件事已经不是空白市场**。open-managed-agents 在 1:1 复刻（AGPL，个人项目，早期）；TrueForge 有公司背书；OpenHands 在架构同构层有 60k stars。立项前先回答：你跟他们的差异点是什么？
2. **agent loop 是最简单的部分**。tool-use loop 任何框架都能写，Claude Agent SDK 更是现成的（许可要注意）。**不要**把工程量预估在 loop 上。
3. **真正的成本在三处**：
   - **Sandbox 基础设施**：microVM 调度、冷启动优化、镜像管理、网络隔离策略。这是 Anthropic 最强也最难抄的部分。自己造不如直接用 E2B（自托管）或 OpenHands runtime。
   - **Durable 事件流**：SSE 断流重连、事件去重、`processed_at` 语义、session 重调度。看着简单，写对要掉层皮（见 1.3 的 4、5 两点——CMA 官方文档花了大量篇幅讲客户端怎么不写错）。
   - **凭证代理安全模型**：vault 凭证不进沙箱、出沙箱后代理注入。大多数开源项目在这里偷懒（直接环境变量进容器），如果你做对了，这本身就是差异点。
4. **许可证雷区**：open-managed-agents 是 AGPL（直接 fork 做 SaaS 有传染性问题）；claude-agent-sdk 是源码可见但专有许可（不能拿去当"开源版"的核心再分发）；E2B / OpenHands / Suna 是 Apache 2.0 / MIT，可以放心用。

### 3.2 推荐架构（如果决定立项）

```
┌──────────────── Control Plane ────────────────┐
│  API Server（FastAPI 或 Hono）                  │
│  - /v1/agents（版本化配置，append-only versions）│
│  - /v1/environments                             │
│  - /v1/sessions + /events（SSE）                │
│  - /v1/vaults（AES-256-GCM，write-only secrets）│
│  Postgres：agents/versions/sessions/events 表    │
│  （events 表即事件溯源存储，list=回放，stream=tail）│
└──────────────────────┬────────────────────────┘
                       │ 调度
┌──────────────────────▼────────────────────────┐
│  Orchestrator（agent loop worker）              │
│  - 每 session 一个 loop 实例（ durable ）        │
│  - 模型调用 + 工具路由 + compaction + caching    │
│  - 工具 call → 下发到 sandbox，结果回灌事件流    │
└──────────────────────┬────────────────────────┘
                       │ gRPC/HTTP
┌──────────────────────▼────────────────────────┐
│  Sandbox Pool（直接复用，别自造）                │
│  首选：E2B 自托管（Firecracker microVM）         │
│  备选：Docker + gVisor/Kata（便宜一档）          │
└────────────────────────────────────────────────┘
```

关键设计决策（从 CMA 抄作业）：
- **事件表是 single source of truth**：SSE 流只是事件表的 tail 视图，断线重连 = 重放 + 去重
- **凭证代理模式**：MCP/git 调用路由经 orchestrator 侧代理注入 token，沙箱内零 secret
- **stream-first 约定**写进 SDK 而不是靠用户自觉
- session 状态机采用 born `idle` 的读法：`idle → running ⇄ idle → terminated` + `stop_reason`；`rescheduling` 仅保留为 kick 在途的瞬时态

### 3.3 MVP 范围（4-6 周，1-2 人）

| 里程碑 | 内容 | 验证标准 |
|---|---|---|
| M1（1 周） | Postgres schema + agents/sessions/events 三个资源的 CRUD + agent 版本化 | API 测试通过 |
| M2（1.5 周） | agent loop worker：tool-use 循环 + bash/read/write/edit 四工具 + Docker 单容器沙箱 | 一个 session 端到端跑完"读文件→改文件→跑命令" |
| M3（1 周） | SSE 事件流 + `events.list` 回放 + `processed_at` + idle/stop_reason 状态机 | 断流重连测试：杀客户端重连不丢事件、pending tool 不死锁 |
| M4（1 周） | E2B 替换 Docker 沙箱 + 网络策略（unlimited/allowed_hosts） | 沙箱逃逸测试用例 |
| M5（0.5-1 周） | vault + MCP 代理注入 + TS/Python SDK 雏形 | 端到端：agent 调 MCP 服务，容器内无 secret |

**明确不做**（第一版）：multiagent threads、outcomes 评分循环、memory store、webhooks。这些是 P1 以后的事，CMA 自己也是分阶段上的。

### 3.4 三条路线对比

| 路线 | 成本 | 收益 | 我的判断 |
|---|---|---|---|
| A. 全新自研 | 最高（MVP 4-6 周 + sandbox 持续投入） | 完全可控，差异化最大 | 除非有明确差异化定位，否则不值 |
| B. fork/贡献 open-managed-agents | 低 | 站在 387 个测试的肩膀上；但 AGPL + 个人项目风险 | **想快速有东西就用这个**，先给作者提 PR 探探活跃度 |
| C. 拼装（OpenHands runtime/E2B + 自研 control plane） | 中 | sandbox 拿成熟的，自己只做有差异化的编排层和治理层 | **长期最务实**，也是我认为 Warren 该走的路 |

---

## 4. 结论

- Managed Agents 的技术本质是**清晰的工程**而不是黑科技：四个对象、一个 loop、一条事件流。拆开看每一块都有成熟开源对应物。
- 它真正的壁垒是**平台工程的完成度**：sandbox 调度、凭证代理、durable 事件语义、compaction。这些加起来是「能跑 demo」和「扛得住生产」的区别。
- 开源世界已经把这件事做了七七八八。建议先花一天跑通 open-managed-agents 和 OpenHands，确认缺口再决定 fork、拼装还是自研。**别从写 agent loop 开始——那是这个项目里最不值钱的部分。**

## 来源

- [open-managed-agents (GitHub)](https://github.com/rogeriochaves/open-managed-agents)
- [TrueForge 发布报道 (The New Stack)](https://thenewstack.io/truefoundry-trueforge-claude-managed-agents/)
- [Multica 介绍 (faun.pub)](https://faun.pub/the-open-source-claude-managed-agents-alternative-is-here-meet-multica-7035cca69a5d)
- [Claude Managed Agents Alternatives (Layer3 Labs)](https://www.layer3labs.io/comparisons/claude-managed-agents-alternatives)
- [Comparing Open-Source AI Agent Frameworks (Langfuse)](https://langfuse.com/blog/2025-03-19-ai-agent-comparison)
- [AI Agent Sandbox Technologies: 2026 Comparison (grigio.org)](https://grigio.org/ai-agent-sandbox-technologies-a-complete-2026-comparison/)
- [Daytona vs E2B in 2026 (Northflank)](https://northflank.com/blog/daytona-vs-e2b-ai-code-execution-sandboxes)
- [Best sandboxes for coding agents in 2026 (Northflank)](https://northflank.com/blog/best-sandboxes-for-coding-agents)
- [E2B vs Daytona vs Modal vs Docker (Towards AI)](https://pub.towardsai.net/e2b-vs-daytona-vs-modal-vs-docker-how-ai-agent-sandboxes-actually-differ-bd1ea9bb3333)
- [AI Code Sandbox Benchmark 2026 (superagent.sh)](https://www.superagent.sh/blog/ai-code-sandbox-benchmark-2026)
- [claude-agent-sdk-python (GitHub)](https://github.com/anthropics/claude-agent-sdk-python)
- [claude-agent-sdk-typescript (GitHub)](https://github.com/anthropics/claude-agent-sdk-typescript)
- [Agent SDK overview (Claude Code Docs)](https://code.claude.com/docs/en/agent-sdk/overview)
- [Anthropic Managed Agents vs Open-Source Frameworks (MindStudio)](https://www.mindstudio.ai/blog/anthropic-managed-agents-vs-open-source-frameworks-comparison)
- Anthropic 官方 Managed Agents 文档（platform.claude.com，经 API skill 缓存：overview / core / events / environments / tools / self-hosted-sandboxes / memory / multiagent / outcomes / webhooks）
