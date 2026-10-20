# Agentix 开发指引文档（第二部分）

## 前置：恢复流程的清理与收尾

### 怎么清理

回滚，不是继续。读 `journal` → 恢复配置到发布前 → 移回文件 → 删 `journal`。因为不知道做到哪一步了，回滚到发布前的状态最安全。

### converge 和 finish

- `converge_recovered_agent_plugin_publications`：通知其他正在运行的 daemon 也同步回滚。
- `plugin_recovery.finish()`：释放恢复过程中持有的锁，标记恢复完毕。

---

## 19. 其他核心机制

### Plan（任务计划）

```rust
pub struct PlanData {
    pub title: Option<String>,          // 计划标题
    pub entries: Vec<PlanEntryData>,    // 步骤列表
    pub goal: Option<String>,           // 目标描述
    pub success_criteria: Vec<String>,  // 成功标准
    pub constraints: Vec<String>,       // 约束
    pub verification: Vec<String>,      // 验证方式
}

pub struct PlanEntryData {
    pub step: u32,
    pub description: String,
    pub status: PlanEntryStatus, // Pending / InProgress / Completed
}
```

Plan 存在 `SessionEntry` 的 `active_plan` 字段里。每轮调用 AI 时，系统把当前 Plan 状态作为一条 `name: "runtime_active_plan"` 的 User 消息插入 `context.messages`。

### Checkpoint（状态快照）

存当前 turn 边界 + 可选的文件快照。恢复时回到 checkpoint 记录的状态。

### 自动压缩（Auto Compaction）

- **触发条件：** 消息列表 token 数量超过上下文窗口的 80%。
- **处理方式：** 把旧消息总结成摘要替换。
- **熔断器：** 3 次连续失败 → 60 秒冷却。
- **两种时机：** turn 开始时和工具循环中途。

### Prompt Cache

系统提示词和历史消息会被 AI 后端缓存。多轮对话中系统提示词每次都发，但因为缓存不会重复处理。压缩后 cache 断点失效。

### Fallback Chain

主模型 + 备用模型的重试链。主模型遇到限流或服务器错误时切换到备用模型。

### Doom Loop 检测

检测 AI 是否在重复调用同一工具、同一参数。检测到后给 AI 发提醒消息。

### Steer（用户中途插话）

检测到 steer 后：

关闭当前工具调用 → 注入 steer 消息 → 重置 doom-loop → continue，重新调用 AI。

### with_xxx 模式

配置用线程局部存储传递，避免参数爆炸。

### SessionTurnPermit

Mutex guard，确保每个 session 同一时刻只有一个 writer。

### TurnSupervisor

用 `tokio::select!` 监听多个信号的事件循环，包括超时、runtime 完成、AI 事件、审批请求等。

### 启动前对账（`reconcile_durable_state_before_accept`）

daemon 崩溃后重启时，goal 状态因为存在磁盘上不会丢失。遍历所有 session，找到 Active 状态的 goal：

- 普通模式 → 重新启动（`Reawaken`）。
- plan 模式 → 暂停（`HaltStranded`）。
- 已经不是 Active → 重新投影状态，让前端知道（`ReprojectRestState`）。

turn 的中间过程会丢，但 goal 层面不受影响。

### RPC registry

方法名到处理函数的映射表。

`build_default_registry()` 创建空表，然后注册所有 handler，例如 `chat.send`、`session.create`、`agent.info` 等。收到请求时查表找到对应处理函数。

### 后台任务

daemon 启动后运行的后台周期性任务：

- MCP 清理
- metrics GC
- 系统信息上报
- plan 确认对账
- 子 agent 对账
- cron 调度
- 心跳
- 客户端租约清理
- 资源健康检查

都是“定时醒来 → 检查 → 清理 → 继续睡”的模式。

---

## 20. Config Reload 热重载

**位置：** `crates/daemon/src/handler/config/reload.rs`

### 要做什么

配置文件已经被 `config set` 改好了。`config reload` 要在 daemon 不重启的情况下，用新配置构建一个新的 `AgentRuntime`，替换掉旧的。

核心矛盾：正在进行的 turn 用着旧的 `AgentRuntime`，不能被打断。

### 问题 1：正在进行的 turn 被打断

**会不会发生：** 不会。

turn 开始时从 `turn_admission` 里拿到当前 `AgentRuntime` 的引用。拿到之后这个引用就固定了——turn 从头到尾用这一个引用，不会变。

重载时，`replace_current_generation_mainline` 把 `turn_admission` 里的 `AgentRuntime` 换成新的。但已经在跑的 turn 手里拿的是旧引用，不受影响。

