# Multica × 飞书集成设计方案

## 1. 背景与目标

multica 是一个轻量化的研发协同 control plane，本身只做任务编排与状态管理，把代码执行下沉到开发者本机的 daemon 与本地编程 agent（Claude Code、Codex 等）。本方案在不破坏这个 control plane 模型的前提下，把飞书引入到协同链路里。

**目标分两步：**
- **第一步**：把 multica 的任务与状态信息同步到飞书。
- **第二步**：让飞书反向触发 multica 的基础交互。

**最终愿景**：项目管理（multica）、代码开发（cc / codex）、信息沟通（飞书）三者闭环联动，但所有 IM 复杂度只在 multica server 这一层处理。

## 2. 范围

**纳入范围：**
1. multica 事件 → 飞书群通知（带可交互卡片）
2. 卡片按钮回写（claim / mark done / snooze）
3. 任务上下文里的飞书文档自动展开
4. 同步会议创建（人触发）
5. 飞书 thread → multica issue（@bot 结构化动作）
6. 中途澄清问答桥（agent ↔ 飞书 thread，经 server 中转）
7. multica web 端的飞书配置 UI

**显式排除：**
- 自由文本派活（"@bot 帮我修个登录 bug"这类 NLU）
- agent 主动对接飞书（绕过 server 的本地 CLI 调用）
- 飞书文档双向编辑（multica 状态自动回写文档表格）
- 会议纪要自动转任务
- daemon 侧的 lark-cli skill

## 3. 核心架构原则

> **multica server 是 IM-aware 层；daemon / agent runtime / coding CLI 对飞书完全无感知。**

这是整套设计的脊梁。守住这条原则，三个直接收益：

1. **agent 不需要学新协议** —— 它只看到任务描述、issue、comment 这些 multica 原生概念。
2. **daemon 不需要装 lark-cli 也不需要 Node.js** —— 部署面不变。
3. **未来换 IM（钉钉、Slack）只动 server 的一层文件** —— 集成与编排解耦。

### 三类角色分层

```
agent runtime（同级）：    codex, claude-code, gemini, ...
external system（同级）：  GitHub, Lark, Jira, ...
capability tool（同级）：  gh, lark-cli, jq, ...（当前 scope 不暴露）
```

