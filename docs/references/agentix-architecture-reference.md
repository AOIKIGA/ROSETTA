# Agentix 架构复刻参考

本文档从复刻角度提炼 Agentix 的核心架构，省去细节枝叶，聚焦“必须有什么、怎么组织、怎么跑起来”。

## 1. 整体架构

```text
┌────────────┐  ┌────────────┐  ┌────────────┐
│    CLI     │  │    ACP     │  │   Gateway  │
│  (命令行)   │  │ (IDE集成)  │  │  (HTTP/WS) │
└─────┬──────┘  └─────┬──────┘  └─────┬──────┘
      └───────────────┼───────────────┘
                      │
             Unix Socket
               (JSON-RPC)
                      │
             ┌────────▼────────┐
             │     Daemon      │
             │                 │
             │  ┌───────────┐  │
             │  │AgentRuntime│  │
             │  │           │  │
             │  │ Provider  │  │
             │  │ Tools     │  │
             │  │ Sandbox   │  │
             │  │           │  │
             │  │ Memory    │  │
             │  │ Engine    │  │
             │  │ Context   │  │
             │  │ Engine    │  │
             │  └───────────┘  │
             │                 │
             │ Storage Config  │
             │ Cron             │
             └─────────────────┘
```

### 核心原则

1. **一 Agent 一 Daemon**：一个 agent 对应一个 daemon 进程，独立启停。
2. **同一二进制**：CLI 和 daemon 是同一个可执行文件，daemon 通过 `__daemon` 子命令在后台运行。
3. **三入口统一**：CLI、ACP、Gateway 都通过 Unix Socket 发送 JSON-RPC 请求给 daemon。
4. **流式通信**：daemon 和 AI 后端之间是流式的，daemon 和前端之间也是流式的。

## 2. 目录结构

```text
~/.agentix/
├── config.json                 # 全局配置
├── .env                        # 环境变量（API Key 等）
├── agents/
│   └── {agent_id}/
│       ├── agent.json          # Agent 配置（覆盖全局配置）
│       ├── approval_memory.json # 审批记忆
│       ├── checkpoints/        # 快照
│       └── skills/             # 技能
├── runtime/
│   └── {agent_id}/
│       ├── agent.sock          # Unix Socket
│       ├── pid                 # PID 文件
│       └── ready               # 就绪标记
├── logs/
│   └── {agent_id}/
│       └── daemon.log          # 日志
├── sessions/
│   └── {agent_id}/             # 会话记录
└── plugins/                    # 插件
```

其中，`state_dir` 是所有数据的根目录。

### 配置覆盖关系

全局配置是底层默认值，Agent 配置是覆盖层。

- Agent 配置里写了的，使用 Agent 配置。
- Agent 配置里没写的，使用全局配置。

### workspace_dir

AI 执行命令和读写文件的工作目录，与 `state_dir` 完全独立。

工作目录从 Agent 配置的 `workspace` 字段读取；如果没有配置，则使用全局默认值。

## 3. 核心模块

### 3.1 Daemon（进程级）

负责管理“daemon 这个进程在跑什么、配置是什么、谁连着它”。

持有：

- 进程信息：`agent_id`、`state_dir`、`config`、`cancel_token`
- `AgentRuntime`：核心运行时
- 会话存储：`storage`
- 交互门控通道：`approval`、`plan`、`question`
- 请求管理：排队、准入、写权
- RPC 路由表
- Cron 定时任务
- 各种锁和缓存

### 3.2 AgentRuntime（运行时级）

负责管理“AI 怎么调、工具怎么执行、上下文怎么组装”。

持有：

- `providers`：AI 后端连接
- `tools`：工具列表
- `sandbox`：沙箱
- `context_engines`：上下文引擎，负责组装消息、压缩上下文
- `memory_manager`：记忆系统
- `approval_gate`：审批门控
- `subagent_manager`：子 Agent 管理

**职责边界：**

AgentRuntime 只管“拿到消息后，组装上下文、调 AI、执行工具、返回回复”。

其他事情，例如请求怎么来、怎么排队、怎么定时，都是 daemon 的职责。

## 4. 生命周期管理

### 4.1 文件锁机制

`start` / `stop` 操作前先获取文件锁（`flock`），防止同时操作同一个 Agent。

文件内容不重要，重要的是有没有人持有排他锁。进程崩溃时，操作系统会自动释放锁。

### 4.2 生命周期状态机

```text
Stopped → Starting → Ready → Stopping → Stopped
```

状态记录在磁盘上的 `DaemonLifecycleRecord` 中，包括：

- `phase`
- `pid`
- `generation`
- `instance_token`

