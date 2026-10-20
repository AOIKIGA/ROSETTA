# Agentix 架构学习指南

本文档记录了从零开始学习 Agentix 项目的完整过程，涵盖从 CLI 入口到 AI 回复的完整调用链、核心模块机制，以及关键代码位置。

## 目录

1. 项目概览
2. 二进制与 Crate 结构
3. 核心概念
4. 目录与配置体系
5. init 流程
6. start 流程
7. daemon 内部初始化
8. session 创建流程
9. chat 请求完整流程
10. run_agent_pipeline 五阶段流水线
11. 工具循环（Tool Loop）
12. Provider 调用链
13. 审批 Gate 机制
14. Gateway 鉴权
15. Sandbox 沙箱
16. IM 通道（钉钉等）
17. 生命周期管理
18. 插件恢复机制
19. 其他核心机制
20. Config Reload 热重载
21. DaemonState 与 AgentRuntime 的职责划分
22. MCP 机制详解
23. Sub-agent 完整链路
24. Memory Engine 详解
25. Agent Stop 流程
26. Provider 内部机制
27. Stream 流机制
28. Cron 定时任务
29. 写入流程（config set / agent create / session create）
30. Rust 宏基础
31. 完整调用链速查
32. 关键文件索引

---

## 1. 项目概览

Agentix 是企业级智能体核心底座引擎，以 daemon 模式运行，可通过前端、网关、消息通道扩展出多样化的企业智能体。

### 核心特性

- **可运维：**「一 Agent — Daemon」架构，支持独立启停、状态查询、配置热加载。
- **可观测：**四维度实时指标，包括进程资源、工作负载、Token 用量、Provider 调用。
- **资源高效：**存算分离架构，异步非阻塞运行时。
- **多重安全：**path/shell/approval 底线、JSONL 审计与 exec sandbox。

---

## 2. 二进制与 Crate 结构

### 2.1 五个二进制

| 二进制 | 作用 |
|---|---|
| `dtclaw` | 开发版入口 |
| `dtcoder` | 产品 launcher，找到并 exec 真正的产品二进制 |
| `dtcodercli-internal` | 8 行代码，调用 `product_main::run()` |
| `dtcodercli` | CLI 入口 |
| `dtcodercn` | 国内版入口 |

### 2.2 启动链

```text
dtcoder（launcher）
    ↓
找到 dtcodercli-internal
    ↓
exec dtcodercli-internal
    ↓
product_main::run()
    ↓
runtime::run()（建 Tokio runtime）
    ↓
cli::run()（解析命令行参数）
```

### 2.3 Crate 依赖层次

```text
cli / acp / gateway    ← 三入口
          ↑
daemon                 ← daemon 进程逻辑
          ↑
agent                  ← AgentRuntime，核心运行时
          ↑
security / config / storage / provider / skill
          ↑
common                 ← 最底层，所有人都依赖它
```

### 2.4 三入口

| 入口 | Crate | 协议 | 用途 |
|---|---|---|---|
| CLI | `crates/cli` | 命令行 | 本地开发、运维操作 |
| ACP | `crates/acp` | stdio JSON-RPC | IDE 集成 |
| Gateway | `crates/gateway` | WebSocket/HTTP | 服务化部署 |

三入口都通过 Unix Socket（JSON-RPC）连接 daemon，走同一条处理路径。

---

## 3. 核心概念

### 3.1 turn vs round

- **turn：**用户发一条消息到 AI 彻底回完，算一个 turn。用户行为驱动。
- **round：**工具循环里调一次 AI，算一个 round。工具循环驱动。
- 一个 turn 可能包含多个 round（AI 中间要调工具）。

### 3.2 turn vs goal

- **turn：**用户说一句“读一下 `a.txt`”，AI 回复，结束。一个 turn。
- **goal：**用户说“帮我重构这个模块”，这是一个 goal。它要很多个 turn 才能完成——读代码、分析、写新代码、跑测试。每个 turn 推进 goal 一步。
- goal 存在 session 里，跟着 session 走。daemon 崩了也不会丢（存在磁盘上）。

### 3.3 AgentRuntime

“大盒子”，包含 provider、tools、sandbox、storage、memory engine、context engines 等所有组件。