时间线：

```text
时间线：
    ───────────────────────────────────────────→ 时间

Turn A：
    从 turn_admission 取 runtime_v1
    → 用 runtime_v1 跑
    → Turn A 完成

重载：
    turn_admission 里换成了 runtime_v2

Turn B（重载后开始）：
    从 turn_admission 取 runtime_v2
    → 用 runtime_v2 跑
```

旧 `AgentRuntime` 不能立刻扔掉，因为还有 turn 在用。

系统通过 `runtime_owners` 跟踪哪些 turn 还持有旧 runtime 的引用。重载后旧 `AgentRuntime` 被标记为“退休”。每 5 毫秒检查一次（`drain_lease`），等所有持有旧 runtime 的 turn 都结束了，旧 runtime 才被清理。

`drain_lease` 就是“等所有使用者都放手”的过程。lease 就是“谁还在用旧 runtime”的记录。等所有人放手了，旧 runtime 才能扔掉，否则扔了正在用的 turn 就崩了。

### 问题 2：审批丢失

**会不会发生：** 不会。

turn 用的是旧 `AgentRuntime`，里面的旧 `ApprovalGate` 发了审批请求。审批请求存在 `DaemonState` 的 `pending_approvals` 里。

用户点“同意”后，daemon 的 RPC handler 从 `pending_approvals` 找到这个审批，把回复塞进 oneshot 通道。旧 `ApprovalGate` 从通道拿到回复，返回给工具。

旧 `ApprovalGate` 一直活着——因为它在旧 `AgentRuntime` 里，而旧 `AgentRuntime` 还有 turn 在用，不会被清理。

`pending_approvals` 存在 `DaemonState` 里而不是 `AgentRuntime` 里，原因是：

- RPC handler 是 daemon 层的代码，不在 `AgentRuntime` 里。
- 用户回复审批时，daemon 的 RPC handler 要直接访问 `pending_approvals`。
- 存在 `DaemonState` 里，RPC handler 直接访问，不需要先拿到 `AgentRuntime`。

### 问题 3：MCP 连接断

**会不会发生：** 不会。

重载时先做指纹比对——算新配置的 MCP 部分的哈希值，和旧的比：

- **相同** → 复用旧 MCP 连接，不重建。正在进行的工具调用完全不受影响。
- **不同** → 先构建新 MCP router，构建成功后再替换。正在跑的 turn 用着旧 MCP Router，不受影响。等所有用旧 MCP Router 的 turn 结束后，旧 MCP 连接才通过 `drain_lease` 清理。

构建失败不替换，继续用旧的。

### 问题 4：并发重载互相覆盖

**会不会发生：** 会，如果不防的话。

两个 `config set` 命令几乎同时到达 daemon，都触发重载。如果都走完“读配置 → 构建新 runtime → 替换”，可能互相覆盖，或者两个构建过程混了配置。

**怎么防的：三道锁，每道锁管不同阶段。**

1. `mcp_mutation_lane`：防重载和 MCP 变更操作同时进行。
2. `config_reload_transaction_lock`：防两个重载同时进入构建阶段。
3. `prepare_live_reload` 内部锁：防发布阶段撞车。

B 在等待的过程中，A 已经改完配置了。B 进入后读到的是 A 改过的最新配置，不会覆盖 A 的结果。

### 问题 5：配置文件写一半损坏了

**会不会发生：** 会，如果不防的话。

**第一层防护：原子写入。**

写配置文件时先写临时文件 `config.json.tmp`，写完后再 rename 成 `config.json`。操作系统保证 rename 是原子的——要么整个成功，要么没发生。

所以目标文件要么是完整的旧配置，要么是完整的新配置，不会是半成品。

**第二层防护：失败回滚。**

重载前先备份旧配置（`ConfigRollback`）。如果构建新 runtime 失败，例如新配置里写了一个不存在的 AI 后端地址，恢复旧配置：

```text
构建失败
    → restore_previous_raw
    → 回滚配置文件
    → daemon 继续用旧配置运行
    → 报错
```

daemon 不会因为配置错误而挂掉。

### 问题 6：客户端连接被断

**会不会发生：** 会，如果不防的话。

`ClientLeaseReloadGuard` 在重载开始时冻结客户端租约操作。冻结期间不检查、不清理、不拒绝客户端。重载完成后自动解冻。

冻结的意义不是“保护客户端不断”，而是防止重载过程中状态不一致导致误判。

重载过程中 `DaemonState` 的配置字段在陆续更新。如果这时候检查客户端，可能拿到一半新、一半旧的配置，判断结果不可靠。冻结后等所有配置字段都更新完了，再检查，结果才正确。

