# Agentix 架构学习指南

> 整理说明：由用户提供的三份学习材料合并。保留第 1–32 章；第二部分开头重复的恢复说明不重复收录，新增的 converge/finish 说明接入第 18 章；第 29 章占位表格由第三份开头的完整表格替换。其余技术叙述保留原文，尚未对照原项目源码核验。原始分段另行保存。

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

|  | `config.set` | `agent.create` | `session.create` |
|---|---|---|---|
| 改什么 | 全局配置 | agent 配置 | session 记录 |
| 防并发 | 配置写入锁 | mutation guard | create guard |
| 写完后 | 触发 reload | 无 | 无 |
| 回滚 | ConfigRollback | 删 agent.json | upsert 错误处理 |
| 原子写入 | 是 | 是 | 是 |

## 30. Rust 宏基础

### 宏是什么

编译时的代码模板。填不同参数生成不同的代码。编译时展开，运行时不存在。

### 声明宏（macro_rules!）

```rust
macro_rules! provider_factory {
    ($name:ident, $api_type:expr, $provider_type:path) => {
        pub struct $name;

        impl ProviderFactory for $name {
            fn api_type(&self) -> ApiType {
                $api_type
            }

            fn create(&self, base_url, api_key) -> ... {
                create_provider_with_validation_cache(
                    self.api_type(),
                    base_url,
                    api_key,
                    None,
                    <$provider_type>::new,
                )
            }
        }
    };
}

provider_factory!(OpenAiFactory, ApiType::OpenAiCompletions, OpenAiProvider);
// 编译时展开成一整段结构体+实现代码
```

### 参数模式

- `$x:expr` — 任何表达式
- `$x:ident` — 标识符（变量名、函数名）
- `$x:ty` — 类型
- `$x:literal` — 字面量
- `$x:path` — 路径（比如 `OpenAiProvider`）

### 为什么用宏

7 个 Provider 工厂代码几乎一模一样，只是名字和类型不同。用宏写一遍模板填 7 次，省掉重复代码。

### 过程宏

`#[derive(Debug)]` 是过程宏。编译时自动给结构体生成 Debug trait 的实现。

---

## 31. 完整调用链速查

```text
CLI: cli::run() → dispatch_chat_command() → run_chat_with_media()
    → resolve_running_daemon_client("coder")
        → state_dir = ~/.agentix, runtime_dir = ~/.agentix/runtime/coder/
        → probe_daemon_readiness → Ready
        → DaemonRpcClient 连 agent.sock
    → start_stream("chat.send")
    → process_chat_stream()（读帧循环）

—— Unix Socket ——

Daemon: serve_connection() → dispatch_rpc()（查 rpc_registry）→ handle_chat_send_streaming()
    → prepare_registered_chat_start() → admission check → acquire_session_turn_permit
    → run_streaming_chat_turn() → run_prepared_foreground_stream()
    → tokio::join!(run_stream_context_with_timeout, run_frame_writer)
        ├─ TurnSupervisor::supervise() → start_runtime_stream() → spawn_runtime_handle()
        │   └─ TurnExecutor::spawn() → handle_message_for_agent_with_options()
        └─ run_frame_writer()（事件 → RPC 帧 → socket）

AgentRuntime: handle_message_for_agent_with_options() → handle_message_for_pipeline_with_profile()
    → run_agent_pipeline()
        ├─ 阶段1: Hook (user_prompt_submit, token budget, auto compact)
        ├─ 阶段2: Context Build (select route, attach, compute budget, shape tools, recall memory)
        ├─ 阶段3: AI + Tool Loop (run_turn_kernel → call_with_fallback → ToolLoopEngine)
        │   └─ loop { prepare → compact → invoke_llm → check terminal → cancel/steer → execute tools }
        │       └─ invoke_llm → consume_llm_stream_attempt → open_llm_stream_once
        │           └─ provider.stream_with_options() [网络请求]
        ├─ 阶段4: Post-processing (sanitize, hooks, skill evolution, build reply)
        └─ 阶段5: Commit + Complete (TurnCommitter, after_turn hooks, emit Completed, memory flush)
    → 返回 payload

—— payload 返回路径 ——

run_agent_pipeline → handle_message_for_pipeline_with_profile → handle_message_for_agent_with_options
    → TurnExecutor::spawn → TurnSupervisor::supervise (select! 收到)
    → run_prepared_foreground_stream → finalize → 释放写权
    → CLI 读到 completed 帧 → chat 命令完成
```

## 32. 关键文件索引