### 3.4 DaemonState

daemon 运行期间的核心状态，包含 AgentRuntime、storage、配置、RPC registry、审批系统、cron 等所有东西。后面所有处理请求的代码都从这里拿组件。

### 3.5 StreamContext（daemon 侧）

打包了 `session_key`、`run_id`、AgentRuntime、写 permit、abort token、event subscriber、projector。

### 3.6 StreamContext（provider 侧）

打包了 `system_prompt`、`messages`、`tools`、`max_tokens`，用于调 AI 后端。

### 3.7 SessionEntry vs RuntimeSessionSnapshot

- `SessionEntry`：完整的磁盘记录（`session_id`、`cwd`、`model`、`history_file`、`approval_behavior`、`tokens`、`active_plan` 等）。
- `RuntimeSessionSnapshot`：内存中的简化版本。

### 3.8 CancellationToken

树形结构的取消令牌，父节点取消会传播到所有子节点。用于中断正在执行的 AI 调用或工具执行。

### 3.9 RunPhaseTracker

共享的原子计数器，跟踪当前执行阶段：

```text
ContextBuild=0
LlmRequest=1
LlmStreaming=2
ToolExecution=3
PostProcessing=4
ProviderWaiting=5
ContextCompacting=6
```

### 3.10 emit 函数

闭包，把 AgentEvent 通过 broadcast channel 发给 TurnSupervisor，TurnSupervisor 再转发给前端。

### 3.11 emit 与事件流

```text
AgentRuntime 内部产生事件
    ↓
emit(event) 发送到 broadcast channel
    ↓
TurnSupervisor 的 select! 收到
    ↓
projector 转成 RPC 帧
    ↓
写回 Unix Socket
    ↓
CLI 的 process_chat_stream 读到
    ↓
显示到终端
```

### 3.12 流式输出 vs payload

- **流式输出：**过程中实时发出的碎片（`TextEvent`、`ThinkingEvent`、`ToolCallEvent`），用户看到文字逐个蹦出。
- **payload：**整个 turn 结束后的最终返回值（完整回复、产物、token 用量、是否暂停）。

### 3.13 句柄

操作系统用来标记你打开的某个东西的编号。打开一个文件，OS 给一个编号，之后用这个编号操作文件。编号就是句柄。

---

## 4. 目录与配置体系

### 4.1 目录结构

以 `agent_id = coder` 为例：

```text
~/.agentix/
├── config.json                 ← 全局配置文件
├── .env                        ← 环境变量文件（存 API key 等）
├── agents/
│   └── coder/
│       └── agent.json          ← 这个 agent 的配置
├── runtime/
│   └── coder/
│       ├── agent.sock          ← Unix Socket
│       ├── pid                 ← PID 文件
│       └── ready               ← 就绪标记文件
├── logs/
│   └── coder/
│       └── daemon.log.2026-09-14-10
│                                ← 日志文件（按小时轮转）
├── sessions/
│   └── coder/                  ← 会话记录
└── plugins/
    └── journals/               ← 插件发布日志（正常完成后为空）
```

### 4.2 配置覆盖关系

全局配置（`config.json`）是底层默认值，agent 配置（`agent.json`）是覆盖。agent 配置里写了的用 agent 的，没写的用全局的。

`config.json`：

```json
{
  "agents": {
    "defaults": {
      "model": "claude-sonnet-4",
      "workspace": "/Users/.../projects"
    }
  }
}
```

其中，`model` 是默认模型。

`agent.json`：

```json
{
  "model": "gpt-4o",
  "workspace": "/Users/.../my-app"
}
```

其中，`model` 和 `workspace` 都覆盖全局配置。

### 4.3 workspace_dir / cwd

`workspace_dir` 是 AI 执行命令和读写文件的工作目录。从 agent 配置的 `workspace` 字段读，或从全局默认值读。daemon 启动时把进程的 `cwd` 设成 `workspace_dir`。

`state_dir` 和 `workspace_dir` 是完全独立的两个目录：

- `state_dir` 存 daemon 的数据（配置、会话、日志）。
- `workspace_dir` 是 AI 操作代码的地方。

### 4.4 会话级模型切换