### 4.3 start 流程

1. 获取文件锁。
2. 检查状态，必须为 `Stopped`。
3. `fork` 子进程，运行 `__daemon` 子命令。
4. 等待 Socket 可连接，由 daemon 在子进程中完成初始化。
5. 标记为 `Ready`。
6. 释放文件锁。

### 4.4 stop 流程

1. 获取文件锁。
2. 检查状态，必须为 `Ready`。
3. 发送 `SIGTERM`，daemon 收到后触发 `cancel`，清理并退出。
4. 轮询等待进程退出。
5. 如果仍未退出，发送 `SIGKILL`。
6. 清理文件，标记为 `Stopped`。
7. 释放文件锁。

### 4.5 CancellationToken

取消机制采用信号通知。

`cancel()` 后，所有通过 `clone` 派生的副本都能感知到取消。每个组件自行检查 `is_cancelled()`，决定是否提前退出。

取消令牌只负责发信号，不自动终止进程。

## 5. Daemon 初始化

子进程跑完 `__daemon` 后，按以下顺序初始化：

1. 路径规范化、加载配置、设置日志。
2. 插件恢复：回滚上次没有收尾的插件安装。
3. 创建会话存储和审批系统，包括 `ApprovalGate`、审批记忆。
4. 创建凭证管理器，管理 AI 后端的 API Key。
5. 创建基础资源：进程管理器、记忆管理器、AI 后端连接。
6. 创建运行时表面：工具列表、沙箱、上下文引擎、MCP 路由。
7. 创建子 Agent 管理器和交互门控，包括计划确认、问题询问。
8. 组装 `AgentRuntime`，把所有零件装进运行时。
9. 创建 Cron 组件。
10. 绑定 Unix Socket。
11. 启动前对账，恢复上次崩溃的 goal 状态。
12. 启动 listener 和后台任务。
13. 写入 `ready` marker，告诉父进程“我好了”。

## 6. Chat 请求完整流程

### 6.1 CLI 侧

用户执行：

```bash
agentix chat --agent coder --session main -m "读一下 a.txt"
```

CLI 的处理流程：

1. 解析命令行参数。
2. 连接 daemon，通过路径规则找到 `agent.sock`。
3. 发送 JSON-RPC 请求 `chat.send`。
4. 循环读取流式帧，并显示到终端。

### 6.2 Daemon 侧

收到请求后：

1. 在 RPC 路由表中找到 `chat.send`。
2. 调用 `handle_chat_send_streaming`。
3. 解析参数，找到 session 和 `AgentRuntime`。
4. 执行准入检查，包括排队或直接执行。
5. 获取 session 写权，同一 session 同时只能有一个 turn。
6. 构建 `StreamContext`。
7. 创建 `TurnSupervisor`，负责事件循环，监听 AI 事件、超时、取消和审批。
8. 通过 `TurnExecutor::spawn` 调用 `handle_message_for_agent_with_options`。

### 6.3 AgentRuntime 侧：`run_agent_pipeline` 五阶段

#### 阶段 1：Hook

执行用户消息提交前的钩子和压缩检查。

#### 阶段 2：上下文组装

- 选择模型和 Provider，从 session 的 `model` 字段读取。
- 附加图片和文档。
- 计算 Token 预算。
- 整理工具列表。
- 召回记忆。
- 组装系统提示词。
- 预提交用户消息到磁盘。

#### 阶段 3：调用 AI + 工具循环

```text
run_turn_kernel
    ↓
call_with_fallback
    ↓
ToolLoopEngine
    ↓
循环：
  判断是否结束
    → 压缩上下文
    → 调用 AI
    → 判断是否需要工具
    → 检查取消和用户插话
    → 执行工具
    → 将结果追加到消息列表
```

#### 阶段 4：后处理

- 清理重复回复。
- 执行回复钩子。
- 构建回复对象。

#### 阶段 5：提交与完成

- 将所有消息写入磁盘。
- 发送完成事件给前端。
- 安排后台记忆刷新。
- 返回 payload。

### 6.4 前端看到的事件流

```text
stream.started        # 开始
thinking("...")       # AI 思考
text("我来读")        # AI 文字，逐个输出
tool_call(...)        # 工具调用（如需审批，弹卡片）
tool_result(...)      # 工具结果
text("a.txt 是...")   # AI 继续回复
completed             # 完成
```

## 7. 工具循环

核心循环，每轮做 6 件事：

