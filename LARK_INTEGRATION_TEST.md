I now have a comprehensive understanding of the codebase. Here's the test design.

---

## 飞书集成测试设计思路

### 现有测试覆盖盘点

当前已有 **10 个测试文件、~90 个 test case**，覆盖面集中在单元/模块层：

| 层      | 文件                      | 覆盖内容                                                      |
| ------- | ------------------------- | ------------------------------------------------------------- |
| service | `lark_test.go`            | 加密/解密、卡片 JSON 稳定性、Notify 生命周期                  |
| service | `lark_routing_test.go`    | 路由决策 golden matrix                                        |
| service | `lark_thread_test.go`     | 动词解析、mention strip、thread context                       |
| service | `lark_docs_test.go`       | URL 提取、docx/wiki fetch、错误映射                           |
| service | `lark_media_test.go`      | 消息体解析、MIME、budget、权限码                              |
| handler | `lark_webhook_test.go`    | 签名验证、卡片回调、claim/markdone/createIssue、thread bridge |
| handler | `lark_slash_test.go`      | /help /status /whoami 解析和回复                              |
| handler | `lark_settings_test.go`   | binding CRUD、事件过滤                                        |
| handler | `lark_user_oauth_test.go` | OAuth state HMAC、callback、link/unlink                       |
| handler | `lark_media_test.go`      | 附件下载落库、oversized、权限                                 |
| daemon  | `prompt_test.go`          | LinkedDocs 注入 prompt                                        |

### 缺失的测试方向

两条测试主线对应你提到的两个方向：

---

## 方向一：飞书 ↔ Multica Server 交互（研发流程管理）

这条线测的是 **"飞书群/私聊里的操作能驱动 multica 状态变化，multica 事件能正确分流回飞书"** 的完整闭环。

### 1. 事件监听器集成测试（`lark_listeners_test.go`）

目前 `lark_listeners.go` 里的 `registerLarkListeners` / `registerLarkThreadListeners` **没有任何测试**。这是最关键的缺口——它是事件总线到通知出口的胶水层。

**测试内容：**
- `EventIssueCreated`（无 assignee）→ 调用 `NotifyIssueCreated`，验证 channel=team
- `EventIssueCreated`（有 assignee）→ 验证 DM 路由 + 团队群同时收到
- `EventIssueCreated`（source="lark_thread"）→ 验证 **不** 触发 NotifyIssueCreated（防循环）
- `EventIssueUpdated`（assignee 变化）→ 触发 `NotifyIssueAssigned`
- `EventIssueUpdated`（终态 status）→ 触发 `PatchIssueTerminalCards`
- `EventTaskCompleted` / `EventTaskFailed` → 验证 DM 路由 + 用户偏好控制（opted-out 时静默）
- `EventCommentCreated`（有 @mention、human author）→ `NotifyComment`
- `EventCommentCreated`（agent author、有 issue_link）→ `MirrorAgentCommentToThread`
- `EventCommentCreated`（agent author、无 issue_link）→ 静默

**实现方式：** 构造 fake event bus + mock LarkNotify/LarkThreadService，只验证"正确的方法被调用、参数正确"，不打真实 HTTP。

### 2. 端到端 Webhook 闭环测试（handler 层增强）

现有 handler test 测的是单个 verb，但缺少 **跨步骤场景**：

**测试内容：**
- **Thread→Issue→Agent Comment→Thread Reply 全链路**：POST @bot 创建任务 → 验证 issue + lark_issue_link 创建 → 触发 agent comment event → 验证 thread reply 出现
- **Claim→Assign→Task Complete→Card Patch 全链路**：群认领卡点击 claim → 验证 assignee 设置 → task completed event → 验证原始卡片被 patch 为终态
- **Inbound reply→Comment 往返**：Lark 用户在 thread 回复 → 创建 multica comment → agent 回复 → mirror 回 thread

### 3. 通知偏好路由测试（扩展 `lark_routing_test.go`）

现有 golden matrix 测路由决策，但 **没有把用户偏好 (LarkUserPref) 纳入**：

**测试内容：**
- 用户关闭 `TaskCompletedDM` → `task:completed` 不产生 DM routing
- 用户关闭 `AssignedDM` → 新 assignee 不收到 DM
- 用户未 link 飞书 → 所有 DM 路由静默降级（不报错）
- 默认偏好（Assigned + AgentClarification ON）覆盖新用户场景

### 4. Card Patch / Message Ref 生命周期测试

**测试内容：**
- `recordMessageRef` → `finalizeMessageRef` 状态流转（active → superseded → finalized）
- Issue 到达终态时，所有 active ref 被 patch 为 terminal card
- 重复 patch 幂等（finalized 状态不再 patch）

### 5. WebSocket 模式测试（`lark_ws.go`）

目前 WS 路径 **零测试**。

**测试内容：**
- `handleMessageReceive` 正确调用 `ProcessLarkMessageEvent`
- WS mode 下 v2 event subscription via HTTP 被跳过
- 卡片回调仍走 HTTP webhook（即使 LARK_CALLBACK_MODE=websocket）

### 6. Permission Warning 节流测试

**测试内容：**
- `im:resource` 权限错误 → 写 `last_perm_warning_at`
- 节流窗口内重复错误 → 不重复告警
- 窗口过期后 → 重新告警