session 的 `SessionEntry` 有一个 `model` 字段，创建 session 时从配置复制默认值（model、workspace 等），之后 session 独立于配置。

前端切换模型时改的是 session 记录里的 `model`，不是改配置文件。配置始终是默认值，session 是实际值。

### 4.5 谁指导谁的建议

**init 命令**

```text
init 命令
    ↓
创建 state_dir（~/.agentix/）
    ↓
创建 config.json（全局配置）
```

**agent create 命令**

```text
agent create 命令
    ↓
在 state_dir/agents/coder/ 下创建 agent.json
```

**agent start 命令**

```text
agent start 命令
    ↓
在 state_dir/runtime/coder/ 下创建 socket、PID 文件
    ↓
在 state_dir/logs/coder/ 下创建日志文件
    ↓
daemon 进程的 cwd 设成 workspace_dir
```

**chat 命令**

```text
chat 命令
    ↓
在 state_dir/sessions/coder/ 下创建或读取会话记录
    ↓
AI 执行的工具操作在 workspace_dir 下进行
```

---

## 5. init 流程

**入口：**

`crates/cli/src/commands/init.rs` → `run_init()`

### 做了什么

1. 解析 state 目录路径（`~/.agentix`）。
2. 创建必要的目录结构。
3. 加载或创建配置文件。
4. 注册默认 agent（如果配置了）。
5. 设置 provider 默认配置。

---

## 6. start 流程

**入口：**

`crates/cli/src/commands/agent/lifecycle.rs` → `run_start()`

### 6.1 CLI 侧

```rust
pub async fn run_start(
    agent_id,
    verbose,
    provider_startup_check,
    json,
) -> i32 {
    let paths = config::resolve_state_paths()?;
    let options = AgentStartOptions {
        startup_profile,
        provider_startup_check,
    };
    daemon::admin::start_agent_with_options(&paths, agent_id, options).await
}
```

### 6.2 daemon admin 侧

位置：`crates/daemon/src/admin.rs`

`start_agent_with_options` 做了 4 步。

#### 第 1 步：获取生命周期租约

获取 `DaemonLifecycleActionLease`（文件锁），防止同时 start 和 stop 同一个 agent。

#### 第 2 步：采纳已有的旧版 daemon

检查是不是已经有一个旧版 daemon 在跑。如果健康，直接采纳。

#### 第 3 步：准备启动（prepare_agent_start）

- 验证 `agent_id` 合法。
- 确认 agent 在注册表里。
- 恢复上次未完成的生命周期操作。
- 计算配置和启动参数的 SHA-256。

#### 第 4 步：实际启动（start_agent_action）

1. `ensure_startable_runtime`：检查当前状态（Stopped 才能启动）。
2. `spawn_agent_daemon`：fork 子进程。

```rust
let exe = std::env::current_exe()?; // 当前可执行文件

cmd.arg("__daemon") // 用 __daemon 子命令
    .arg("--agent-id")
    .arg(agent_id)
    .arg("--state-dir")
    .arg(paths.state_dir)
    .stdin(null())
    .stdout(log_file)
    .stderr(log_file);

detach_background_process(&mut cmd); // 脱离父进程
spawn_background_without_parent_stdio(&mut cmd); // 实际创建进程
```

**关键：**daemon 不是单独的二进制，就是同一个二进制用 `__daemon` 子命令在后台跑。

3. `bind_spawned_daemon`：把 PID 绑定到生命周期记录。
4. `wait_for_daemon_ready`：轮询 Unix Socket 是否可连接。
5. `finalize_daemon_start`：标记状态为 Running。

### 6.3 start 完整时序

```text
用户执行 agent start coder
    ↓
CLI: run_start("coder")
    ↓
admin::start_agent_with_options
    ├── 获取生命周期租约（文件锁）
    ├── 检查旧版 daemon
    ├── prepare_agent_start → 验证、对账
    └── start_agent_action
        ├── ensure_startable_runtime → Stopped，可以启动
        ├── spawn_agent_daemon → fork 子进程跑 __daemon
        ├── bind_spawned_daemon → 记录 PID
        ├── wait_for_daemon_ready → 轮询 socket
        └── 返回 AgentStartResult
    ↓
finalize_daemon_start → 标记 Running
    ↓
CLI: 输出 "Agent 'coder' started"
```