### 问题 7：API key 换了影响进行中的 turn

**会不会发生：** 不会。

正在进行的 turn 用旧 `AgentRuntime` 里的旧 provider，旧 provider 的 key 不受重载影响。

重载完成后做 reconcile，更新的是新 provider 的 key，只影响新 turn 的请求。已发出的 HTTP 请求不受影响。

### 整体流程

```text
config set / config reload
    ↓
写配置文件（原子写入：先写临时文件再 rename）
    ↓
进入重载流程
    ├── 获取第一道锁（mcp_mutation_lane，防 MCP 变更和重载同时进行）
    ├── 获取第二道锁（config_reload_transaction，同一时间只有一个重载进入构建）
    ├── 冻结客户端租约操作
    └── 获取第三道锁（prepare_live_reload，串行化发布）
    ↓
读取配置
    ↓
检查 MCP 指纹
    ├── 没变 → 复用旧 MCP 连接
    └── 变了 → 构建新 MCP router（失败则不替换）
    ↓
构建新 AgentRuntime（providers、tools、sandbox、context_engines 等）
    ↓
构建失败？
    ├── 是 → 回滚配置文件 → daemon 继续用旧配置 → 报错
    └── 否 → 继续
    ↓
替换 turn_admission 里的 AgentRuntime（旧 turn 用旧的，新 turn 用新的）
    ↓
更新 DaemonState 里跟配置相关的字段（config、sandbox、lsp_manager、capabilities 等）
    ↓
更新 MCP router（复用或替换）
    ↓
更新凭证管理器（reconcile API key）
    ↓
发布配置变更通知
    ↓
解冻客户端租约
    ↓
释放三道锁
    ↓
完成
    ↓
后台：等所有持有旧 runtime 的 turn 结束后，清理旧 runtime 和旧 MCP 连接（drain_lease）
```

### 总结表

| 问题 | 会不会发生 | 怎么防的 |
|---|---|---|
| 正在进行的 turn 被打断 | 不会 | turn 一开始固定了 `AgentRuntime` 引用，重载换的是 `turn_admission` 里的，不是 turn 手里的 |
| 审批丢失 | 不会 | 旧 `ApprovalGate` 随旧 `AgentRuntime` 一起活着。`pending_approvals` 存 `DaemonState` 是为了 RPC handler 直接访问 |
| MCP 连接断 | 不会 | 没变就复用，变了等旧 turn 完成才清理旧连接 |
| 并发重载互相覆盖 | 会，如果不防 | 三道锁串行化 |
| 配置写一半损坏 | 会，如果不防 | 原子写入 + 失败回滚到旧配置 |
| 客户端被断连 | 会，如果不防 | 冻结租约，等所有配置更新完再检查 |
| API key 换了影响进行中的 turn | 不会 | reconcile 只改新 provider 的 key，已发出的请求不受影响 |

---

## 21. DaemonState 与 AgentRuntime 的职责划分

### DaemonState（进程级）

管的是“daemon 这个进程在跑什么、配置是什么、谁连着它”。

持有：

- **进程信息：** `agent_id`、`state_dir`、`config`、`agent_config`、`cancel`、`start_time`
- **AgentRuntime：** 通过 `runtime` 字段，以 `MessageHandler` trait 形式存
- **会话存储：** `storage`
- **交互门控：** `pending_approvals`、`approval_notify_tx`、`plan_gate`、`question_gate`
- **请求管理：** `active_requests`、`run_coordinator`、`turn_admission`
- **RPC：** `rpc_registry`
- **Provider：** `provider_metrics`、`provider_capacity`
- **cron：** `cron_store`、`cron_executor`、`cron_event_queue`
- **资源管理：** `resource_manager`、`background_exec_manager`、`memory_manager`
- **代码指标：** `ai_code_metrics_policy`、`mutation_writer`、`repo_enrollment`
- **日志和诊断：** `plane_logs`、`diagnostics_plane`
- **子 agent：** `subagent_manager`
- **MCP：** `mcp_router`、`current_mcp_summary`
- **凭证：** `router_credential_manager`
- 各种缓存和锁

### AgentRuntime（运行时级）

管的是“AI 怎么调、工具怎么执行、上下文怎么组装”。

持有：