```text
loop {
    1. 判断是否需要继续
       （所有工具执行完了吗？该结束了吗？）

    2. 消息太长就压缩
       （Token 超过上下文窗口的 80%）

    3. 调 AI
       （发请求，拿流式回复和工具调用请求）

    4. AI 还要调工具吗？
       （没工具调用 → 结束；有 → 继续）

    5. 用户取消了吗？用户插话了吗？
       （取消 → 退出；插话 → 重新调用 AI）

    6. 执行工具
       （逐个执行 AI 请求的工具，结果追加到消息列表）
}
```

### 7.1 Turn vs Round

- **Turn**：用户发一条消息到 AI 完成回复，算一个 turn。
- **Round**：工具循环里调用一次 AI，算一个 round。
- 一个 turn 可能包含多个 round，因为 AI 中间可能需要调用工具。

### 7.2 `response_chain`

某些 AI 后端有 `response_id` 续话机制。多轮调用用 `response_id` 串联。

压缩后历史消息发生变化，原来的链可能断开，需要清除 `response_id` 并重新开始。

### 7.3 Doom-loop 检测

检测 AI 是否在重复调用同一工具、同一组参数。

检测到后，向 AI 发送提醒消息。

### 7.4 Steer（用户中途插话）

检测到用户插话后：

1. 关闭当前工具调用。
2. 注入 `steer` 消息。
3. 重新调用 AI。

## 8. Provider 适配层

### 8.1 Provider 是什么

Provider 相当于翻译官：把统一格式的请求转换成各家后端的格式发送出去，再把返回结果转换成统一格式传回来。

### 8.2 核心 trait

```rust
pub trait Provider: Send + Sync {
    fn id(&self) -> &str;

    fn api_type(&self) -> ApiType;

    async fn stream_with_options(
        &self,
        model: &str,
        context: &StreamContext,
        options
    ) -> StreamResult;
}
```

### 8.3 统一事件格式

| 事件 | 含义 |
|---|---|
| `TextDelta` | AI 输出了一段文字 |
| `ThinkingDelta` | AI 输出了一段思考 |
| `ToolCallCompleted` | AI 请求调用一个工具 |
| `Usage` | Token 用量 |
| `Done` | AI 回复完毕 |

### 8.4 工厂模式

根据配置里的 `api_type` 创建对应 Provider。

所有 Provider 存在一个 `HashMap` 中，key 是 Provider 名字。调用时，根据 session 的 `model` 字段找到对应 Provider 并取出来。

### 8.5 Stream 机制

Provider 发送 HTTP 请求后，AI 后端通过 SSE 一条条推送事件。

Provider 将字节流转换为统一的事件流。AgentRuntime 使用异步循环读取：

```rust
while let Some(event) = stream.next().await {
    // 处理事件
}
```

`await` 保证异步等待数据时不会阻塞整个线程。

## 9. 审批与交互门控

### 9.1 三种交互机制

| 机制 | 什么时候使用 | 回复类型 |
|---|---|---|
| Approval | AI 要写文件或执行命令 | `bool`，同意或拒绝 |
| Question | AI 信息不足，需要询问用户 | `String` |
| PlanConfirmation | AI 写了计划，需要用户确认 | `PlanDecision` |

### 9.2 通用流程

底层使用相同模式：

1. 创建 pending 请求。
2. 通过通道通知 `TurnSupervisor`。
3. 转发给前端并弹出交互卡片。
4. 通过 oneshot 通道阻塞等待回复。
5. 用户操作后，通过通道返回结果。

### 9.3 审批记忆

用户对某操作同意后，可以选择记住决定。

下次遇到同类操作时，如果命中审批记忆，就自动放行，不再弹出审批卡片。

### 9.4 两层阻塞

工具在 oneshot 通道阻塞等待回复，工具循环则等待所有工具执行完成后才进入下一轮。

因此，在等待用户审批期间，AI 不会被继续调用。

## 10. Sandbox 沙箱

### 10.1 四道检查

1. **路径检查**：解析符号链接，检查路径是否在允许范围内，并检查 inode 别名。
2. **危险命令检查**：解析命令，检查是否涉及删除重要文件、执行网络操作等。
3. **受保护路径检查**：检查命令是否涉及 Agentix 自己的文件。
4. **进程隔离**：使用独立进程组、限制 `PATH`、`kill_on_drop` 和工作目录。

### 10.2 三种实现

- `NativeSandbox`：默认实现，进行路径检查和命令检查。
- `DockerSandbox`：通过容器隔离。
- `SrtSandbox`：轻量级隔离。

## 11. 上下文管理与压缩

### 11.1 上下文引擎

负责组装发送给 AI 的消息列表，包括：

- 系统提示词。
- 用户消息。
- 历史消息。
- 工具列表。
- 记忆召回结果。

### 11.2 自动压缩