---

## 7. daemon 内部初始化

子进程跑 `__daemon` 子命令后的流程。

### 7.1 __daemon 入口

CLI 解析到 Daemon 命令后：

`run_foreground()`（`crates/cli/src/commands/agent/lifecycle.rs:166`）

`run_foreground` 做的事：

1. 解析 state 目录和配置文件路径。
2. 加载配置。
3. 设置日志。
4. 安装信号处理（忽略 SIGHUP，监听 SIGTERM/Ctrl+C）。
5. 调用 `daemon::run_daemon_with_options()`。

### 7.2 run_daemon_with_options

位置：`crates/daemon/src/lib.rs:318`

```text
run_daemon_with_options
    ├── 基础准备：路径规范化、配置加载、日志设置、panic hook
    ├── 确认身份：从环境变量读 instance marker，等父进程注册完成
    ├── 构建 RPC registry：注册所有 RPC 方法
    ├── 初始化核心状态：init_daemon_state（创建 AgentRuntime 等所有组件）
    ├── 插件收尾：处理上次残留的插件发布
    ├── 绑定 Socket：创建 agent.sock，写 PID
    ├── 启动前对账：reconcile_durable_state_before_accept
    ├── 启动 listener：spawn accept 循环
    ├── 启动后台任务：metrics、MCP 清理等
    ├── 标记就绪：写 ready marker（父进程等的就是这个）
    ├── 注册生命周期：记录身份到 lifecycle_record
    ├── 等待结束：listener_task.await，daemon 主循环阻塞在这里
    └── 清理退出：停止任务，更新 record 为 Stopped，删文件
```

### 7.3 init_daemon_state

位置：`crates/daemon/src/state/` 目录。

按顺序创建所有零件，最后组装成 `DaemonState`：

1. **准备目录和配置：**创建 agent 目录、runtime 目录、workspace 目录，加载配置。
2. **恢复中断记录：**修复上次崩掉的 AI 调用记录。
3. **创建会话存储 + 审批系统：**storage（读写 session）、ApprovalGate（审批门控）、审批记忆。
4. **创建凭证管理器：**管理 AI 后端的 API key。
5. **创建基础资源：**进程管理器、记忆管理器、AI 后端连接。
6. **创建运行时表面：**工具列表、沙箱、上下文引擎、MCP 路由、LSP 管理器。
7. **创建 agent + 交互门控：**子 agent 管理器、计划确认门控、问题门控。
8. **创建代码指标服务：**记录 AI 修改了哪些文件。
9. **组装 AgentRuntime：**把所有零件装进“大盒子”。
10. **创建 cron 组件：**定时任务存储、执行器。
11. **创建 sidecar + 日志：**辅助进程管理器、日志注册表。
12. **打包成 DaemonState 返回。**

### 7.4 外部怎么找到 daemon

CLI 通过路径规则找到 socket：

```text
state_dir   = ~/.agentix
agent_id    = coder
runtime_dir = ~/.agentix/runtime/coder/
socket_path = ~/.agentix/runtime/coder/agent.sock
```

CLI 和 daemon 用同样的路径规则拼出 socket 路径。CLI 连上 socket 后先发 `agent.info` 验证身份，通过后发业务请求。

---

## 8. session 创建流程

**CLI 入口：**

`crates/cli/src/commands/session/runtime.rs` → `run_create()`

**daemon 入口：**

`crates/daemon/src/handler/session.rs` → `handle_sessions_create()`

创建 session 就是一条 `SessionEntry` 到磁盘存储。创建时从配置复制默认值（model、workspace 等），之后 session 独立于配置。

---

## 9. chat 请求完整流程

### 9.1 CLI 侧

```text
cli::run()                         ← 解析命令行
    ↓
dispatch_chat_command()            ← 路由到 chat
    ↓
run_chat_with_media_with_completion_evidence()
    ├── prepare_chat_client_session()  ← 连接 daemon，确保 session 存在
    ├── prepare_chat_run_context()     ← 打包参数，发 RPC
    ├── client.start_stream("chat.send", ...) ← 发 JSON-RPC 请求
    └── process_chat_stream()          ← 循环读帧，显示到终端
```