- `providers`：AI 后端连接，调 AI 时用。
- `tools`：工具列表，AI 请求工具时用。
- `sandbox`：沙箱，执行 `exec` 工具时用。
- `storage`：会话存储，读写 session 数据，和 `DaemonState` 共用。
- `context_engines`：上下文引擎，组装消息列表、压缩。
- `memory_manager`：记忆系统，召回和存储记忆。
- `subagent_manager`：子 agent 管理器。
- `approval_gate`：审批门控，工具需要审批时用。
- `plan_gate`：计划确认门控。
- `resource_manager`：资源管理器。
- `config`：配置。

### 关系

`DaemonState` 里面包含 `AgentRuntime`。`DaemonState` 是更大的容器。

一个 daemon 进程只有一个 `agent_id`，只有一个 `AgentRuntime`。

如果有两个 agent（coder 和 reviewer），就有两个 daemon 进程，各自独立，各有一个 `AgentRuntime`。

config reload 时换的是同一个 daemon 进程里的 `AgentRuntime`——旧的换成新的，但始终只有一个当前的 `AgentRuntime`。

### 为什么分两层

有些东西只有 daemon 层需要管，`AgentRuntime` 不需要知道：

- **RPC 路由：** daemon 收请求时用，AgentRuntime 不管请求怎么来的。
- **请求排队：** daemon 决定能不能执行，AgentRuntime 只管执行。
- **cron 定时任务：** daemon 到点了触发，AgentRuntime 不管定时。
- **代码指标统计：** daemon 记录，AgentRuntime 不管统计。
- **配置热加载：** daemon 管配置更新，AgentRuntime 只管用配置。

AgentRuntime 只专注一件事：拿到消息后，组装上下文、调 AI、执行工具、返回回复。

### Config Reload 时 DaemonState 哪些要换

**换的（和配置直接相关的字段）：**

- `current_config`、`current_runtime`、`current_capabilities`、`current_sandbox`
- `lsp_manager`、`registered_ce_ids`、`registered_me_ids`
- MCP router、MCP summary

**不换的（和配置无关的进程级固定状态）：**

- `agent_id`、`state_dir`、`cancel`、`start_time`
- `rpc_registry`、`pending_approvals`
- `plan_gate`、`question_gate`
- `cron_store`
- 各种锁

---

## 22. MCP 机制详解

**位置：** `crates/agent/src/mcp.rs`、`crates/agent/src/mcp/router.rs`

### 三个角色

- **MCP 服务器：** 外部进程，提供工具。比如一个数据库查询服务跑在 `localhost:3000`。
- **MCP Router：** daemon 内部的路由器。AI 调 MCP 工具时，MCP Router 找到对应的 MCP 服务器，转发请求，拿结果返回。
- **MCP 连接：** daemon 和 MCP 服务器之间的长连接。连上后一直保持，多次工具调用复用同一个连接。

### 代码结构

```rust
pub struct McpRouter {
    clients: HashMap<String, Arc<McpClient>>, // server_id → 客户端连接
}

pub struct McpClient {
    id: String,                                      // 服务器名字
    transport: McpTransport,                         // 传输方式
    process: Mutex<Option<StdioProcess>>,            // 子进程句柄（stdio 方式）
    sse_url: Mutex<Option<String>>,                  // SSE 连接地址
    streamable_http: Mutex<Option<StreamableHttpClient>>, // HTTP 流式客户端
    connection_state: RwLock<McpConnectionState>,    // 连接状态
    tools_cache: RwLock<Vec<ToolDefinition>>,        // 工具列表缓存
}
```

### 三种传输方式

```rust
pub enum McpTransport {
    Stdio { command, args, env, ... }, // 启动子进程，通过标准输入输出通信
    Sse { url, headers },              // HTTP SSE 长连接
    StreamableHttp { url, headers },   // HTTP 流式
}
```

**stdio 方式：** daemon 启动一个子进程（如 `node db-server.js`），通过子进程的标准输入发请求、标准输出收回复。子进程一直跑着，不退出。

### 为什么是长连接

AI 在一个 turn 里可能连续调好几次同一个 MCP 工具集里的工具。

长连接连一次，多次调用复用同一个连接，省掉重复建连的开销。有些 MCP 服务器启动慢（要加载模型、连接数据库），每次重连等很久。

### AI 调 MCP 工具的路径

```rust
// AI 请求调用 "mcp:database:query_table" 工具
pub async fn call(
    &self,
    namespaced_name: &str,
    args: Value,
) -> AgentixResult<Value> {
    let (_server_id, client, tool_name) =
        self.resolve_namespaced_tool(namespaced_name).await?;

    // 从 "mcp:database:query_table" 解析出 server_id="database"
    // tool_name="query_table"
    client.call_tool(tool_name, args).await

    // 通过 McpClient 的连接发请求，等回复
}
```