消息列表 Token 数量超过上下文窗口的 80% 时触发压缩。

压缩时，将旧消息总结成摘要并替换掉原消息。

压缩后 `response_id` 链可能断开，需要重新开始。

### 11.3 熔断器

连续 3 次压缩失败后，进入 60 秒冷却。

### 11.4 两种压缩时机

- Turn 开始时。
- 工具循环中途。

### 11.5 Prompt Cache

系统提示词和历史消息可能被 AI 后端缓存。

多轮对话中，系统提示词每次都会发送，但缓存可以避免重复处理。

压缩后，原有的 cache 断点可能失效。

## 12. 记忆系统

### 12.1 架构

记忆引擎采用插件形式，只负责存储、搜索、删除，类似向量数据库。

召回的排序、过滤和最终选择由 runtime 层负责。

### 12.2 三层记忆

| 层 | 用途 | 跨会话 |
|---|---|---|
| `semantic` | 长期记忆 | 全局跨会话 |
| `working` | 临时记忆 | 不跨会话 |
| `episode` | 事件记忆 | 不跨会话 |

### 12.3 三条召回路径

1. **环境注入**：每次用户发送消息时，自动触发记忆引擎，排序后选取得分最高的一条注入提示词，最多 1 条。
2. **编程偏好注入**：查询所有用户确认过的偏好，全部注入。
3. **显式查询**：AI 主动调用 `memory_search` 工具查找记忆。

### 12.4 排序规则

纯规则排序，不调用 LLM：

```text
没过期 > 手动存 > 可靠性高 > 时间匹配 > 置信度高 > 更新
```

### 12.5 编程偏好状态流转

```text
候选
  → AI 判断是否值得记住
  → 询问用户确认
  → 确认后才存储
```

用户不确认，则不存储、不注入。

## 13. Sub-agent

### 13.1 机制

主 Agent 通过 `spawn_subagent` 工具派生子 Agent。

子 Agent 具有独立 session，继承父配置，使用同一个 `AgentRuntime`，走完全相同的 pipeline。

### 13.2 流程

```text
AI 调用 spawn_subagent("分析 a.txt")
    ↓
准入检查
    ↓
创建子 session（继承父配置）
    ↓
写入委派合约（Token + Budget）
    ↓
后台启动执行
    ↓
立即返回 {"status": "running", "run_id": "xxx"}
    ↓
子 Agent 独立执行 run_agent_pipeline
    ↓
完成后将结果存到 SubAgentManager
    ↓
AI 调用 wait_subagents("xxx") 获取结果
```

### 13.3 委派合约

为子 Agent 的 session 设置约束：

- 谁派来的。
- 允许使用什么工具。
- 能使用多少资源，包括 Token、工具调用次数和时间。
- 是否允许继续派生子 Agent。

### 13.4 不需要 TurnSupervisor

子 Agent 不直接与前端交互，事件通过 monitor 转发给主 Agent 的 `TurnSupervisor`。

## 14. Checkpoint / Rewind

### 14.1 自动触发

每个 turn 结束时自动创建 checkpoint，记录这一轮修改了哪些文件的 before/after 内容。

### 14.2 存储内容

- `turn_boundary`：turn 开始前消息列表有多少条，用于回退对话。
- `rewind_point`：这一轮修改了哪些文件的 `before_content` 和 `after_content`。

### 14.3 如何回退

```bash
rewind checkpoint_1 --apply --mode all
```

执行过程：

1. 聚合从目标 checkpoint 到最新的所有 checkpoint 的文件变更记录。
2. 每个文件取最早的 `before_content`。
3. 写回 `before_content`，删除之后新建的文件。
4. 将消息列表截断到目标位置。

### 14.4 三种模式

- `All`：文件和对话都回退。
- `ConversationOnly`：只回退对话。
- `FilesOnly`：只回退文件。

### 14.5 DryRun 与 Apply

- `DryRun`：只返回预览，包括哪些文件会被恢复、哪些存在冲突。
- `Apply`：实际修改文件。

## 15. Config Reload 热重载

### 15.1 核心矛盾

正在进行的 turn 使用旧的 `AgentRuntime`，不能被打断。

### 15.2 解决方案

turn 开始时，从 `turn_admission` 中固定 `AgentRuntime` 引用。

热重载时，替换的是 `turn_admission` 中的引用，而不是已经开始的 turn 持有的引用。

旧 runtime 等所有使用它的 turn 结束后，才清理（`drain_lease`）。

### 15.3 七个问题及解法

