# 文件工具与第一版子 Agent

日期：2026-10-24。本次增加 `list_files`、`read_files` 和阻塞式 `subagent`。后台进程的启动和关闭仍按第五课运行。

> 2026-10-30 更新：本文保留该阶段开发时的说明，main 现已整合第六至八课的能力。父会话持久化、历史记忆和文件/子 Agent 工具可同时使用；子任务内部过程仍不独立落盘。整合说明见 [第九课](09-merging-branches.md)。

## 1. 这次多了什么

现在主 Agent 有四个工具：

| 工具 | 做什么 |
|---|---|
| get_time | 查实际时间 |
| list_files | 查看工作目录中某一层有哪些文件 |
| read_files | 读取工作目录中的文本文件 |
| subagent | 把一项任务交给子 Agent，等它完成后拿回结果 |

子 Agent 也使用已有的 Runtime，拥有前三个工具，但没有 subagent 工具。这一版只允许一层委派。

子 Agent 不会启动另一个 daemon，也不会创建 `agents/<名称>/`。它是当前 daemon 内的一次独立模型/工具循环，共享模型配置和只读工作目录，但拥有自己的消息列表和运行计数。

## 2. state dir 与 workspace 的区别

state dir 保存程序自己的配置、进程锁和日志。workspace 是模型通过文件工具能够读取的目录。

默认路径：

```text
~/.rosetta/agents/personal/
├── agent.json
├── run/
├── logs/
└── workspace/    ← 文件工具默认从这里读取
```

如果希望读取自己的项目，在启动时指定：

```sh
./bin/rosetta stop personal
./bin/rosetta start personal --workspace /Users/aoki/Documents/ROSETTA
```

`--workspace` 的相对路径按执行启动命令时的目录解析，然后转换为绝对路径传给后台进程。后台进程改变自己的工作位置，也不会让文件工具读错地方。

此参数只影响本次运行。想保存为默认配置，在 agent.json 中增加 `workspace` 字段，值必须是绝对路径。省略或空字符串使用 Agent 自己的 workspace。指定的外部目录必须已经存在；默认 workspace 会自动创建。已有 Agent 配置无需迁移。

这里是文件工具的读取边界，不是整个进程的操作系统沙箱。如果手动给两个 Agent 指定同一 workspace，它们就可以读取相同文件。使用真实模型时，工具读取的内容会作为对话上下文发送给模型，因此应选择你希望交给助手处理的目录。

## 3. 文件工具如何工作

模型请求：

```json
{"paths":["README.md","go.mod"],"start_line":1,"max_lines":80}
```

Runtime 将这组参数交给 read_files。工具从已打开的 workspace 目录读取文件，返回每个文件的路径、起止行、内容、是否截断，以及可能的错误。

一个文件不存在时，其他文件仍可返回结果。整个结果会标为有错误，模型可以根据逐文件错误修正路径。

当前限制：

- 只能使用相对路径，不能用绝对路径或 `..` 越出目录。
- 使用 Go 的 `os.Root` 约束读取位置，指向目录外的符号链接也不能逃出边界。
- 路径最后一项如果是符号链接，拒绝打开；只读普通文件，不读设备或命名管道。
- 一次最多四个文件，每个文件最多 1 MiB，必须是 UTF-8 文本且不能包含 NUL 字节。
- 起始行为 1–100000，每次最多 200 行，默认 80 行。每个文件片段还受 1600 字节上限约束，长行可能只返回前半段。
- 整个工具结果最多 8 KiB。为保持 JSON 完整，必要时缩短内容，并设置 truncated，而不是直接从 JSON 中间剪断。
- list_files 不递归，最多返回 100 个条目；结果过大还会减少条目。截断时不保证是整个目录按名称排序的前 100 项，目前没有目录翻页接口。

普通文件工具保留三秒调用超时。读取前后检查取消；对本地普通文件使用有界读取。这个超时不是操作系统强制终止文件系统调用的机制。

## 4. 子 Agent 走哪条路

例如你让主 Agent：“找一个助手读 README，告诉我项目现在能做什么。”

```text
父 Agent 的模型
    ↓ 请求 subagent，给出完整任务说明
subagent 工具
    ↓ Start：创建带 ID 的子任务
Task Manager
    ↓ 子 Runtime：调用模型 → 读文件 → 再调用模型
subagent 工具
    ↓ Wait：等子任务结束
把 task_id、状态、最终回答作为工具结果交回父模型
    ↓
父模型结合结果，给你最终回答
```

子 Agent 不自动获得父 Agent 的聊天记录。父模型必须在 prompt 参数中提供必要信息。这样不会把父会话里的全部内容无意间交给子任务。