工具名格式：`mcp:{server_id}:{tool_name}`，例如 `mcp:database:query_table`。

### 新旧 MCP Router 不会冲突

两个 MCP Router 是两个独立的 `HashMap<String, Arc<McpClient>>`，各存各的。

Turn A 用旧 MCP Router 的旧 `McpClient`（旧连接，子进程 PID=100），Turn B 用新 MCP Router 的新 `McpClient`（新连接，新子进程 PID=200）。

两个连接各自独立，不会串。

旧 MCP Router 等所有用它的 turn 完成后才清理（`drain_lease`），清理时杀掉旧子进程。

---

## 23. Sub-agent 完整链路

**位置：** `crates/agent/src/tools/sub_agent.rs`、`crates/agent/src/subagent.rs`

### 什么是 sub-agent

主 agent 在执行任务时，如果任务太复杂或需要并行处理，可以派生一个子 agent 去做子任务。子 agent 有自己的 session，独立执行，执行完把结果返回给主 agent。

### SubAgentManager

管理所有子 agent 运行状态的中心。记录哪些子 agent 在跑、哪些完成了、结果是什么。存在 `DaemonState` 和 `AgentRuntime` 里。

```rust
pub struct SubAgentManager {
    active_runs: Mutex<HashMap<String, ActiveRun>>,          // 正在跑的子 agent
    recent_runs: Mutex<VecDeque<RecentRunInfo>>,             // 最近完成的子 agent
    terminal_payloads: Mutex<HashMap<String, Value>>,        // 子 agent 的执行结果
    semaphore: Semaphore,                                    // 并发限制
    steer_senders: Mutex<HashMap<String, Sender<SteerMessage>>>, // 父子间的插话通道
}
```

### 派生流程

1. AI 调用 `spawn_subagent` 工具，传入任务描述、超时时间等。
2. 准入检查（`SubAgentManager.try_admit_run`）：并发数、递归深度、单写锁。
3. 创建子 session（继承父 session 的配置——model、workspace、tools）。
4. 写入委派合约（`delegation_token + delegation_budget` 存到子 session 的 `SessionEntry` 里）。
5. 后台启动执行任务：

   `spawn_delegation_task → execute_delegated_message → handle_message_for_agent_with_options`

6. `spawn_subagent` 立即返回 `{"status": "running", "run_id": "xxx"}` 给 AI。

### 子 agent 独立执行

子 agent 调的是同一个 `AgentRuntime` 的 `handle_message_for_agent_with_options`，和主 agent 走完全一样的 `run_agent_pipeline` 五阶段流程。

区别：

- 用子 session（独立的消息历史、plan、goal）。
- 受委派预算限制（token、工具调用次数、时间）。
- 受委派令牌验证（只允许用指定的工具）。
- 事件通过 `monitor_delegation_events` 转发给主 agent 的 `TurnSupervisor`。
- 审批走同一个 `ApprovalGate`，审批归属标记为父 session。
- 父 agent 可以通过 `send_message` 给子 agent 插话（steer）。

### 结果回传

`spawn_result_finalizer` 在后台等子 agent 完成。

子 agent 完成后：

- 结果存到 `SubAgentManager.terminal_payloads["run_id"]`。
- run 从 `active_runs` 移到 `recent_runs`。
- 委派树节点更新为“已完成”。

AI 后续调用 `wait_subagents("run_id")` 从 `terminal_payloads` 取结果。如果子 agent 还在跑，`wait_subagents` 阻塞等待。

### spawn_subagent 参数

```json
{
  "task": "帮我分析 a.txt 的依赖关系",
  "model_preference": "deep",
  "name": "analyzer",
  "timeout_secs": 300,
  "subagent_type": "explore"
}
```

核心参数是 `task`（做什么）和 `timeout_secs`（多久超时），其他可选。

### 委派合约

给子 session 盖个章，记录“你是谁派来的、能做什么、能用多少资源”。存在子 session 的磁盘记录里。

```rust
pub struct DelegationToken {
    pub parent_session_key: String,            // 父 session 是谁
    pub allowed_tools: Vec<String>,            // 允许用哪些工具
    pub allowed_risk_classes: Vec<String>,     // 允许做什么级别的操作
    pub expires_at_unix_secs: Option<u64>,     // 什么时候过期
}

pub struct DelegationBudget {
    pub token_budget: Option<u64>,             // 最多用多少 token
    pub tool_call_budget: Option<u32>,          // 最多调用几次工具
    pub wall_clock_secs: u64,                   // 最多跑多久
    pub subagent_spawn_budget: Option<u32>,    // 最多能再派生几个 agent
}
```