CLI 找到 daemon 的方式：

`resolve_running_daemon_client(agent_id)`

1. 解析 `state_dir`。
2. 拼出 `runtime_dir`。
3. `probe_daemon_readiness_async` 检测就绪（检查 ready marker + 连 socket + 发 `agent.info`）。
4. 创建 `DaemonRpcClient` 连接 socket。

### 9.2 daemon 侧

```text
serve_connection()                 ← 读 JSON-RPC 请求
    ↓
dispatch_rpc()                     ← 在 rpc_registry 里查方法名
    ↓
handle_chat_send_streaming()       ← chat 请求入口
    ├── prepare_registered_chat_start() ← 解析参数，找 session + runtime
    ├── admission_check (steer/queue)   ← 排队或直接执行
    ├── acquire_session_turn_or_respond() ← 获取写权（单写锁）
    └── run_streaming_chat_turn()
        └── run_prepared_foreground_stream()
            ├── 构建 StreamContext
            ├── emit stream.started 事件
            └── tokio::join!（两个任务）
                ├── 任务 1：run_stream_context_with_timeout
                │   └── TurnSupervisor::supervise()
                │       ├── start_runtime_stream() ← 准备消息
                │       └── spawn_runtime_handle()
                │           └── TurnExecutor::spawn()
                │               └── handle_message_for_agent_with_options()
                └── 任务 2：run_frame_writer() ← 把事件转成帧写回 socket
```

### 9.3 AgentRuntime 侧

```text
handle_message_for_agent_with_options() ← 转发
    ↓
handle_message_for_pipeline()           ← 转发
    ↓
handle_message_for_pipeline_with_profile() ← 创建 emit 函数，包装配置
    ↓
run_agent_pipeline()                    ← 五阶段流水线
```

---

## 10. run_agent_pipeline 五阶段流水线

位置：`crates/agent/src/runtime/message.rs` → `run_agent_pipeline()`（约 line 1173）。

### 阶段 1：Hook（约 line 1206）

- `run_user_prompt_submit_hooks`：用户消息提交前钩子。
- `delegated_token_budget_preflight`：token 预算预检。
- `run_turn_entry_auto_compact_and_refresh`：进入 turn 时压缩检查。

### 阶段 2：上下文组装（约 line 1263）

- `resolve_pipeline_context` → `build_context`：组装上下文。
  - `select_main_turn_route`：选模型/provider（从 session 的 model 字段读）。
  - `attach_images/docs`：附加图片/文档。
  - `compute token budget`：计算 token 预算。
  - `shape tools`：整理工具列表。
  - `recall memory`：召回记忆。
  - `assemble_prompt_output`：组装提示词输出。
- `emit_lifecycle_status(Started)`：发开始事件。
- `precommit_user_message`：预提交用户消息。

### 阶段 3：调 AI + 工具循环（约 line 1414）

调用链：

```text
run_turn_kernel
    → TurnKernel::run
    → call_with_fallback
    → run_tool_loop_attempt
    → ToolLoopEngine::run_provider_rounds
    → run_provider_rounds_inner（loop{}）
```

调 AI 失败时：

- `rollback_provider_poisoning_user_turn_transcript`：回滚预提交。
- `maybe_commit_allowed_failed_user_turn`：部分失败可保留。

### 阶段 4：后处理（约 line 1516）

- `sanitize_terminal_goal_final_reply`：清理重复回复。
- `session_context_commit_policy`：确定提交策略。
- `run_assistant_reply_hooks`：运行回复钩子。
- `prefix_execution_intent_notice`：执行意图前缀。
- `run_post_turn_skill_evolution`：技能进化。
- `build_reply`：构建回复对象。

### 阶段 5：提交 + 完成（约 line 1610）

- `TurnCommitter::commit`：提交会话记录到磁盘。
- `AfterTurnExecutor::inline`：必需的收尾。
- `run_after_turn_hooks`：after_turn 钩子。
- `emit_lifecycle_status(Completed)`：发完成事件。
- `post_completion_memory_flush`：安排后台记忆刷新。
- 返回 payload。

---

## 11. 工具循环（Tool Loop）

位置：`crates/agent/src/runtime/tool_loop_engine/runner.rs` → `run_provider_rounds_inner()`。

