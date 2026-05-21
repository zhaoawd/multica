# Multica × 飞书集成设计方案

## 1. 背景与目标

multica 是一个轻量化的研发协同 control plane，本身只做任务编排与状态管理，把代码执行下沉到开发者本机的 daemon 与本地编程 agent（Claude Code、Codex 等）。本方案在不破坏这个 control plane 模型的前提下，把飞书引入到协同链路里。

**目标分两条通道：**
- **团队模式**：飞书群承载需求讨论、共识形成、公共阻塞和 thread → issue 闭环。
- **个人模式**：飞书私聊承载个人行动队列，类似"文件助手"里的 multica inbox。

**最终愿景**：项目管理（multica）、代码开发（cc / codex）、信息沟通（飞书）三者闭环联动，但团队群不被个人执行流淹没，所有 IM 复杂度只在 multica server 这一层处理。

## 2. 范围

**纳入范围：**
1. multica 事件 → 飞书团队群 / 个人私聊的分流通知（带可交互卡片）
2. 卡片按钮回写（claim / snooze / retry）
3. 任务上下文里的飞书文档自动展开
4. 同步会议创建（人触发）
5. 飞书 thread → multica issue（@bot 结构化动作）
6. 中途澄清问答桥（agent ↔ 飞书 thread 或个人私聊，经 server 中转）
7. multica web 端的飞书配置 UI
8. 个人飞书私聊通知偏好和测试消息

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