---

## 方向二：本地 Agent 使用本地仓库（Daemon 侧）

这条线测的是 **"daemon claim 到 task 后，正确准备 repo、构建 prompt、运行 agent、上报结果"** 的完整链路，以及 Lark 文档注入。

### 1. LinkedDocs Prompt 注入增强测试（扩展 `prompt_test.go`）

现有 3 个 test case 只验证 heading 出现，缺少：

**测试内容：**
- doc.Error="forbidden" → prompt 出现 `[文档不可访问: forbidden]` 占位符
- doc.Error="not_found" → 出现 not_found 占位符
- 混合成功/失败 docs → 成功的展示内容，失败的展示占位
- Content 含特殊字符（markdown、代码块）→ prompt 不被破坏
- 超过 MaxDocsPerClaim(5) 的截断验证

### 2. Claim→Repo→Agent 全链路 Daemon 测试

现有 `daemon_test.go` 覆盖 registration/polling，但缺少 **带 repo 的 claim-to-execution**：

**测试内容：**
- Task 带 `Repos[0].URL` → daemon 调 `repoCache.Sync` → `CreateWorktree` → agent 在 worktree 执行
- Task 带 `ProjectResources` (github_repo) → `taskRepoURLs` 更新 → 不在 refresh 时被清除
- `PriorWorkDir` 非空 → 复用上次 worktree 而非创建新的
- `PriorSessionID` 非空 → 注入到 agent prompt 让 agent resume

### 3. Task Runner 隔离测试（扩展 `runtime_isolation_test.go`）

**测试内容：**
- 并发 task 在不同 slot → 不互相干扰（env root refcount）
- Task 取消（server-side cancel）→ agent 进程被 kill → 上报 cancelled
- Task 超时 → graceful shutdown → 上报 timeout
- Agent 退出码非 0 → 上报 failed + stderr 截取

### 4. Daemon → Server 状态上报测试

**测试内容：**
- `handleTask` 成功 → POST /tasks/{id}/complete，body 含 summary
- `handleTask` 失败 → POST /tasks/{id}/fail，body 含 error
- Network 断开时 → 重试逻辑（或 graceful 降级）
- `AuthToken` 非空 → agent 环境变量 MULTICA_TOKEN = task.AuthToken（不是 daemon token）

### 5. Auto-Update 与 Task 互斥测试（扩展 `auto_update_test.go`）

**测试内容：**
- 有 task 在跑 → `pauseClaims` 设不了 → 升级延后
- 无 task → 正常升级 → `triggerRestart`
- 升级期间新 task 进来 → poller 跳过 claim

### 6. Repo Cache 测试（`repocache/`）

**测试内容：**
- 同 workspace 相同 repo → 复用 bare clone
- 不同 workspace 相同 repo → 独立 bare clone
- `CreateWorktree` → 返回 isolated work dir
- Git clone 失败（网络/auth）→ 返回清晰错误，不阻塞其他 repo

---

## 跨两个方向的集成测试

这是最有价值但目前完全缺失的一层——验证 **"飞书操作 → server 处理 → daemon 执行 → 结果回飞书"** 的全链路。

### 建议方案：Go Integration Test with Fake Lark Server

```
httptest.NewServer (fake Lark API)
      ↓
real multica server (test DB)
      ↓
fake daemon (mock taskRunner)
```

**关键场景：**

| #   | 场景                                                                        | 验证点                                                             |
| --- | --------------------------------------------------------------------------- | ------------------------------------------------------------------ |
| 1   | Lark thread @bot 创建任务 → 自动 claim → 执行 → 完成                        | issue 创建 + lark_issue_link + task dispatch + 完成卡回飞书 thread |
| 2   | 群卡片 claim → agent 执行 → 失败 → retry                                    | claim 改 assignee + failed 通知 DM + retry 按钮重派                |
| 3   | Agent 产生 comment → mirror 到 thread → 用户回复 → 回 multica comment       | 双向 comment bridge 无丢失                                         |
| 4   | Issue body 含飞书文档 URL → claim 时 server 展开 → daemon prompt 含文档内容 | LinkedDocs 端到端可达                                              |
| 5   | 用户关闭所有 DM 偏好 → 全流程执行 → 无 DM 发出                              | 偏好路由实际生效                                                   |

### 实现建议

- 用 `httptest.Server` 做 fake Lark API（`tenantAccessToken` + `im/v1/messages`），记录所有收到的请求
- 跑真实 multica server（in-process，用 test DB）
- Daemon 用 `taskRunnerFunc` 注入 fake agent
- 断言覆盖：DB 状态（issue、link、ref） + fake Lark 收到的请求（card JSON、reply 内容）

---

## 优先级建议

1. **事件监听器测试** — ROI 最高，当前零覆盖，是所有通知路由的枢纽
2. **跨方向集成测试（至少场景 1 和 4）** — 验证 Lark→server→daemon→Lark 闭环
3. **LinkedDocs prompt 增强** — 简单但能覆盖 Lark 文档→agent 这条数据通路
4. **通知偏好路由** — 用户可感知的行为，偏好不生效会产生噪音
5. **其余按需补充**