| 问题 | 解法 |
|---|---|
| 正在进行的 turn 被打断 | turn 固定 runtime 引用，重载只替换 `turn_admission` 中的引用 |
| 审批丢失 | 旧 `ApprovalGate` 随旧 runtime 存活，pending 请求保存在 `DaemonState` |
| MCP 连接断开 | 指纹比对，未变化则复用，变化后等旧 turn 完成才清理 |
| 并发重载 | 三道锁串行化 |
| 配置写入一半崩溃 | 原子写入，失败回滚 |
| 客户端被断连 | 冻结租约，等待所有配置更新完成后再检查 |
| API Key 更换 | `reconcile` 只更新新 Provider 的 Key，已经发出的请求不受影响 |

## 16. Gateway 鉴权

### 16.1 Token 验证

对 Token 进行 SHA-256 哈希后，再采用常量时间比较，不直接比较明文，防止时序攻击。

### 16.2 速率限制

- 远程 IP：60 秒内失败 10 次，锁定 300 秒。
- 本地回环：60 秒内失败 1000 次，锁定 60 秒。

## 17. IM 通道

通过第三方平台（如钉钉）中转。

`channel-host` 是适配层，负责：

1. 将钉钉消息格式转换成 daemon 的 RPC 请求。
2. 将 daemon 的事件转换成钉钉的消息格式。

消息进入 daemon 后，所有处理逻辑与其他入口完全一致。

## 18. Cron 定时任务

`CronScheduler` 在后台运行，每秒检查所有定时任务，到点后执行。

执行时创建 session，发送消息给 AgentRuntime，走与 chat 相同的 pipeline。

并发数量有限制，同一任务不能同时重复执行。

### 三种调度方式

- 一次性（`At`）。
- 间隔（`Every`）。
- Cron 表达式。

### 三种执行目标

- 主 session。
- 临时独立 session（默认）。
- 指定 session。

## 19. 复刻建议

### 19.1 最小可运行版本

如果要复刻一个最小版本，必须实现：

1. **CLI 入口**：解析命令行参数，发送 RPC 请求。
2. **Daemon 进程**：监听 Unix Socket，接收 JSON-RPC 请求。
3. **RPC 路由**：将方法名映射到处理函数。
4. **会话存储**：session 记录的读写。
5. **AgentRuntime**：组装上下文、调用 AI、执行工具。
6. **Provider**：至少适配一个 AI 后端，例如 OpenAI。
7. **工具列表**：至少实现 `read_file`、`write_file`、`exec`。
8. **流式输出**：将 AI 返回的流式事件转发给前端。

### 19.2 可以先省略的功能

- Sandbox：先不做隔离，直接执行。
- 审批 Gate：先不做审批，工具直接执行。
- 记忆系统：先不做记忆召回。
- 子 Agent：先不做并行任务。
- Checkpoint / Rewind：先不做快照。
- Cron：先不做定时任务。
- IM 通道：先不接钉钉。
- Config Reload：先不支持热重载，改配置后重启。
- 插件系统：先不做插件化。

### 19.3 建议的实现顺序

**第 1 步：CLI + Daemon + Unix Socket + JSON-RPC**

- 能发起请求。
- 能收到回复。

**第 2 步：会话存储 + session create**

- 能创建会话。

**第 3 步：Provider 适配（OpenAI）+ 流式输出**

- 能调用 AI。
- 能流式返回结果。

**第 4 步：`run_agent_pipeline` 五阶段**

- 能完整跑完一个 turn。

**第 5 步：工具循环 + 工具列表**

实现：

- `read_file`
- `write_file`
- `exec`

使 AI 能调用工具。

**第 6 步：上下文组装 + 自动压缩**

- 多轮对话不会超过上下文窗口。

**第 7 步：审批 Gate**

- 写文件、执行命令前询问用户。

**第 8 步：生命周期管理（start/stop + 文件锁）**

- 能启停 daemon。

**第 9 步：按需添加其他功能**

例如记忆、子 Agent、Checkpoint、Cron 等。

### 19.4 关键设计决策

1. **Daemon 和 CLI 是同一个二进制**：用子命令区分。
2. **Unix Socket 通信**：比 HTTP 轻量，不需要端口。
3. **流式 JSON-RPC**：一行一个 JSON 帧，不是一次性返回。
4. **Turn 级别的写权锁**：同一个 session 同时只能有一个 turn。
5. **Provider trait 抽象**：统一不同 AI 后端的接口。
6. **AgentRuntime 和 DaemonState 分层**：runtime 只管执行，daemon 管理其余生命周期和调度工作。
7. **插件化记忆引擎**：引擎只管存、搜、删，业务逻辑在 runtime 层。
8. **文件锁做生命周期管理**：跨进程互斥，进程崩溃时由操作系统自动释放。