飞书在架构里**与 GitHub 同级**（external system），与 codex / cc **不同级**。multica server 通过 Go 进程内 HTTP client 直接对接；API 表面扩大后可切到 [oapi-sdk-go](https://github.com/larksuite/oapi-sdk-go)。生产路径上不出现 lark-cli。

## 4. 需求清单

| 需求 | 用户角色 | 触发方 | 落地形式 |
|---|---|---|---|
| 群里讨论完直接转 issue | PM / Tech Lead | Lark → multica | thread 内 @bot 结构化动作 |
| 未分配任务在团队群可被认领 | 团队成员 | multica → Lark | 群卡片 `认领` |
| 已分配事项只通知相关个人 | assignee / watcher | multica → Lark | 个人私聊卡片 |
| 个人在飞书里处理自己的任务 | assignee | Lark → multica | 私聊卡片按钮 |
| 任务描述里贴 Lark 文档 URL，agent 自动看到内容 | 开发者 | multica 内部 | 派发前文档抓取 |
| 在 multica 一键给项目相关人建同步会议 | 项目负责人 | multica → Lark | 按钮 + 日历 API |
| agent 卡住要澄清，问题自动投到正确上下文 | 开发者 + agent | 双向 | thread / 私聊 comment 桥 |
| workspace 管理员绑定群、设置团队事件 | admin | UI | multica web settings |
| 每个用户一次性绑定自己的飞书账号和私聊偏好 | 用户 | UI | OAuth + preferences |

### 4.1 团队模式与个人模式

| 模式 | 设计目标 | 承载内容 | 禁止承载 |
|---|---|---|---|
| 团队模式 | 保留群作为需求讨论和团队共识场 | thread → issue、未分配任务、公共阻塞、会议同步、thread 内创建确认与澄清桥 | 已分配给个人的常规状态变化 |
| 个人模式 | 把飞书私聊作为个人行动队列 | assigned、mention、agent 提问、可选 task failed/done、snooze、个人摘要 | 团队讨论广播 |

路由原则：**团队群只放需要团队共同注意的信号；个人私聊放需要某个人行动的信号。**

| multica 事件 | 团队群 | 个人私聊 |
|---|---|---|
| `issue:created`，来源为 Lark thread | 回贴原 thread，附 issue 链接 | 通知 creator |
| `issue:created`，无 assignee | 可发群认领卡片 | 可选通知 owner/watchers |
| `issue:updated`，assignee 变化 | 默认不发群 | 通知新 assignee |
| `task:completed` | 不发 | 通知 assignee / creator（默认关） |
| `task:failed` | 仅 issue 无 assignee 时发群 | 通知 assignee / creator（默认关） |
| `comment:created`，@mention | 默认不发群 | 通知被 mention 人 |
| agent 澄清问题 | 有 `lark_issue_link` 时 bot 消息回原 thread | 无 thread 时通知 assignee / owner |
| 会议创建 | 回贴原 thread（如有） | 通知参会人 |

团队侧路由优先级：thread 只承载两类消息——issue **创建确认**和**澄清桥转发**。完成、失败、状态变更一律不回贴 thread。群顶部认领卡只用于无 Lark thread 来源的未分配 issue。

## 5. 架构与文件结构

### 5.1 server 端新增/扩展

```
server/
├── internal/
│   ├── handler/
│   │   ├── github.go              (existing)
│   │   ├── lark.go                (new) — webhook 入口：事件订阅 + 卡片回调 + @bot
│   │   └── lark_settings.go       (new) — workspace 绑定 CRUD + 用户 OAuth / 偏好回调
│   └── service/
│       ├── email.go               (existing — 模板参考)
│       ├── lark_notify.go         (new) — 订阅 event bus，路由到群 / 私聊，调 Lark API
│       ├── lark_docs.go           (new) — 文档抓取
│       ├── lark_meeting.go        (new) — 日历事件创建
│       └── lark_thread.go         (new) — thread ↔ issue 桥接（comment 双向）
├── migrations/
│   └── NNNN_lark.sql              (new) — Lark 绑定、偏好、桥接、审计表（见 5.2）
└── pkg/protocol/
    └── events.go                  (extend) — Lark 相关事件常量
```

### 5.2 数据模型

```sql
-- workspace ↔ 团队群绑定。bot_token_enc 在 P1 为空，使用 app-level
-- tenant_access_token；后续需要群级 token 时再写入。
CREATE TABLE lark_workspace_binding (
    workspace_id      UUID PRIMARY KEY REFERENCES workspace(id),
    chat_id           TEXT NOT NULL,
    bot_token_enc     BYTEA NOT NULL,
    enabled_events    TEXT[] NOT NULL DEFAULT '{}',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- multica 用户 ↔ 飞书用户
CREATE TABLE lark_user_link (
    user_id           UUID PRIMARY KEY REFERENCES "user"(id),
    lark_open_id      TEXT NOT NULL UNIQUE,
    refresh_token_enc BYTEA,
    dm_enabled        BOOLEAN NOT NULL DEFAULT TRUE,
    linked_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 用户通知偏好
CREATE TABLE lark_notification_pref (
    user_id           UUID NOT NULL REFERENCES "user"(id),
    workspace_id      UUID NOT NULL REFERENCES workspace(id),
    event_kind        TEXT NOT NULL,
    channel           TEXT NOT NULL CHECK (channel IN ('dm', 'digest')),
    enabled           BOOLEAN NOT NULL DEFAULT TRUE,
    PRIMARY KEY (user_id, workspace_id, event_kind, channel)
);

-- issue ↔ thread（双向桥的关键状态）。同一个 thread 可拆出多个 issue。
CREATE TABLE lark_issue_link (
    id                UUID PRIMARY KEY,
    issue_id          UUID NOT NULL REFERENCES issue(id),
    chat_id           TEXT NOT NULL,
    root_message_id   TEXT NOT NULL,
    source_scope      TEXT NOT NULL DEFAULT 'thread',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX lark_issue_link_issue_id_idx
    ON lark_issue_link (issue_id);
CREATE UNIQUE INDEX lark_issue_link_unique_thread_issue_idx
    ON lark_issue_link (chat_id, root_message_id, issue_id);
CREATE INDEX lark_issue_link_thread_idx
    ON lark_issue_link (chat_id, root_message_id);

-- IM 写动作审计
CREATE TABLE lark_action_log (
    id                UUID PRIMARY KEY,
    workspace_id      UUID NOT NULL REFERENCES workspace(id),
    user_id           UUID REFERENCES "user"(id),
    lark_open_id      TEXT,
    channel           TEXT NOT NULL CHECK (channel IN ('dm', 'team', 'thread')),
    verb              TEXT NOT NULL CHECK (verb IN (
        'claim', 'snooze', 'retry', 'create_issue',
        'link_doc', 'open_meeting',
        -- §13 HITL 阶段化
        'approve_plan', 'enter_testing', 'verify'
    )),
    issue_id          UUID REFERENCES issue(id),
    message_id        TEXT,
    result            TEXT NOT NULL CHECK (result IN ('success', 'ignored', 'failed')),
    error             TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- webhook 幂等日志
CREATE TABLE lark_webhook_event_log (
    event_id          TEXT PRIMARY KEY,
    event_type        TEXT NOT NULL,
    message_id        TEXT,
    processed_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 可靠投递 outbox（个人私聊高信号事件）
CREATE TABLE lark_delivery_log (
    id                UUID PRIMARY KEY,
    workspace_id      UUID NOT NULL REFERENCES workspace(id),
    user_id           UUID REFERENCES "user"(id),
    channel           TEXT NOT NULL CHECK (channel IN ('dm', 'team', 'thread')),
    event_kind        TEXT NOT NULL,
    target_id         TEXT NOT NULL,
    payload_json      JSONB NOT NULL,
    status            TEXT NOT NULL DEFAULT 'pending',
    attempt           INT NOT NULL DEFAULT 0,
    next_attempt_at   TIMESTAMPTZ,
    last_error        TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 个人 snooze 状态。每条记录对应某个用户对某个 issue / reminder 的延后处理。
CREATE TABLE lark_snooze (
    id                UUID PRIMARY KEY,
    user_id           UUID NOT NULL REFERENCES "user"(id),
    workspace_id      UUID NOT NULL REFERENCES workspace(id),
    issue_id          UUID REFERENCES issue(id),
    event_kind        TEXT NOT NULL,
    channel           TEXT NOT NULL DEFAULT 'dm' CHECK (channel = 'dm'),
    wake_at           TIMESTAMPTZ NOT NULL,
    status            TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'sent', 'cancelled')),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX lark_snooze_due_idx
    ON lark_snooze (wake_at)
    WHERE status = 'pending';

-- P6 需要扩展 comment.type，避免用文本启发式识别 agent 提问。
-- §13 HITL 阶段化继续追加 *_proposed / plan_approved / verified。
ALTER TABLE comment DROP CONSTRAINT comment_type_check;
ALTER TABLE comment ADD CONSTRAINT comment_type_check
    CHECK (type IN (
        'comment', 'clarification', 'status_change', 'progress_update', 'system',
        -- §13 HITL：agent 产出
        'plan_proposed', 'code_proposed', 'tests_proposed',
        -- §13 HITL：server 代人写入
        'plan_approved', 'verified'
    ));

-- §13 HITL：issue 阶段（观察值 / filter 字段，不是状态机闸门）。
ALTER TABLE issue ADD COLUMN stage TEXT
    CHECK (stage IS NULL OR stage IN
        ('planning', 'developing', 'testing', 'verifying', 'done'));
```

`lark_action_log` 不延期。所有来自飞书卡片或 @bot 的写动作都跨系统，必须可审计。

### 5.3 配置

| 项 | 位置 | 说明 |
|---|---|---|
| `LARK_APP_ID` | env | 应用凭证 |
| `LARK_APP_SECRET` | env | 应用凭证 |
| `LARK_VERIFICATION_TOKEN` | env | webhook 签名校验 |
| `LARK_ENCRYPT_KEY` | env | 加密订阅消息 |
| workspace 绑定 | DB + UI | admin 配置 |
| 用户绑定 | DB + UI | 用户一次性 OAuth，保存 open_id 供私聊投递 |
| 个人偏好 | DB + UI | 控制个人私聊事件 |

凭证一律走 env：自托管场景顺、泄露面小、不必专做加密存储逻辑。多租户 SaaS 化时再考虑提升为 UI。

## 6. 模块详细设计

### 6.1 出站通知 — `service/lark_notify.go`

订阅现有 event bus 后做路由判定，再渲染不同形态的卡片。出站通知按协作语义分流到团队群、原 thread 或个人私聊，禁止采用"事件默认进群"。

**卡片操作模型收敛**：每张卡最多一个写动作按钮，`查看` 全去掉——整卡 card-level url 跳 multica。"飞书只承载需要人马上理解/行动的最小交互，状态消费和任务完成都回 multica"。

团队群卡片：

| 事件 | 投递条件 | 卡片内容 | 按钮 |
|---|---|---|---|
| `EventIssueCreated` | issue 无 assignee 且无 thread 来源 | 标题 + 描述摘要 + 创建人 | `认领` |
| `EventIssueCreated`（thread 来源） | thread 内 `@bot 创建任务` | 简短确认 + issue 链接 | 无 |
| `EventTaskFailed` | 公共阻塞（issue 无 assignee + failed） | 任务 + 错误摘要 | 无 |
| meeting created | 人触发，且有团队上下文 | 会议时间 + issue 链接 | `打开会议` |

个人私聊卡片：

| 事件 | 接收人 | 卡片内容 | 按钮 |
|---|---|---|---|
| `EventIssueUpdated` assignee changed | 新 assignee | 任务 + 来源 + due date | `稍后提醒` |
| `EventTaskCompleted`（默认关） | assignee / creator | 任务 + PR / 结果链接 | 无 |
| `EventTaskFailed`（默认关） | assignee / creator | 错误摘要 + issue 链接 | `重试` |
| `EventIssueCommented` mention | 被 mention 人 | comment 摘要 | 无 |
| agent clarification | assignee / owner | agent 问题 + issue 链接 | 无（飞书原生回复） |

卡片模板和路由规则都**硬编码在 Go 里**，不做 DSL。`enabled_events` 和 `lark_notification_pref.event_kind` 只表达粗粒度订阅意愿，不能决定通道；实际通道由 `service/lark_notify.go` 的路由函数根据 payload 条件判定。个人私聊使用 `lark_user_link.lark_open_id` 作为接收者；飞书发送接口使用 `receive_id_type=open_id`，需要机器人具备给用户发消息的权限。

硬编码路由规则：
- `issue:created`：有 `lark_issue_link` → thread 回贴一次创建确认；无 assignee 且无 thread → team；有 assignee → dm。
- `issue:updated`：只有 `assignee_changed=true` 进入 dm；其他 update 默认静默。
- `task:failed`：满足公共阻塞 → team；其他失败 → dm（默认关）。
- `task:completed`：一律走 dm（默认关），不回 thread、不进群。
- `comment:created`：`comment.type='clarification'` 才进入澄清桥；普通 mention 只进被 mention 人 dm。

公共阻塞判定**只认一个条件**：issue 无 assignee 且 task failed。其他场景（手动 `blocked`、daemon failure_reason、自定义 payload 标记）一律走个人 DM——让团队群只承载"没人能接，需要团队接手"这一种共同行动。

默认个人偏好：OAuth 绑定后**只开启** `assigned` + `agent_clarification`；其他事件（`task_failed`、`task_completed`、普通 mention、daily digest）默认关闭——daemon 会自动 retry，连续失败升级为 clarification 时人才需要被打扰。用户可在 Linked Accounts 里 opt-in 更多事件。

### 6.2 卡片回写 — `handler/lark.go`

路由：`POST /api/webhooks/lark`，照 `handler/github.go` 形状：
1. challenge 握手（首次配置）
2. 签名校验
3. 按 `event_type` / `action_type` 分发

第二步只处理结构化 callback。卡片按钮按来源分为团队卡片和个人卡片：

```
action.value = {
  "verb": "claim" | "snooze" | "retry",
  "issue_id": "...",
  "source_channel": "team" | "dm"
}
```

通过 `lark_user_link` 把点击人映射回 multica user，走现有 issue / comment API 改状态、publish 事件。**未绑账号的用户点按钮 → 卡片提示去绑定**，不做猜测。

**澄清桥不走 action_type**：用户对 bot 澄清消息使用飞书原生 reply 语义，命中 `handler/lark.go` 的 message reply event 分支，不走 card callback。详见 6.6。

并发规则：
- `claim` 必须是 CAS：仅允许 `assignee_id IS NULL` 时写入，已被认领就返回"已由 X 认领"。
- `retry` 直接复用 multica 现有的 task retry API，daemon 侧本就幂等。
- `snooze` 只作用于个人提醒，不改变 issue 状态和团队群。
- 所有写动作写入 `lark_action_log`。

`snooze` 交互：
- 单按钮 `稍后提醒`，**固定 1h**，不做档位选择。如果用户后续抱怨再加。
- 点击后写入 `lark_snooze`，`wake_at = now() + 1h`；同一 user + issue + event_kind 的 pending 记录被新 wake_at 覆盖。
- 后台定时任务每分钟扫描到期记录，写入 reliable outbox 重新发送个人 DM。
- issue 完成、取消、用户解绑 Lark 时，将相关 pending snooze 标记为 `cancelled`。

### 6.3 文档消费 — `service/lark_docs.go`

唯一暴露的接口：

```go
func FetchDocContent(ctx context.Context, url string) (string, error)
```

调用时机：**任务派发前**，扫描 issue body + comments，发现 Lark 文档 URL 就抓正文，拼到任务上下文里。agent 看到的就是纯文本任务描述。

策略：
- 不缓存（doc reads 属于非热路径）。
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
- 同时给参会人发个人私聊卡片（可关闭）

**只支持人触发**。agent 没有这个能力。

### 6.5 thread → issue — `handler/lark.go` 的 @bot 分支

在飞书 thread 里 `@bot 创建任务`（结构化 verb，禁用 NLU）：
1. 抓 thread 标题 + 最近 N 条消息正文作为 issue 描述
2. 创建 multica issue
3. **写入 `lark_issue_link`** 记录 chat id + thread root message id + issue id（后续问答桥要靠它）
4. 回贴 thread："已创建 multica-1234"，并附带针对该 issue 的操作卡

支持的 verb 列表硬编码：`创建任务` / `link-doc` / `open-meeting`。其他文本不响应（避免误触）。

同一个 thread 可以拆出多个 issue（schema 允许）。但 v1 入站回复路由不做精确挂载：用 `chat_id + root_message_id` 查 `lark_issue_link`，多条命中时落到**最近创建**的 issue。"哪条澄清对应哪条回复"的精度延期到 v2。

### 6.6 中途澄清桥 — `service/lark_thread.go`

**这是整套设计里最值钱也最容易写歪的一块**。守原则：

- agent 端走 multica 现有的 **comment 机制**（`handler/comment.go` + `mention/expand.go` 已经在）。
- 桥逻辑全部在 server 端，agent 一行不改、daemon 一行不改。
- 飞书侧**完全复用原生 thread / 私聊回复语义**，不发卡片、不放"回复 agent"按钮，不要 `reply` action_type。

流程：

```
agent 写 comment(type="clarification") "这个字段应该用 UUID 还是 ULID?"
    │
    ▼ (订阅 EventIssueCommented，只接收 comment.type=clarification)
service/lark_thread.go:
    有 lark_issue_link → bot 消息回复原 thread
    无 lark_issue_link → bot 私聊发给 assignee / owner
    （bot 消息为纯文本 + issue 链接，无按钮、无卡片）
    │
    ▼ (人**原生回复** bot 那条消息——thread 内 reply 或 DM 内 reply)
handler/lark.go webhook (message reply event):
    thread 场景：用 chat_id + root_message_id 查 lark_issue_link，
                 多条命中取最近创建的 issue。
    DM 场景：    用 lark_user_link 反查接收人最近的 clarification target。
    落成 multica issue comment（type='comment'，不强挂 parent_id），
    @ 原 agent，触发 on_comment。
    │
    ▼
agent 在自己的 comment 流里看到答复，继续干
```

关键设计点：
- agent **不知道飞书存在**，它的语义只是"我写 comment、我等 comment"。
- agent 提问识别必须依赖结构信号 `comment.type='clarification'`。禁止用 `Q:`、问号、关键词等文本启发式。
- **入站不做 `reply` action_type**，server 只认 `lark_issue_link` + thread `root_message_id`（或 DM 的 `lark_user_link`）。用户用飞书的原生 reply 即可，不学新交互。
- **v1 不追求把回复精确挂到某条 clarification comment 上**（即不写 `parent_id` 指向 clarification）。先落成 issue comment 并唤醒 agent，agent 在自己的 comment 流里通过最近 clarification 上下文判断。多澄清并发场景的精度延期到 v2。
- 没有 `lark_issue_link` 的 issue → agent 提问进入个人私聊，接收人为 assignee / owner / 最近触发人，不落默认团队群。
- Lark thread 普通闲聊不落库。只有两类消息会桥回 multica：`@bot 创建任务` / `@bot link-doc` 等结构化 verb，或对 bot 澄清消息的原生 reply。

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
| `GET /api/users/me/lark/preferences` | 查询个人私聊偏好 |
| `PATCH /api/users/me/lark/preferences` | 修改个人私聊事件 |
| `POST /api/users/me/lark/test-message` | 给自己发测试消息 |

multica web 端新增两个页面：
- **Workspace Settings → Integrations → Lark**（admin 用）
- **User Profile → Linked Accounts**（用户用）

UI 要把两个模式显式分开：
- workspace integration 只配置团队群：绑定群、允许进群的公共事件、测试群消息。
- user linked account 配置个人模式：绑定状态、个人事件开关、测试私聊消息、静音/摘要。

### 6.8 投递等级

| 等级 | 适用事件 | 失败策略 |
|---|---|---|
| best-effort | 团队群普通通知、thread 简短回贴 | worker queue 满则丢弃 + WARN |
| reliable | 个人 assigned、agent 澄清、task failed、会议邀请 | 事务内写 `lark_delivery_log`，异步 worker 消费，失败重试 |
| audit-only | 卡片写动作、@bot 结构化写动作 | 成功或失败都写 `lark_action_log` |

分层形态：

```
domain mutation ─► event bus/router ─┬─► best-effort: in-process bounded channel，满了丢弃
                                     └─► reliable:    domain 事务内写 outbox
                                                     worker 异步消费 + 重试
```

P1 可只实现 best-effort。P2 起个人私聊必须具备可靠投递语义，否则个人模式会退化成不可信 inbox。可靠事件的 event bus 只负责唤醒 worker / 刷新 UI，不作为投递事实来源；投递事实来源是 `lark_delivery_log`。

## 7. 端到端闭环

```
[团队群 thread: 讨论 SSO 需求]
   └─ @bot 创建任务 ─────────────► multica issue（lark_issue_link 写入）
                                           │
                                           ├─ 无 assignee：thread / 群里出现认领卡
                                           │
                                           ├─ 被 A 认领或分配给 A
                                           │   └─ 后续状态进入 A 的个人私聊，不刷团队群
                                           │
                                           ├─ agent 写 comment(type=clarification)
                                           │   ├─ issue 来源于 thread：bot 消息回贴原 thread
                                           │   └─ issue 无 thread：bot 消息发到 A 的个人私聊
                                           │
                                           ├─ 人**原生回复** bot 那条消息（thread 或 DM 内 reply）
                                           │   └─ server 落成 issue comment → 唤醒 agent
                                           │
                                           └─ PR / 任务完成
                                               └─ 一律走个人 DM（默认关，可 opt-in）；不回 thread、不进群
```

整个闭环里 daemon / agent / coding CLI 一行不动。所有 IM 翻译集中在 `service/lark_*.go` + `handler/lark.go`。

## 8. 安全设计

| 风险点 | 防御 |
|---|---|
| webhook 伪造 | `LARK_VERIFICATION_TOKEN` 签名校验，照 github.go |
| 凭证泄露 | App credentials 走 env，bot token 加密存 DB |
| 用户冒用 | 卡片按钮必须 `lark_user_link` 已建立才生效，否则提示绑定 |
| Bot 越权 | 团队消息只能往绑定群发，个人消息只能往已绑定用户 open_id 发 |
| 文档越权 | 文档抓取用任务创建者的 OAuth token，权限由飞书侧决定 |
| 审计缺失 | `lark_action_log` 记录所有 IM 触发写动作 |
| 群消息过载 | 路由层默认禁止已分配事项进团队群 |
| 重复 webhook | 以 Lark event/message id 做幂等日志，重复事件直接 ack |

**注意**：因为 agent 不直接接触飞书（comment 桥模型），**不需要单独的 agent 写动作 confirm gate**。agent 的所有外部影响都通过 multica 的 comment 系统过一遍，沿用已有的人工审阅模型。

## 9. 分阶段路线

| Phase | 内容 | 依赖 |
|---|---|---|
| **P1** | 团队群绑定 + 团队事件最小集（thread 回贴、未分配认领、公共失败） | — |
| **P2** | 用户 OAuth + 个人私聊投递 + 个人偏好 UI + 测试私聊 | P1 |
| **P3** | 卡片回写（claim / snooze / retry）+ `lark_action_log` + `lark_snooze`（固定 1h） | P1 + P2 |
| **P4** | 文档消费（`lark_docs.go`） | P2 |
| **P5** | thread → issue + `lark_issue_link` 表 + 原 thread 回贴 | P1 |
| **P6** | 澄清问答桥：`comment.type=clarification` + thread 优先 + 个人私聊兜底 | P2 + P5 |
| **P7** | 会议创建 + 个人/团队同步通知 | P2 + P5 |

P3 和 P5 可以并行。P2 是个人模式的前置能力。没有个人私聊通道时，不能把已分配事项推到团队群临时代替。

## 10. 显式延期项（写进 RFC，避免被翻出来争论）

| 延期项 | 解锁条件 |
|---|---|
| 自由文本派活（NLU dispatcher） | P5 跑稳之后，且团队真的反馈结构化 verb 不够用 |
| agent 直接调 lark-cli（daemon-side skill） | 出现 multica 中转走不通的真实场景 |
| 飞书文档双向编辑（状态自动回写） | PM 真的在文档表格里追状态并抱怨手动维护 |
| 会议纪要自动转任务 | 团队真在用飞书智能纪要 |
| 以个人身份发消息（区别于 bot 私聊） | bot 身份发消息被证明影响协作语义 |
| 通用 IM adapter 抽象 | 第二家 IM（钉钉 / Slack）真要接入 |

## 11. 不变量（实现里要持续守护）

1. `daemon/` 目录在本方案里**一行不改**。任何要求改 daemon 的需求 → 触发 RFC review。
2. 飞书集成走 **Go 进程内 HTTP**（P1 直接用 `net/http`，后续可升级到 `oapi-sdk-go`），**禁止在生产路径上 spawn lark-cli 子进程**。一次性脚本、运维工具不在此限。
3. agent 端**不接触飞书概念**。所有 IM 桥逻辑只出现在 `service/lark_*.go` 和 `handler/lark.go`。
4. **没有 NLU**。所有飞书 → multica 的写动作都是结构化 verb 或卡片按钮。
5. 飞书 HTTP 调用**永远不能阻塞同步事件总线**。best-effort 通道使用 bounded worker channel，满了丢弃 + WARN；reliable 通道走事务内 outbox + 异步 worker，HTTP 失败只影响 `lark_delivery_log` 重试状态。
6. 已分配给个人的常规事项**默认不进团队群**。任何要求把 assigned / done / mention 全量推群的需求 → 触发 RFC review。
7. 个人私聊是 bot 发送给已绑定用户的 DM；"以用户个人身份发消息"属于延期项。

### 11.1 P1 实现说明

- **Lark 客户端**：P1 用 `net/http` 直接打 `auth/v3/tenant_access_token/internal` 和 `im/v1/messages` 两个稳定接口（约 70 行），不引入 `oapi-sdk-go`。个人私聊同样走 `im/v1/messages`，差异只是 `receive_id_type=open_id`。文档抓取、会议和更复杂的卡片回写需要更广的 API 表面时再切到 SDK；接口形状已经按 `LarkClient` interface 化，切换无侵入。
- **enabled_events 词汇**：团队群 `enabled_events` 和个人 `lark_notification_pref.event_kind` 都复用 `protocol.EventXxx` 常量（`issue:created` 等）。它们只表达粗粒度订阅意愿；通道选择和公共阻塞判定由 `service/lark_notify.go` 硬编码路由函数负责。
- **`bot_token_enc` 字段**：P1 不写入（用 app-level tenant token）。schema、AES-GCM 加解密、`LARK_ENCRYPT_KEY` 全部到位，留给 P2 用户 OAuth + 群级 bot token。

---

这套方案把"PM + 代码 + IM 三方联动"拆成团队协作和个人行动两条通道：团队群保留需求讨论和公共阻塞，个人私聊承接 assigned / mention / failed / snooze。daemon 0 改动、agent 0 改动，IM 翻译仍集中在 server 层。


## 12. multica server、daemon、飞书的关系
```mermaid
flowchart LR
  %% =========================
  %% Multica / Daemon / Feishu
  %% =========================

  subgraph DEV["开发者本机"]
    DAEMON["multica daemon"]
    AGENT["本地 coding agent<br/>Codex / Claude Code / Gemini"]
    CLI["multica CLI"]
    REPO["本地 repo / worktree"]

    DAEMON -->|"启动 / 管理任务"| AGENT
    AGENT -->|"读写代码"| REPO
    AGENT -->|"issue comment / status / result"| CLI
    CLI -->|"HTTP API"| SERVER
    DAEMON -->|"claim task / heartbeat / complete / fail"| SERVER
  end

  subgraph SERVER_BOX["Multica Server: 唯一 IM-aware 层"]
    SERVER["HTTP API / Webhook Handler"]
    BUS["Event Bus"]
    ROUTER["Lark Router<br/>按团队 / 个人路由"]
    BEST["Best-effort Queue<br/>团队群普通通知"]
    OUTBOX["Reliable Outbox<br/>lark_delivery_log"]
    WORKER["Lark Delivery Worker"]
    DB[("Postgres<br/>issues / comments / tasks<br/>lark_* tables")]
    DOCS["Lark Docs Fetcher"]
    THREAD["Thread Bridge<br/>lark_issue_link"]
    ACTION["Action Audit<br/>lark_action_log"]
    IDEMP["Webhook Idempotency<br/>lark_webhook_event_log"]

    SERVER <--> DB
    SERVER --> BUS
    BUS --> ROUTER

    ROUTER -->|"团队普通信号"| BEST
    ROUTER -->|"个人高信号<br/>assigned / failed / clarification"| OUTBOX
    BEST --> WORKER
    OUTBOX --> WORKER

    SERVER --> THREAD
    THREAD <--> DB
    SERVER --> DOCS
    SERVER --> ACTION
    SERVER --> IDEMP
  end

  subgraph LARK["飞书"]
    GROUP["团队群 / Thread<br/>需求讨论 / 公共阻塞 / 未分配认领"]
    DM["个人私聊<br/>个人行动队列 / snooze / agent 问答"]
    CARD["交互卡片<br/>claim / snooze / retry"]
    LARK_API["Feishu OpenAPI<br/>im/v1/messages<br/>docs / calendar"]
    LARK_HOOK["飞书 Webhook<br/>card callback / @bot / reply event"]

    GROUP --> CARD
    DM --> CARD
    CARD --> LARK_HOOK
  end

  %% Server to Feishu
  WORKER -->|"send message<br/>chat_id / open_id"| LARK_API
  DOCS -->|"fetch doc content"| LARK_API
  LARK_API --> GROUP
  LARK_API --> DM

  %% Feishu to Server
  LARK_HOOK -->|"POST /api/webhooks/lark"| SERVER

  %% Team mode
  GROUP -->|"@bot 创建任务"| LARK_HOOK
  SERVER -->|"create issue + lark_issue_link"| DB
  SERVER -->|"回贴原 thread"| LARK_API

  %% Personal mode
  DM -->|"snooze / retry / 原生回复"| LARK_HOOK
  SERVER -->|"create comment / update issue / audit"| DB

  %% Task dispatch path
  SERVER -->|"task context<br/>issue + comments + linked docs"| DAEMON
  DOCS -->|"纯文本文档内容进入任务上下文"| SERVER

  %% Clarification bridge
  AGENT -->|"comment.type=clarification"| SERVER
  SERVER -->|"有 lark_issue_link: 回原 thread"| GROUP
  SERVER -->|"无 thread: 发个人私聊"| DM
  GROUP -->|"原生回复 bot 消息"| LARK_HOOK
  DM -->|"原生回复 bot 消息"| LARK_HOOK
  SERVER -->|"落成 issue comment<br/>v1 不强挂 parent"| DB
  SERVER -->|"触发 agent on_comment"| DAEMON

  %% Boundaries
  DAEMON -. "无飞书 SDK / 无 lark-cli / 无 webhook 处理" .- LARK
  AGENT -. "只理解 issue / comment / task" .- SERVER

  %% Styling
  classDef server fill:#e8f1ff,stroke:#2563eb,stroke-width:1px,color:#111827;
  classDef local fill:#eefdf3,stroke:#16a34a,stroke-width:1px,color:#111827;
  classDef lark fill:#fff7ed,stroke:#f97316,stroke-width:1px,color:#111827;
  classDef db fill:#f3f4f6,stroke:#6b7280,stroke-width:1px,color:#111827;

  class SERVER,BUS,ROUTER,BEST,OUTBOX,WORKER,DOCS,THREAD,ACTION,IDEMP server;
  class DAEMON,AGENT,CLI,REPO local;
  class GROUP,DM,CARD,LARK_API,LARK_HOOK lark;
  class DB db;
```

## 13. 团队协作的 HITL 阶段化

> **HITL 不是新协议，也不是强状态机；它只是把关键交付物表达成结构化 comment，并复用飞书单按钮确认 + 原生 reply 修改意见。**

### 13.1 与澄清桥的边界

澄清桥（6.6）和本节的语义不同，因此独立成节、不合并到 6.6：

| 机制 | 触发方 | 语义 |
|---|---|---|
| 澄清桥 | agent 卡住 | "我缺信息，请人补" |
| HITL 阶段化 | agent 推进到关键节点 | "我没卡住，但需要人确认我可以进入下一步" |

底层共用同一套桥逻辑：bot 消息 + 飞书原生 reply + comment 落库唤醒 agent。差异只在 `comment.type` 取值和按钮 verb。

### 13.2 comment.type 扩展

把 `comment.type` 继续扩成"审核节点"语义。**`*_proposed` 由 agent 产出**；**`plan_approved` 和 `verified` 由 server 代人写入**（人按按钮时落库），不是 agent 自己宣告通过。

| comment.type | 写入方 | 含义 |
|---|---|---|
| `plan_proposed` | agent | 读需求 + 关联文档，提出实现方案 |
| `plan_approved` | server（代人） | 人在飞书或 multica 点 `通过方案` 时落库 |
| `code_proposed` | agent | 开发完成，附 PR 链接 |
| `tests_proposed` | agent | 从需求 + 技术文档生成测试用例草案 |
| `verified` | server（代人） | 人点 `验收通过` 时落库，issue 进入 done |

不为这套语义另起表。`comment.type` 已经是事件流里的一等公民，路由层只多识别几个值。

### 13.3 单按钮 + 原生 reply

每张提案卡只一个按钮，命名按阶段具体化以免用户猜"继续到哪"：

| agent 产出 | 飞书卡片按钮 | 落库效果 |
|---|---|---|
| `plan_proposed` | `通过方案` | server 写 `plan_approved`，agent 进入开发 |
| `code_proposed` | `进入测试` | server 记录 `enter_testing`，`stage` 置为 `testing`，触发 agent 产出 `tests_proposed` |
| `tests_proposed` | `验收通过` | server 写 `verified`，issue 关闭 |

**没有 `reject` 按钮，没有"退回"动作**。人不满意就**原生 reply** 写修改意见，server 落成 issue comment，agent 重新产出同类 `*_proposed`（语义上以最新一条为准，历史仍留在 comment 流）。这把"approve / reject" 的二元 UI 折叠成"approve / 自然回话"，飞书侧只多一个按钮、不多一个交互形态。

### 13.4 stage 是观察值，不是闸门

issue 加一列 `stage`（schema 见 5.2 数据模型，下同；这里只讲语义）。明确语义：

- `stage` 只用于**列表筛选**（"看所有在 planning 阶段的 issue"）和**卡片展示**（飞书提案卡顶部一行 "Stage: planning → developing"），帮人一眼看出 agent 走到哪里。
- **不**做严格状态机校验。允许跳阶段、回退、并行——真实流程经常打破阶段顺序（写到 code 才发现方案有缺、测试阶段又改方案）。
- 推进由按钮 verb 触发：`approve_plan` / `verify` 会写 server comment，`enter_testing` 只记录 action 并推进到测试阶段；但 stage 与 comment 不强一致也无所谓，comment 流是事实来源。

如果未来需要做"必须先 plan_approved 才能 code_proposed"这类硬约束，那时再谈，不在 v1 范围。

### 13.5 与 PR review 的边界

`code_proposed` 的 `进入测试` 按钮**不等于** PR merge。PR 评审仍走 GitHub（已有 `handler/github.go` 集成）：

- `code_proposed` 卡片附 PR 链接，人到 GitHub 上 review 代码。
- 卡片上的 `进入测试` 按钮只表示"我看过了，可以让 agent 写测试用例"。
- 是否真正 merge PR 是 GitHub 侧的事，multica 不抢这个决策。

这条边界写明，避免日后有人提"飞书上一键 merge PR"——那是另一个 RFC。

### 13.6 不在范围

写进 RFC，避免被翻出来：

| 项 | 解锁条件 |
|---|---|
| 多 agent 角色拆分（planning agent / test agent / dev agent） | 同一 agent 跑全流程被证明明显不够用 |
| 自动 approve（高置信度方案跳过人审核） | 团队真要 SLA，且历史上人审都点 `通过方案` |
| timeout 自动通过 | 永不解锁——HITL 的价值就是 gate，"没看 = 没通过" |
| 阶段闸门强校验（stage 状态机） | 真出现 agent 跳阶段产生坏数据的案例 |
| 飞书侧一键 merge PR | 永不解锁，PR review 归 GitHub |

### 13.7 HITL 流程图

```mermaid
flowchart TD
  START([issue 创建]) --> S1

  subgraph S1["阶段 1: 方案审核 (stage=planning)"]
    direction TB
    A1["agent 写 comment(plan_proposed)<br/>方案 + 关联文档摘要"]
    B1["bot 消息<br/>thread 或 DM<br/>单按钮 [通过方案]"]
    D1{人的动作}
    R1["原生 reply<br/>= 修改意见"]
    K1["点 [通过方案]"]

    A1 --> B1 --> D1
    D1 -->|reply| R1
    D1 -->|click| K1
    R1 -->|"server 落 comment<br/>唤醒 agent"| A1
  end

  K1 -->|"server 写 comment(plan_approved)<br/>stage → developing"| S2

  subgraph S2["阶段 2: 开发完成 (stage=developing)"]
    direction TB
    A2["agent 写 comment(code_proposed)<br/>+ PR 链接"]
    B2["bot 消息<br/>thread 或 DM<br/>单按钮 [进入测试]"]
    D2{人的动作}
    R2["原生 reply<br/>= 修改意见<br/>(代码评审在 GitHub)"]
    K2["点 [进入测试]"]

    A2 --> B2 --> D2
    D2 -->|reply| R2
    D2 -->|click| K2
    R2 -->|"server 落 comment<br/>唤醒 agent"| A2
  end

  K2 -->|"记录 enter_testing<br/>stage → testing"| S3

  subgraph S3["阶段 3: 测试用例 (stage=testing)"]
    direction TB
    A3["agent 写 comment(tests_proposed)<br/>测试用例草案"]
    B3["bot 消息<br/>thread 或 DM<br/>单按钮 [验收通过]"]
    D3{人的动作}
    R3["原生 reply<br/>= 修改意见"]
    K3["点 [验收通过]"]

    A3 --> B3 --> D3
    D3 -->|reply| R3
    D3 -->|click| K3
    R3 -->|"server 落 comment<br/>唤醒 agent"| A3
  end

  K3 -->|"server 写 comment(verified)<br/>stage → done"| DONE([issue 关闭])

  classDef agent fill:#e8f1ff,stroke:#2563eb,color:#111827;
  classDef lark fill:#fff7ed,stroke:#f97316,color:#111827;
  classDef gate fill:#fef3c7,stroke:#d97706,color:#111827;
  classDef human fill:#eefdf3,stroke:#16a34a,color:#111827;
  classDef terminal fill:#f3f4f6,stroke:#6b7280,color:#111827;

  class A1,A2,A3 agent;
  class B1,B2,B3 lark;
  class D1,D2,D3 gate;
  class R1,R2,R3,K1,K2,K3 human;
  class START,DONE terminal;
```

三个阶段同构：**agent 产出 → bot 单按钮卡片 → 人 reply 或 click → 推进 or 重做**。reply 走澄清桥的回路（comment 落库唤醒 agent），click 走 card callback（`approve_plan` / `verify` 写 server comment，`enter_testing` 只推进 stage 并触发测试用例草案）。没有 reject 边、没有跳阶段边、没有 timeout 自动推进边——这些都是 13.6 的延期项。

### 13.8 改动面相对当前文档

很薄：

- `comment.type` enum 扩 5 个值（已在 5.2 的 `comment_type_check` 约束里有占位逻辑，直接追加）。
- `issue.stage` 新增 nullable 列 + check 约束。
- `service/lark_notify.go` 的路由表多识别 `plan_proposed` / `code_proposed` / `tests_proposed`，复用 6.6 的 bot 消息 + 原生 reply 机制。
- 卡片模板新增 3 个（plan / code / tests），每张仍是单按钮。
- `lark_action_log.verb` 扩 3 个 verb：`approve_plan` / `enter_testing` / `verify`。

没有新服务、没有新表、没有新桥逻辑。