### 11.1 循环步骤

```rust
loop {
    1. prepare_round_or_finish
       // 判断要不要继续

    2. maybe_compact
       // 消息太长就压缩

    3. invoke_llm_and_apply_round_metadata
       // 调 AI

    4. handle_terminal_decision_or_continue
       // AI 还要调工具吗

    5. 取消检查 + steer 注入
       // 用户取消/插话处理

    6. execute_current_round_and_handle_stop
       // 执行工具
}
```

### 11.2 各步详解

**第 1 步：**

检查是否所有工具都执行完了，是否该结束。返回 `true` 就 `break`。

**第 2 步：**

检查 token 数量是否超过压缩阈值（通常是上下文窗口的 80%）。超过就压缩。压缩后 `response_id` 清空（链断了），`current_turn_suffix_start` 更新。

**第 3 步：**

调 AI。有 plan 走 `execute_provider_round`，没有 plan 走普通路径（计算请求大小 → 设置 prompt cache → 调 `invoke_llm`）。记录元数据到 `round_meta`。

**第 4 步：**

AI 没请求工具调用 → 准备结束。请求了 → 返回 `Continue`。

**第 5 步：**

- **取消检查：**用户取消了 → 关闭 pending 工具调用 → emit 取消事件 → 退出。
- **steer 注入：**用户中途插话 → 关闭当前工具调用 → 注入 steer 消息 → `continue`，重新调 AI。

**第 6 步：**

- doom-loop 观测（检查是否重复调用同一工具、同一参数）。
- 逐个执行 AI 请求的工具（可能需要审批，通过 approval gate 阻塞等待）。
- 结果追加到 `tool_exchanges` 和 `context.messages`。
- doom-loop 后处理。

### 11.3 两层阻塞

- **第一层（工具阻塞）：**工具在 `wait_for_approval_response` 里等 oneshot 通道。
- **第二层（模型阻塞）：**`execute_current_round_and_handle_stop` 等所有工具执行完才返回，loop 不会进入下一轮 AI。

### 11.4 current_turn_suffix_start

标记这个 turn 内新消息的起点位置，不是每个 round 的起点，而是整个 turn 新增内容的起点。只有压缩导致前面消息数量变化时才会移动。

### 11.5 response_chain

某些 AI 后端有 `response_id` 续话机制。多轮调用用 `response_id` 串联，避免重复发送历史消息。压缩后历史消息变了，链断了，需要清 `response_id` 重新开始。

### 11.6 tool_exchanges

`Vec<ChatMessage>`，每次工具执行有两条消息：

- Assistant 消息（AI 请求工具调用）。
- Tool 消息（工具返回结果）。

### 11.7 journal

记录消息列表变化历史的账本。压缩后旧账本失效，开新账本（`start_new_epoch`）。

---

## 12. Provider 调用链

### 12.1 完整链路

```text
invoke_llm_and_apply_round_metadata
    └── invoke_llm
        └── consume_llm_stream_attempt
            ← 重试循环（空回复/参数错误重试）
            ├── open_llm_stream_with_repair_retry
            │   ← 发请求 + 读流
            │   └── open_llm_stream_once
            │       ← 真正发网络请求的地方
            │       └── provider.stream_with_options()
            │           ← 发网络请求，到底了
            └── consume_stream_with_visible_tools_tracked
                ← 读流
                ├── text 片段 → emit TextEvent
                ├── thinking 片段 → emit ThinkingEvent
                └── tool_call → 收集起来
```

### 12.2 invoke_llm

有内部 `loop{}`，处理两种重试：

- 空回复（AI 什么都没返回）。
- 工具参数格式错误。

### 12.3 open_llm_stream_once

真正发网络请求的地方。用 `tokio::select!` 同时监听三个信号：

```rust
tokio::select! {
    _ = token.cancelled() => 用户取消,
    _ = sleep_until(deadline) => 超时,
    result = &mut stream_future => AI 响应了,
}
```

`stream_future` 就是“调 AI 这件事”的包装，包成 future 后可以被 `select!` 轮询。

### 12.4 AI 流式响应格式