飞书在架构里**与 GitHub 同级**（external system），与 codex / cc **不同级**。multica server 通过 [oapi-sdk-go](https://github.com/larksuite/oapi-sdk-go) 直接对接，生产路径上不出现 lark-cli。

## 4. 需求清单

| 需求 | 用户角色 | 触发方 | 落地形式 |
|---|---|---|---|
| 任务创建/分配/完成时群里有通知 | 团队成员 | multica → Lark | 卡片消息 |
| 在飞书群里直接 claim / 标完成 | assignee | Lark → multica | 卡片按钮 |
| 任务描述里贴 Lark 文档 URL，agent 自动看到内容 | 开发者 | multica 内部 | 派发前文档抓取 |
| 在 multica 一键给项目相关人建同步会议 | 项目负责人 | multica → Lark | 按钮 + 日历 API |
| 在飞书 thread 里讨论完直接转 issue | PM / Tech Lead | Lark → multica | @bot 结构化动作 |
| agent 卡住要澄清，问题自动到飞书，回复自动回流 | 开发者 + agent | 双向 | comment 桥 |
| workspace 管理员绑定群、勾选事件 | admin | UI | multica web settings |
| 每个用户一次性绑定自己的飞书账号 | 用户 | UI | OAuth |

## 5. 架构与文件结构

### 5.1 server 端新增/扩展

```
server/
├── internal/
│   ├── handler/
│   │   ├── github.go              (existing)
│   │   ├── lark.go                (new) — webhook 入口：事件订阅 + 卡片回调 + @bot
│   │   └── lark_settings.go       (new) — workspace 绑定 CRUD + 用户 OAuth 回调
│   └── service/
│       ├── email.go               (existing — 模板参考)
│       ├── lark_notify.go         (new) — 订阅 event bus，渲染卡片，调 Lark API
│       ├── lark_docs.go           (new) — 文档抓取
│       ├── lark_meeting.go        (new) — 日历事件创建
│       └── lark_thread.go         (new) — thread ↔ issue 桥接（comment 双向）
├── migrations/
│   └── NNNN_lark.sql              (new) — 三张表（见 5.2）
└── pkg/protocol/
    └── events.go                  (extend) — Lark 相关事件常量
```

### 5.2 数据模型

```sql
-- workspace ↔ 群绑定
CREATE TABLE lark_workspace_binding (
    workspace_id      UUID PRIMARY KEY REFERENCES workspaces(id),
    chat_id           TEXT NOT NULL,
    bot_token_enc     BYTEA NOT NULL,
    enabled_events    TEXT[] NOT NULL DEFAULT '{}',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- multica 用户 ↔ 飞书用户
CREATE TABLE lark_user_link (
    user_id           UUID PRIMARY KEY REFERENCES users(id),
    lark_open_id      TEXT NOT NULL UNIQUE,
    refresh_token_enc BYTEA,
    linked_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- issue ↔ thread（双向桥的关键状态）
CREATE TABLE lark_issue_link (
    issue_id          UUID PRIMARY KEY REFERENCES issues(id),
    chat_id           TEXT NOT NULL,
    root_message_id   TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

可选 phase-2b 加 `lark_action_log` 做审计。

### 5.3 配置

| 项 | 位置 | 说明 |
|---|---|---|
| `LARK_APP_ID` | env | 应用凭证 |
| `LARK_APP_SECRET` | env | 应用凭证 |
| `LARK_VERIFICATION_TOKEN` | env | webhook 签名校验 |
| `LARK_ENCRYPT_KEY` | env | 加密订阅消息 |
| workspace 绑定 | DB + UI | admin 配置 |
| 用户绑定 | DB + UI | 用户一次性 OAuth |

凭证一律走 env：自托管场景顺、泄露面小、不必专做加密存储逻辑。多租户 SaaS 化时再考虑提升为 UI。

## 6. 模块详细设计

### 6.1 出站通知 — `service/lark_notify.go`

订阅现有 event bus 的几个事件，渲染成卡片发到绑定群：

| 事件 | 卡片内容 | 含按钮 |
|---|---|---|
| `EventTaskCreated` | 标题 + 描述摘要 + 创建人 | `认领` |
| `EventTaskAssigned` | 任务 + assignee + 链接 | `查看` |
| `EventTaskCompleted` | 任务 + PR 链接 | `打开 PR` |
| `EventTaskFailed` | 任务 + 错误摘要 | `重试` `转人工` |
| `EventIssueCommented` | comment 内容（仅 mention 时） | `回复` |

卡片模板**硬编码在 Go 里**，不做 DSL。enabled_events 字段控制每种事件是否推送。

### 6.2 卡片回写 — `handler/lark.go`

路由：`POST /api/webhooks/lark`，照 `handler/github.go` 形状：
1. challenge 握手（首次配置）
2. 签名校验
3. 按 `event_type` / `action_type` 分发

第二步只处理结构化 callback：

```
action.value = { "verb": "claim" | "mark_done" | "snooze", "issue_id": "..." }
```

通过 `lark_user_link` 把点击人映射回 multica user，直接走现有 issue API 改状态、publish 事件，循环回出站通知链路。**未绑账号的用户点按钮 → 卡片提示去绑定**，不做猜测。

### 6.3 文档消费 — `service/lark_docs.go`

唯一暴露的接口：

```go
func FetchDocContent(ctx context.Context, url string) (string, error)
```

调用时机：**任务派发前**，扫描 issue body + comments，发现 Lark 文档 URL 就抓正文，拼到任务上下文里。agent 看到的就是纯文本任务描述。

策略：
- 不缓存（doc reads 不是热路径）。
- 任务 resume / 重派时重抓，自动获得最新版本。
- 抓不到（权限不足、文档删除）→ 在任务上下文里留一行 `[doc unavailable: <url>]`，不阻塞派发。

### 6.4 会议创建 — `service/lark_meeting.go`

唯一接口：

```go
func CreateMeeting(ctx context.Context, issueID uuid.UUID, opts MeetingOpts) error
```

UI 入口：issue 详情页 "安排同步会议" 按钮。参与人 = assignees + watchers。建完之后：
- 会议链接作为 comment 留在 issue 上
- 同时回贴到关联的 Lark thread（如有 `lark_issue_link`）

**只支持人触发**。agent 没有这个能力。

### 6.5 thread → issue — `handler/lark.go` 的 @bot 分支

在飞书 thread 里 `@bot 创建任务`（结构化 verb，不是 NLU）：
1. 抓 thread 标题 + 最近 N 条消息正文作为 issue 描述
2. 创建 multica issue
3. **写入 `lark_issue_link`** 记录 thread root message id（后续问答桥要靠它）
4. 回贴 thread："已创建 multica-1234"

支持的 verb 列表硬编码：`创建任务` / `link-doc` / `open-meeting`。其他文本不响应（避免误触）。

### 6.6 中途澄清桥 — `service/lark_thread.go`

**这是整套设计里最值钱也最容易写歪的一块**。守原则：

- agent 端走 multica 现有的 **comment 机制**（`handler/comment.go` + `mention/expand.go` 已经在）
- 桥逻辑全部在 server 端，agent 一行不改、daemon 一行不改

流程：

```
agent 写 comment "Q: 这个字段应该用 UUID 还是 ULID?"
    │
    ▼ (订阅 EventIssueCommented，过滤出 agent 提问类)
service/lark_thread.go:
    通过 lark_issue_link 找到对应 thread，
    把 Q 贴成 thread 回复
    │
    ▼ (人在 Lark 回复)
handler/lark.go webhook:
    收到 reply event，
    通过 lark_issue_link 反向找到 issue，
    把回复落成 multica issue 的 comment "A: ..."
    并 @ 原 agent
    │
    ▼
agent 在自己的 comment 流里看到回答，继续干
```

关键设计点：
- agent **不知道飞书存在**，它的语义只是 "我写 comment、我等 comment"
- thread context 用 `lark_issue_link.root_message_id` 一张表搞定，不引入复杂的会话状态机
- 没有 `lark_issue_link` 的 issue（即 issue 不是从 thread 来的）→ agent 提问就落到默认绑定群，不开新 thread

### 6.7 配置 UI — `handler/lark_settings.go`

照 `handler/github.go` 暴露的 endpoint 形状对称：

| Endpoint | 用途 |
|---|---|
| `POST /api/workspaces/{id}/lark/connect` | 启动 OAuth、绑定群 |
| `GET /api/workspaces/{id}/lark/binding` | 查询当前绑定 |
| `PATCH /api/workspaces/{id}/lark/binding` | 改 enabled_events |
| `DELETE /api/workspaces/{id}/lark/binding` | 解绑 |
| `POST /api/users/me/lark/link` | 用户级 OAuth 起点 |
| `GET /api/users/me/lark/link/callback` | OAuth 回调 |
| `DELETE /api/users/me/lark/link` | 解绑个人账号 |

multica web 端新增两个页面：
- **Workspace Settings → Integrations → Lark**（admin 用）
- **User Profile → Linked Accounts**（用户用）

## 7. 端到端闭环

```
[Lark thread: 讨论 SSO 需求]
   └─ @bot 创建任务 ───────────► multica issue（lark_issue_link 写入）
                                       │
                                       ├─ 描述里有 Lark 文档 URL
                                       │   └─ server 拉文档塞进上下文
                                       ▼
                                  multica 派给 Codex
                                       │
                                       ├─ agent 写 "Q: ..." comment
                                       │   └─ 桥到原 thread → 人回 → 落 comment → 继续
                                       │
                                       ├─ 需要对齐 → 人在 multica 点 "开会"
                                       │   └─ server 建 Lark 日历 → 回贴 thread
                                       ▼
                                  PR 出来 → 卡片通知回 thread
                                       │
                                       └─ assignee 在 Lark 点 "查看 PR" 按钮
```

整个闭环里 daemon / agent / coding CLI 一行不动。所有 IM 翻译集中在 `service/lark_*.go` + `handler/lark.go`。

## 8. 安全设计

| 风险点 | 防御 |
|---|---|
| webhook 伪造 | `LARK_VERIFICATION_TOKEN` 签名校验，照 github.go |
| 凭证泄露 | App credentials 走 env，bot token 加密存 DB |
| 用户冒用 | 卡片按钮必须 `lark_user_link` 已建立才生效，否则提示绑定 |
| Bot 越权 | bot 只能往 `lark_workspace_binding.chat_id` 发，不做群发现 |
| 文档越权 | 文档抓取用任务创建者的 OAuth token，权限由飞书侧决定 |
| 审计缺失 | phase-2b 加 `lark_action_log`，IM 触发的写动作全记录 |

**注意**：因为 agent 不直接接触飞书（comment 桥模型），**不需要单独的 agent 写动作 confirm gate**。agent 的所有外部影响都通过 multica 的 comment 系统过一遍，沿用已有的人工审阅模型。

## 9. 分阶段路线

| Phase | 内容 | 依赖 |
|---|---|---|
| **P1** | 出站通知（`lark_notify.go`） + workspace 绑定 UI | — |
| **P2** | 卡片回写（`handler/lark.go` 卡片分支） + 用户 OAuth UI | P1 |
| **P3** | 文档消费（`lark_docs.go`） + 会议创建（`lark_meeting.go`） | P1 |
| **P4** | thread → issue（`handler/lark.go` @bot 分支） + `lark_issue_link` 表 | P2 |
| **P5** | 澄清问答桥（`lark_thread.go`） | P1 + P2 + P4 |

P3 / P4 可并行。**P5 不能提前**，它依赖前面几个的链路稳定。

## 10. 显式延期项（写进 RFC，避免被翻出来争论）

| 延期项 | 解锁条件 |
|---|---|
| 自由文本派活（NLU dispatcher） | P4 跑稳之后，且团队真的反馈结构化 verb 不够用 |
| agent 直接调 lark-cli（daemon-side skill） | 出现 multica 中转走不通的真实场景 |
| 飞书文档双向编辑（状态自动回写） | PM 真的在文档表格里追状态并抱怨手动维护 |
| 会议纪要自动转任务 | 团队真在用飞书智能纪要 |
| 个人 Lark 身份发消息（每人 token） | bot 身份发消息被嫌弃成噪音 |
| 通用 IM adapter 抽象 | 第二家 IM（钉钉 / Slack）真要接入 |

## 11. 不变量（实现里要持续守护）

1. `daemon/` 目录在本方案里**一行不改**。任何要求改 daemon 的需求 → 触发 RFC review。
2. 飞书集成走 **Go 进程内 HTTP**（P1 直接用 `net/http`，后续可升级到 `oapi-sdk-go`），**禁止在生产路径上 spawn lark-cli 子进程**。一次性脚本、运维工具不在此限。
3. agent 端**不接触飞书概念**。所有 IM 桥逻辑只出现在 `service/lark_*.go` 和 `handler/lark.go`。
4. **没有 NLU**。所有飞书 → multica 的写动作都是结构化 verb 或卡片按钮。
5. 飞书 HTTP 调用**永远不能阻塞同步事件总线**。`service/lark_notify.go` 在 P1 用一个 bounded worker channel；总线协程只负责入队，飞书不可用时溢出的事件丢弃 + WARN，**禁止反压 issue / comment / task 写路径**。

### 11.1 P1 实现说明

- **Lark 客户端**：P1 用 `net/http` 直接打 `auth/v3/tenant_access_token/internal` 和 `im/v1/messages` 两个稳定接口（约 70 行），不引入 `oapi-sdk-go`。P3（文档抓取）/ 后续会议 / 卡片回写需要更广的 API 表面时再切到 SDK；接口形状已经按 `LarkClient` interface 化，切换无侵入。
- **enabled_events 词汇**：直接复用 `protocol.EventXxx` 常量（`issue:created` 等）。UI checklist、DB 列、event bus 同一套字符串，不引入翻译表。
- **`bot_token_enc` 字段**：P1 不写入（用 app-level tenant token）。schema、AES-GCM 加解密、`LARK_ENCRYPT_KEY` 全部到位，留给 P2 用户 OAuth + 群级 bot token。

---

这套方案把"PM + 代码 + IM 三方联动"的需求做到了：multica 加 5 个文件、daemon 0 改动、agent 0 改动、未来换 IM 改动面也只在这 5 个文件。规模与价值对得上，没有为未来的可能性现在就买单。