### 子 agent 不需要 TurnSupervisor

`TurnSupervisor` 是“和前端沟通的窗口”。

子 agent 没有自己的窗口，它的事件经过主 agent 的 `TurnSupervisor` 转发出去。子 agent 不直接和前端交互。

### chat 链路和 spawn_subagent 链路在 handle_message 处汇合

两条链路的前序准备不同，但到 `handle_message_for_agent_with_options` 后完全一样，都走 `run_agent_pipeline` 五阶段。

| 对比项 | chat | spawn_subagent |
|---|---|---|
| 谁触发 | 用户在 CLI/前端发消息 | AI 在工具循环中调 `spawn_subagent` 工具 |
| session | 用户指定的 | 新创建的子 session |
| TurnSupervisor | 有 | 没有 |

---

## 24. Memory Engine 详解

**位置：** `crates/agent/src/memory/mod.rs`、`crates/agent/src/memory/recall.rs`

### 记忆是什么

AI 的长期记忆系统。AI 在执行任务时把经验存起来，下次遇到类似任务时拿出来用。

### 引擎是什么

一个插件，只负责基础的“存、搜、删”。类似一个向量数据库 + CRUD。

完全插件化，内置了 relic 引擎（SQLite + 向量搜索），也可以用外部 `.so` 文件加载。

引擎不管业务逻辑——排序、过滤、选哪条注入都是 AgentRuntime runtime 层做的。

### MemoryEngine trait

```rust
pub trait MemoryEngine: Send + Sync {
    async fn store(
        &self,
        layer_id: &str,
        key: &str,
        entry: MemoryEntry,
    ) -> AgentixResult<()>;

    async fn recall(
        &self,
        query: &str,
        limit: usize,
    ) -> AgentixResult<Vec<MemoryEntry>>;

    async fn forget(
        &self,
        layer_id: &str,
        key: &str,
    ) -> AgentixResult<bool>;

    async fn clear_session(
        &self,
        session_id: &str,
    ) -> AgentixResult<usize>;

    async fn ingest_message(
        &self,
        session_id: &str,
        role: &str,
        content: &str,
    ) -> AgentixResult<()>;

    // ...
}
```

### 记忆怎么存

`run_agent_pipeline` 阶段 5 结束后，后台 `post_completion_memory_flush` 把这一轮学到的东西存进去。

编程偏好不能静默自动存。需要 AI 判断值得记住 → 用 `question` 工具问用户确认 → 用户同意后才存，标记 `preference_confirmation=confirmed`。

### 记忆怎么召回

三条独立路径：

#### 路径 1：环境注入（每次用户发消息自动触发）

- 把用户消息当查询搜记忆引擎。
- 去重 → 过期过滤 → 策略过滤（只有高可信度记忆才注入）→ 排序 → 选分数最高的一条。
- 有并列就放弃，宁可不注入也不注入错误的。
- 最多 1 条，静默插入系统提示词。

#### 路径 2：编程偏好注入（每次用户发消息自动触发）

- 查所有用户确认过的编程偏好。
- 全部注入，可能多条。

#### 路径 3：显式查询（AI 主动调 `memory_search` 工具触发）

- AI 知道有记忆系统，主动搜索历史。
- 可以返回多条。
- 在工具循环里触发。

### 召回排序（路径 1）

纯规则，不调 LLM。排序优先级：

1. 没过期的 > 过期的。
2. 用户手动存的 > 自动存的。
3. 来源可靠性高的 > 低的。
4. 时间匹配的 > 不匹配的。
5. 置信度高的 > 低的。
6. 更新的 > 更旧的。

分数是记忆引擎返回的（关键词匹配或向量相似度），不是 LLM 判断的。

### 记忆分三层

| 层 | 用途 | 跨会话 |
|---|---|---|
| `semantic` | 长期记忆（偏好、事实） | global 跨，session 不跨 |
| `working` | 临时记忆（这个 turn 的临时信息） | 不跨 |
| `episode` | 事件记忆（之前做过什么） | 不跨 |

### 编程偏好的状态流转

```text
候选
    ↓
AI 判断值得记住
    ↓
用 question 工具问用户
    ├── 用户确认 → Confirmed → 存入，召回时注入
    └── 用户拒绝 → 不存，不注入
```

之前是“关键词命中就静默存为永久偏好”，假阳性高。改成“AI 判断 + 用户确认”后，只有真正值得长期记住的偏好才存。

### 关键文件