```json
{"type": "thinking", "content": "用户要读文件"}
{"type": "text", "content": "我来读取"}
{"type": "text", "content": "a.txt"}
{"type": "tool_call", "name": "read_file", "arguments": {"path": "a.txt"}}
{"type": "done", "reason": "tool_use"}
```

---

## 13. 审批 Gate 机制

位置：`crates/agent/src/audit.rs` → `ApprovalGate`。

### 13.1 三种交互机制

```rust
pub enum InteractionOwnerKind {
    Approval,          // 审批
    PlanConfirmation,  // 计划确认
    Question,          // 询问用户问题
}
```

三种机制底层模式相同：

创建 pending 请求 → emit 事件给前端 → 通过 oneshot 通道阻塞等待回复 → 用户操作后通过通道返回。

| 机制 | 什么时候使用 | 前端展示 | 回复类型 |
|---|---|---|---|
| Approval | AI 要写文件/执行命令 | 同意/拒绝按钮 | `bool` |
| Question | AI 信息不足要问用户 | 文本输入框 | `String` |
| PlanConfirmation | AI 写了计划要用户确认 | 计划 + 确认/修改/拒绝 | `PlanDecisionData` |

### 13.2 ApprovalGate 核心结构

```rust
pub struct PendingApproval {
    pub session_key: String,
    pub command: String,
    pub responder: oneshot::Sender<bool>, // 回答通道
}

pub type PendingApprovals =
    Arc<RwLock<HashMap<String, PendingApproval>>>;
```

### 13.3 审批流程

1. 工具调用 `request_with_session(session_key, command)`。
2. 检查审批记忆——之前同意过就自动放行。
3. 创建审批请求，发通知 → TurnSupervisor → emit → 前端弹卡片。
4. `wait_for_approval_response` 用 `select!` 等待用户回答或管理员取消。
5. 清理审批请求。

### 13.4 审批记忆

用户对某操作同意后可记住“决定”，下次同类操作查到记忆就自动放行，不再弹卡片。

---

## 14. Gateway 鉴权

位置：`crates/gateway/src/auth.rs`。

### 14.1 Token 验证

```rust
fn verify_token(request_token: &str, config_token: &str) -> bool {
    let hash_req = sha256(request_token.as_bytes());
    let hash_cfg = sha256(config_token.as_bytes());
    hash_req.ct_eq(&hash_cfg).into()
}
```

- SHA-256 哈希后比较，不直接比较明文。
- 常量时间比较 `ct_eq`，防止时序攻击。

### 14.2 Token 来源

明文字符串、环境变量、文件三种方式。

### 14.3 速率限制

| 来源 | 窗口 | 最大失败次数 | 锁定时间 |
|---|---:|---:|---:|
| 远程 IP | 60 秒 | 10 次 | 300 秒 |
| 本地回环 | 60 秒 | 1000 次 | 60 秒 |

最多跟踪 512 个 IP 的状态。

---

## 15. Sandbox 沙箱

位置：`crates/agent/src/sandbox.rs`、`crates/security/src/isolated.rs`。

### 15.1 Sandbox trait

```rust
pub trait Sandbox: Send + Sync {
    fn resolve_read_path(&self, path: &str) -> AgentixResult<PathBuf>;

    fn resolve_write_path(&self, path: &str) -> AgentixResult<PathBuf>;

    async fn exec(
        &self,
        command: &str,
        timeout_dur: Duration,
    ) -> AgentixResult<ExecOutput>;
}
```

### 15.2 三种实现

| 实现 | 隔离方式 |
|---|---|
| NativeSandbox（默认） | 路径检查 + 命令检查 + 环境变量限制 |
| DockerSandbox | Docker 容器隔离 |
| SrtSandbox | SRT 轻量隔离 |

### 15.3 NativeSandbox 四道检查

1. **路径检查：**展开 `~` → 解析符号链接 → 检查路径是否在允许范围内 → 检查是否是受保护文件的 inode 别名。
2. **危险命令检查：**解析命令，检查是否在删除重要文件、执行网络操作等。
3. **受保护路径检查：**检查命令是否涉及 Agentix 自己的二进制、配置文件。
4. **实际执行：**独立进程、限制 PATH、`kill_on_drop`、限制工作目录。

### 15.4 SealedExecCwd