子 Agent 的中间文字也不会直接打印到你的终端。终端会看到父 Agent 的 subagent 工具进度；子 Agent 最后的回答作为一个工具结果交给父模型，由父模型组织回复。

这与时间工具遵循同一个模式：工具提供一个结果，父模型决定怎样使用。特殊之处只是 subagent 内部又运行了一次模型/工具循环。

## 5. 为什么现在阻塞，但仍保留 Start / Status / Wait

内部任务接口已经拆成：

```go
Start(ctx, request)   // 创建任务，得到 ID
Status(ctx, id)       // 立即查看状态与进度
Wait(ctx, id)         // 等待任务结束，得到结果
```

当前 subagent 工具执行 `Start` 后立即调用 `Wait`，所以父 Agent 必须等子 Agent 返回才能继续。

Status 可以读取 running/completed/failed/canceled、当前模型轮次、正在执行的工具、起止时间和最终结果。取消 Wait 的 context 只代表“不再等”，并不直接取消任务；任务本身跟随创建时的 context。当前工具的等待和任务使用同一调用 context，因此父请求取消或工具超时时，子任务也会停止。

以后可以增加返回 task_id 后立即结束的工具形式，再通过 status/wait 查询。这些命令行入口本轮没有开放。

**接口预留不等于已经实现后台子任务。** 目前 Task Manager 属于单个父 turn，父 turn 结束后不保留记录。以后开放跨请求的 status/wait，还需要把任务管理放到 daemon 的较长生命周期中，增加所属会话检查和记录保存，不能只加一条 CLI 命令。

## 6. 如何避免无限委派

| 项目 | 当前限制 |
|---|---|
| 每个父 turn 的子任务总数 | 最多两个，包括已经结束的任务 |
| 子任务再创建子任务 | 不允许，工具列表没有 subagent |
| 父 Runtime | 最多六次模型调用、八次工具调用 |
| 每个子 Runtime | 最多三次模型调用、四次工具调用 |
| 父 turn 整体时间 | 两分钟，包含等待子任务的时间 |
| 单次 subagent 工具时间 | 一分钟，还受父请求剩余时间限制 |
| 子任务最终结果 | 最多 4096 字节，必要时继续缩短以满足工具 JSON 大小限制 |

因此一次父请求最多可能产生 12 次模型调用、16 次工具调用（含父层的委派调用）。这是父子合计的上限，不再是整个请求只有六次模型调用；使用真实模型时，子 Agent 同样消耗模型用量。

父请求取消时，子任务收到取消通知；Runtime 返回前会等待子任务退出。不同父请求各自创建 Task Manager，不能通过另一请求的 task_id 查询其结果。此隔离是本轮内存对象的隔离，不是已经实现了用户登录和权限系统。

子 Runtime 轮数耗尽或模型调用失败，会成为带错误标识的工具结果，父模型可以解释失败或调整做法。父请求取消或 subagent 调用超时则结束当前父 turn。失败时不把子模型的半截文字当作成功答案。

## 7. 离线体验

以下示例使用 mock，不联网、不产生模型费用。先编译并重启旧 daemon：

```sh
make build
./bin/rosetta stop personal
./bin/rosetta start personal --workspace /Users/aoki/Documents/ROSETTA
./bin/rosetta chat personal -m /ls
./bin/rosetta chat personal -m '/read go.mod'
./bin/rosetta chat personal -m '/delegate /read go.mod'
```

`/ls`、`/read`、`/delegate` 是 mock 的固定演示口令，不是新增的 CLI 子命令。Mock 真正执行文件工具和子 Runtime，但不会理解自然语言任务；它会展示工具结果的有限片段。

在 DeepSeek 模式中直接用自然语言：

```sh
./bin/rosetta chat personal -m '请委派一个子 Agent 阅读 README.md，概括项目当前实现的能力，再由你总结下一步适合做什么。'
```

模型会根据工具说明决定是否委派。模型提供的子任务说明可以是自然语言；我们没有把子 Agent 绑定到 mock 的演示口令。

## 8. 代码从哪里读

1. `internal/tool/files.go`：文件工具的参数、读取与输出。
2. `internal/tool/subagent.go`：把子任务包装成普通工具，核心就是 Start → Wait → 返回结果。
3. `internal/task/task.go`：保存任务状态、提供 Status/Wait、处理取消。
4. `internal/agent/runtime.go`：组装父子 Runtime，每个父请求建立独立任务管理器，区分工具超时。
5. `cmd/rosetta/main.go`：解析 workspace，打开文件工具目录，组装 Runtime。
6. `internal/provider/mock.go`：离线演示怎么触发同样的工具流程。

本地聊天协议仍为 v3，使用已有的 tool.started/tool.finished 事件，不增加子任务事件流。子任务轮次和当前工具可以通过内部 Status 接口读取，本轮终端不单独展示这些内部进度。