| 文件 | 内容 |
|---|---|
| `crates/agent/src/memory/mod.rs` | MemoryEngine trait、MemoryEntry、可见性策略 |
| `crates/agent/src/memory/recall.rs` | 召回逻辑（查询、去重、过滤、排序、选胜者） |
| `crates/agent/src/memory/bundled.rs` | 内置引擎适配器 |
| `crates/agent/src/memory/deferred.rs` | 延迟加载引擎 |
| `crates/agent/src/runtime/context_memory_recall.rs` | 召回入口 |
| `plugins/memory-engines/relic/` | relic 引擎实现 |

---

## 25. Agent Stop 流程

**位置：** `crates/cli/src/commands/agent/lifecycle.rs`、`crates/daemon/src/admin_lifecycle_control.rs`

### 和 start 对称

| 对比项 | start | stop |
|---|---|---|
| 租约 | 获取文件锁 | 获取文件锁 |
| 状态检查 | 必须是 Stopped | 必须是 Ready |
| 进程操作 | fork 子进程 | SIGTERM → SIGKILL |
| 等待 | 轮询 socket 可连接 | 轮询进程退出 |
| finalize | 更新为 Running | 更新为 Stopped |

### stop 流程

```text
agent stop coder
    ↓
获取生命周期租约（文件锁）
    ↓
检查 daemon 状态
    ├── Stopped 且进程不存在 → 报错 "not running"
    └── Ready → 正常
    ↓
验证进程身份（PID 没被复用）
    ↓
准备停止（更新 record 为 Stopping）
    ↓
发 SIGTERM（优雅停止）
    ├── daemon 收到 SIGTERM → cancel.cancel()
    └── 所有持有 cancel 的组件收到信号
        ├── listener 停止 accept
        ├── 后台任务停止
        ├── 正在跑的 turn 停止
        └── 审批通道关闭
    ↓
清理退出（停 listener、停后台任务、写日志）
    ↓
轮询等进程退出
    ├── 退出了 → 继续
    └── 没退出 → 发 SIGKILL（强制杀死）→ 轮询等退出
    ↓
finalize（更新 record 为 Stopped，清理临时文件）
    ↓
释放租约
```

### CancellationToken

信号机制。`cancel()` 后，所有 `clone()` 出来的副本都能感知到。

检查方式：`is_cancelled()` 或在 `select!` 里使用 `token.cancelled()`。

取消令牌只是发信号，不会自动杀掉任何东西——每个组件自己决定“收到取消信号后做什么”。大部分组件就是“不干了，退出”。

---

## 26. Provider 内部机制

**位置：** `crates/provider/src/lib.rs`、`crates/provider/src/factory.rs`

### Provider 是什么

翻译官。AgentRuntime 说“给我调 AI”，Provider 把请求翻译成对应后端的格式发出去，把返回翻译成统一格式传回来。

### 7 种 api_type

| api_type | 后端 |
|---|---|
| `openai-completions` | OpenAI Chat Completions |
| `openai-responses` | OpenAI Responses API |
| `anthropic-messages` | Anthropic Claude |
| `ollama` | 本地 Ollama |
| `dashscope` | 阿里云通义千问 |
| `google-generative-ai` | Google Gemini |
| `minimax` | MiniMax |

### Provider trait 核心

```rust
pub trait Provider: Send + Sync {
    fn id(&self) -> &str;
    fn api_type(&self) -> ApiType;

    async fn stream_with_options(
        &self,
        model: &str,
        context: &StreamContext,
        options: ProviderRequestOptions,
    ) -> AgentixResult<StreamResult>;
}
```

Provider 内部三步：

1. 把 context 转成后端格式。
2. 发 HTTP 请求。
3. 读返回流，翻译成 `StreamEvent`。

### StreamEvent 统一格式

| 事件 | 意思 |
|---|---|
| `TextDelta` | AI 输出了一段文字 |
| `ThinkingDelta` | AI 输出了一段思考 |
| `ToolCallCompleted` | AI 请求调一个工具 |
| `Usage` | token 用量 |
| `Done` | AI 回复完了 |
| `Error` | 出错了 |

不管调的是 OpenAI 还是 Anthropic，AgentRuntime 拿到的都是这些统一格式。

### 工厂模式

根据配置里的 `api_type` 创建对应 Provider。用宏批量生成：

```rust
provider_factory!(
    OpenAiFactory,
    ApiType::OpenAiCompletions,
    OpenAiProvider
);

provider_factory!(
    AnthropicFactory,
    ApiType::AnthropicMessages,
    AnthropicProvider
);

// ... 其他 5 个
```

### 怎么存储和取用

