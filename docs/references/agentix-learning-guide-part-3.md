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