记录工作目录的 `dev/ino`，执行时再检查。如果目录被替换（删了重建同名目录），拒绝执行，防止 TOCTOU 攻击。

---

## 16. IM 通道（钉钉等）

位置：`crates/channel-host/`。

### 16.1 和三入口的区别

三入口是用户直接连 daemon。IM 通道是通过第三方平台中转。

**三入口：**

```text
用户 → CLI/ACP/Gateway → Unix Socket → daemon
```

**IM 通道：**

```text
用户在钉钉机器人
    → 钉钉服务器
    → HTTP webhook
    → channel-host
    → Unix Socket
    → daemon
    → AI 回复
    → channel-host
    → 钉钉 API
    → 发到群
```

### 16.2 ChannelHostPlugin trait

每个 IM 平台是一个实现：`DingTalkPlugin`、`WeChatPlugin`。

**关键：**到 daemon 之后的所有处理和三入口完全一样。channel-host 只是适配层，把钉钉的消息格式转成 daemon 的 RPC 请求，把 daemon 的事件转成钉钉的消息格式。

---

## 17. 生命周期管理

位置：`crates/daemon/src/lifecycle.rs`、`crates/registry/src/mutation_guard.rs`。

### 17.1 三个东西

#### DaemonLifecycleRecord

状态记录文件，存在磁盘上，记录 daemon 当前阶段：

```rust
pub enum DaemonLifecyclePhase {
    Stopped,   // 没在跑
    Starting,  // 正在启动
    Ready,     // 启动完成，在跑
    Stopping,  // 正在停止
}

pub struct DaemonLifecycleRecord {
    pub phase: DaemonLifecyclePhase,
    pub pid: Option<u32>,
    pub generation: u64, // 第几代（重启一次 +1）
    pub operation_id: Option<String>,
    pub instance_token: Option<String>,
}
```

#### DaemonInstanceMarker（实例标识）

记录“这一次启动”的身份信息（`generation`、`instance_token`、`pid`）。daemon 子进程启动时通过环境变量收到，防止旧 daemon 冒充新 daemon。

#### DaemonLifecycleActionLease（生命周期租约）

文件锁。底层就是 `flock(LOCK_EX | LOCK_NB)` 对一个 `.lock` 文件加排他锁。拿到锁才能改 lifecycle record。Rust 的 `Drop` trait 自动释放：变量离开作用域时自动触发，释放文件锁。

### 17.2 为什么用文件锁而不是内存锁

start/stop 是跨进程操作（CLI 进程操作 daemon 进程），必须用文件锁。文件锁进程崩溃时 OS 自动释放，不会死锁。

### 17.3 文件内容不重要

锁文件的内容不重要，文件可以是空的。重要的是有没有人持有这个文件的排他锁。

`flock` 锁的是文件句柄，不是文件内容。

### 17.4 三者配合

```text
agent start coder
    ↓
1. 获取 ActionLease（文件锁）   ← 拿到锁才能操作
    ↓
2. 读 DaemonLifecycleRecord     ← 看 phase 是不是 Stopped
    ↓
3. prepare_daemon_start         ← 生成 generation、instance_token
    ↓
4. spawn_agent_daemon           ← fork 子进程（环境变量带 instance marker）
    ↓
5. wait_for_daemon_ready        ← 等子进程就绪
    ↓
6. finalize_daemon_start        ← 更新 record：phase=Ready，pid=12345
    ↓
7. ActionLease drop             ← 释放文件锁
```

---

## 18. 插件恢复机制

位置：`crates/daemon/src/handler/agent_plugin_journal_startup.rs`。

### 18.1 什么情况下会没清完

插件安装是多步操作（准备文件 → 移动文件 → 更新配置 → 提交）。daemon 在安装过程中崩了会留下半成品。

journal 文件记录了操作进度，正常完成后删除，崩了就留在磁盘上。

### 18.2 怎么发现

去 `~/.agentix/plugins/journals/` 找 `.json` 文件。有就是未完成的发布。

### 18.3 怎么清理

回滚，不是继续。

读取 journal → 恢复配置到发布前 → 移动文件 → 删除 journal。

因为不知道做到哪一步了，回滚到发布前的状态最安全。