| 文件 | 内容 |
|---|---|
| `src/product_main.rs` | 产品入口，调用 `cli::run()` |
| `crates/cli/src/lib.rs` | CLI 入口，命令解析和分发 |
| `crates/cli/src/cli_args.rs` | 命令行参数定义（含 `__daemon` 子命令） |
| `crates/cli/src/commands/agent/lifecycle.rs` | agent start/stop + `run_foreground`（`__daemon` 入口） |
| `crates/cli/src/commands/chat/mod.rs` | chat 命令实现 |
| `crates/cli/src/commands/chat/stream.rs` | 流式帧读取循环 |
| `crates/cli/src/daemon_client.rs` | CLI 找到 daemon 并连接 |
| `crates/daemon/src/lib.rs` | `run_daemon_with_options` 主函数 |
| `crates/daemon/src/server.rs` | daemon RPC 服务器，`serve_connection` |
| `crates/daemon/src/admin.rs` | agent start/stop 生命周期管理 |
| `crates/daemon/src/handler/mod.rs` | RPC registry 构建 |
| `crates/daemon/src/handler/chat/mod.rs` | daemon chat handler |
| `crates/daemon/src/handler/chat/foreground_stream.rs` | 前台流式执行 |
| `crates/daemon/src/handler/chat/chat_stream.rs` | `StreamContext` 和 runtime spawn |
| `crates/daemon/src/handler/chat/turn_supervisor.rs` | `TurnSupervisor` 事件循环 |
| `crates/daemon/src/handler/chat/goal_restart_reconcile.rs` | 启动时 goal 对账 |
| `crates/daemon/src/thread/executor.rs` | `TurnExecutor` |
| `crates/daemon/src/lifecycle.rs` | `DaemonLifecycleRecord`、`DaemonInstanceMarker` |
| `crates/daemon/src/state/` | `init_daemon_state` 等初始化模块 |
| `crates/daemon/src/handler/agent_plugin_journal_startup.rs` | 插件恢复 |
| `crates/agent/src/runtime/message.rs` | `run_agent_pipeline` 五阶段流水线 |
| `crates/agent/src/runtime/context.rs` | 上下文构建 |
| `crates/agent/src/runtime/fallback.rs` | Fallback chain 和工具循环入口 |
| `crates/agent/src/runtime/tool_loop_engine/runner.rs` | 工具循环核心 |
| `crates/agent/src/runtime/tool_loop_engine/llm_round.rs` | LLM 调用 |
| `crates/agent/src/runtime/message_context_engine.rs` | 自动压缩 |
| `crates/agent/src/audit.rs` | `ApprovalGate` 审批机制 |
| `crates/agent/src/sandbox.rs` | Sandbox 沙箱 |
| `crates/agent/src/question.rs` | Question 交互门 |
| `crates/agent/src/tools/sub_agent.rs` | 子 agent 生成 |
| `crates/agent/src/prompt_cache.rs` | Prompt cache |
| `crates/registry/src/mutation_guard.rs` | `AgentOperationLease` 文件锁 |
| `crates/gateway/src/auth.rs` | Gateway 鉴权 |
| `crates/channel-host/src/plugin.rs` | IM 通道插件 trait |
| `crates/security/src/isolated.rs` | Sandbox trait 定义 |
| `crates/common/src/message.rs` | `SessionEntry`、`RuntimeSessionSnapshot` |
| `crates/common/src/checkpoint.rs` | `CheckpointArtifact` |
| `crates/common/src/profiling.rs` | `RunPhase`、`RunPhaseTracker` |
| `crates/common/src/event.rs` | `PlanData`、`PlanEntryData` |
| `crates/kit/src/platform/fs_lock.rs` | flock 文件锁实现 |
| `crates/kit/src/platform/ipc.rs` | Unix Socket / Named Pipe 绑定 |
| `crates/daemon/src/handler/config/reload.rs` | config reload 热重载 |
| `crates/daemon/src/state/core.rs` | `DaemonState` 结构定义 |
| `crates/daemon/src/state/core_daemon_state_impl.rs` | `DaemonState` 实现（`replace_dynamic_mainline` 等） |
| `crates/daemon/src/state/turn_admission.rs` | turn 准入管理和 `runtime_owners` 跟踪 |
| `crates/daemon/src/derived_model_run_store.rs` | 派生模型调用记录恢复 |
| `crates/agent/src/mcp.rs` | `McpClient` 定义和传输方式 |
| `crates/agent/src/mcp/router.rs` | MCP Router 路由器 |
| `crates/agent/src/tools/sub_agent.rs` | Sub-agent 工具和委派逻辑 |
| `crates/agent/src/tools/sub_agent/lifecycle.rs` | Sub-agent 生命周期（准入、启动、等结果） |
| `crates/agent/src/tools/sub_agent/lifecycle_execution.rs` | Sub-agent 执行 |
| `crates/agent/src/subagent.rs` | `SubAgentManager`（管理中心） |
| `crates/agent/src/memory/mod.rs` | `MemoryEngine` trait、`MemoryEntry` |
| `crates/agent/src/memory/recall.rs` | 召回逻辑（查询、去重、过滤、排序、选胜者） |
| `crates/agent/src/memory/bundled.rs` | 内置引擎适配器 |
| `crates/agent/src/runtime/context_memory_recall.rs` | 召回入口 |
| `plugins/memory-engines/relic/` | relic 引擎实现 |
| `crates/daemon/src/admin_lifecycle_control.rs` | agent stop 流程 |
| `crates/provider/src/factory.rs` | Provider 工厂模式 |
| `crates/provider/src/openai.rs` | OpenAI Provider 实现 |
| `crates/provider/src/anthropic_transport.rs` | Anthropic Provider 实现 |
| `crates/provider/src/ollama.rs` | Ollama Provider 实现 |
| `crates/cron/src/scheduler.rs` | `CronScheduler` 调度器 |
| `crates/cron/src/executor.rs` | `CronExecutor` trait |
| `crates/cron/src/store.rs` | `CronStore` 持久化 |
| `crates/daemon/src/cron_executor.rs` | daemon 的 `CronExecutor` 实现 |
| `crates/common/src/cron.rs` | `CronJob`、`CronSchedule` 数据结构 |
| `crates/daemon/src/handler/config/mod.rs` | `config.set` 处理和写入 |
| `crates/daemon/src/handler/session.rs` | `sessions.create` 处理 |
| `crates/daemon/src/handler/session_creation.rs` | session 创建逻辑 |
| `crates/daemon/src/state/turn_admission.rs` | turn 准入和 runtime owner 跟踪 |