Provider 存在 AgentRuntime 的 `HashMap` 里，key 是配置里的 provider 名字。

调用时从 session 的 model 字段找到对应的 provider，从 HashMap 取出来调 `stream_with_options`。

---

## 27. Stream 流机制

### Stream 是什么

一个“还没到齐的数据序列”。和数组不同（全在内存），stream 是数据一条一条来的。

### 怎么用

```rust
let stream = provider
    .stream_with_options(model, context, options)
    .await?;

while let Some(event) = stream.next().await {
    match event {
        Ok(StreamEvent::TextDelta { text }) => {
            emit_text(text);
        }
        Ok(StreamEvent::Done) => {
            break;
        }
        _ => {}
    }
}
```

`stream.next().await` 每次等下一条事件，来一条处理一条，直到 `Done` 退出。

### 内部实现

Provider 发 HTTP 请求后，AI 后端用 SSE 一条一条推回来。Provider 把 HTTP 字节流翻译成 `StreamEvent` 流。

`map` 把一个字节流变成事件流。不立刻执行，只是记录“每来一条字节就调用翻译函数”。

外面 `next().await` 读时才执行：

读一条字节 → 翻译 → 返回。

### await 和 stream 交互

`.stream.next().await`：没数据时让出线程去干别的，HTTP 又来数据了再恢复处理。这样同一个 daemon 可以同时处理多个用户请求。

### StreamResult 类型

```rust
pub type StreamResult =
    Box<dyn Stream<Item = Result<StreamEvent, AgentixError>> + Send + Unpin>;
```

返回的不是单条事件，而是一个绑着 HTTP 连接、可以反复读的流对象。

`Box::pin` 把它固定在内存里。

---

## 28. Cron 定时任务

**位置：** `crates/cron/src/`、`crates/daemon/src/cron_executor.rs`

### 数据结构

```rust
pub struct CronJob {
    pub job_id: String,
    pub name: String,
    pub schedule: CronSchedule,
    pub session_target: SessionTarget,
    pub enabled: bool,
    pub run_count: u32,
    pub max_runs: Option<u32>,
    pub next_run_at: Option<DateTime<Utc>>,
}
```

### 三种调度方式

```rust
pub enum CronSchedule {
    At { at: DateTime<Utc> }, // 一次性
    Every { every_ms: u64 },  // 间隔
    Cron { expr: String },    // Cron 表达式
}
```

### 三种执行目标

```rust
pub enum SessionTarget {
    Main,     // 主 session
    Isolated, // 临时独立 session（默认）
    Named,    // 指定 session
}
```

### 调度器

CronScheduler 后台任务，每秒检查一次：

```rust
loop {
    select! {
        _ = cancel.cancelled() => return,
        _ = sleep(1s) => self.tick().await,
    }
}
```

`tick` 做的事：

加载所有任务 → 遍历 → 到点了（`next_run_at <= now`）→ 加入待执行列表 → 准入检查 → 执行。

### 执行

CronScheduler 自己不执行，通过 `CronExecutor` trait 交给 daemon。

daemon 创建 session，发消息给 AgentRuntime，走 `run_agent_pipeline`（和 chat 一样），拿到 AI 回复后按配置投递结果。

### 并发控制

同时最多跑 `max_concurrent_runs` 个任务。同一任务不能同时跑两次。

---

## 29. 写入流程（config set / agent create / session create）

### config set

示例：

```text
config set model gpt-4o
    → 参数校验
    → 获取配置写入锁
    → 读取配置文件 → 解析 JSON → 改值 → 验证 → 原子写入
    → 备份旧配置（ConfigRollback）
    → 释放写入锁
    → 触发 config reload（构建新 AgentRuntime，替换旧的）
    → 通知其他 daemon 重载
    → 记录审计日志
```

### agent create

示例：

```text
agent create coder --model gpt-4o --workspace /projects/my-app
    → 获取 agent mutation guard（防并发）
    → 检查冲突（未完成删除？已存在？）
    → 组装 AgentConfig
    → 写 ~/.agentix/agents/coder/agent.json
    → 创建 skills 目录
    → 创建 workspace 目录
```

### session create

示例：

```text
sessions create --agent coder --session main
    → 解析参数（session 名字、cwd、MCP 服务器）
    → 检查是否克隆
    → 获取创建守卫
    → 从配置复制默认值（model、approval_behavior 等）
    → storage.upsert_session（写 SessionEntry 到磁盘）
```

### 对比

| 对比项 | config set | agent create | session create |
|---|---|---|---|
| 表格后续内容 | 图片未完整显示 | 图片未完整显示 | 图片未完整显示 |

