# ROSETTA

用 Go 开发的个人本地 Agent 项目，同时记录架构决策和学习过程。参考 Agentix 的职责划分，按个人维护成本重新设计。

> 当前版本（2026-10-31）：main 已整合文件工具、阻塞式子 Agent、B 的存储和 C 的会话记忆。分支关系和这次整合见 [合并分支](docs/learning/09-merging-branches.md)。

支持 DeepSeek 结构化工具调用与离线 mock，内置 `get_time`、`list_files`、`read_files` 和 `subagent`。每个 Agent 有独立进程、配置、socket、日志和 SQLite 数据库。会话按 Agent → 用户 → 会话名称划分，保存消息、工具调用及运行状态。**后续请求会加载同一用户、同一会话近期已完成的对话，重启后也有效。** 历史受条数和字节预算限制，目前没有摘要或长期记忆。

main 同时保留 `.env` / `--env-file` 凭证读取。父会话保存 subagent 的调用与最终结果；子任务仍只接收显式任务说明，不自动继承父历史，也不单独持久化子任务过程。

## 编译与后台运行

目标平台：macOS、Linux。Go 版本为 1.27.2；支持自动工具链选择的 Go 命令会按 `go.mod` 下载所需版本，不替换全局安装。数据库使用 `database/sql` 与纯 Go 驱动 `modernc.org/sqlite v1.61.0`，普通构建不依赖 C 编译器；首次构建会下载模块依赖。

```sh
make build
./bin/rosetta start personal
./bin/rosetta status personal
./bin/rosetta chat personal -m "你好，ROSETTA"
./bin/rosetta chat personal --json -m /time
./bin/rosetta stop personal
```

Agent 名称放在选项之前，允许 1–32 位 ASCII 字母、数字、下划线、短横线。省略时使用 `personal`，因此 `rosetta start` 和 `rosetta chat -m /time` 也可以使用。用户和会话参数默认 `--user defaultuser --session main`。两种名称允许 1–64 位 ASCII 字母、数字、下划线、短横线，区分大小写。

`start` 等待服务真正就绪后退出；此后关闭启动终端不会关闭 daemon。`chat` 中按 Ctrl+C 只取消这次请求，后台服务继续运行。`stop` 拒绝新请求、取消活跃 turn，并等待服务释放进程锁。重复 start 返回已运行，重复 stop 返回已停止。

需要前台调试时使用 `./bin/rosetta serve personal`，Ctrl+C 停止。`serve` 和 `start` 共享同一 Agent 的锁，不能重复启动。

多个 Agent 可以独立运行：

```sh
./bin/rosetta start work
./bin/rosetta chat work --session main -m /time
./bin/rosetta stop work
```

普通 mock 消息保留模拟回复；`/time` 是离线演示触发词，会走“请求工具 → 执行真实本地 get_time → 交回 Mock → 输出回答”两轮流程。Mock 不调用 AI。进度写 stderr、回答写 stdout；`--json` 将全部事件写为 NDJSON。

## 文件工具与子 Agent

文件工具默认读取 `<state-dir>/agents/<agent>/workspace/`，不会默认读取整个 state dir。也可以指定已有目录：

```sh
./bin/rosetta stop personal
./bin/rosetta start personal --provider mock --workspace /Users/aoki/Documents/ROSETTA
./bin/rosetta chat personal -m /ls
./bin/rosetta chat personal -m '/read go.mod'
./bin/rosetta chat personal -m '/delegate /read go.mod'
```

以上是 mock 离线演示，文件读取和子 Runtime 实际执行，但不会调用 AI。DeepSeek 模式用自然语言提出任务，模型自行选择工具。文件内容作为上下文交给模型；请选择希望助手读取的目录。

`--workspace` 仅覆盖本次运行。要保存默认值，可在 agent.json 中增加绝对路径字段 `workspace`；旧配置不必修改，仍使用 Agent 自己的 workspace。显式相对路径按启动终端所在目录解析，再传给后台进程。

read_files 最多读取四个 UTF-8 普通文件，每个文件最多 1 MiB，支持指定起始行和行数。目录越界、外部符号链接、特殊文件会被拒绝；输出超过限制时标注截断。list_files 不递归，最多 100 项，暂不支持翻页。工具不会写文件或执行 shell。

subagent 工具会启动子任务并等待完成，再把结果交回父模型。子 Agent 只有时间和文件工具，不能继续委派；它没有父会话历史，也不会把中间文字混入父回答。内部已实现 Start/Status/Wait，**尚未开放子任务 CLI status/wait 命令或异步后台任务**；任务记录随父 turn 结束释放。

## 用户、会话与历史

```sh
./bin/rosetta chat personal --user alice --session main -m "Alice 的消息"
./bin/rosetta chat personal --user bob --session main -m "Bob 的消息"
./bin/rosetta chat personal --user alice --session learning -m /time
./bin/rosetta sessions list personal --user alice
./bin/rosetta sessions show personal --user alice --session main
./bin/rosetta sessions show personal --user bob --session main --json
```

第一次聊天时自动创建用户和会话；查询不会创建。Alice 的 main、Bob 的 main 和 Alice 的 learning 各有独立历史。停止并重新启动 Agent 后记录仍在。这里的 user 是本地数据归属标签，不是登录或权限系统：拥有该 OS 账号和 socket 访问权的程序可以选择任何 user。

历史查询通过 daemon 执行，需要先启动对应 Agent。`sessions list` 列出所选用户的会话；`sessions show` 按消息写入顺序展示用户输入、完整模型消息、工具参数与结果、所属运行的状态。默认最多 20 条，可用 `--limit 1..100` 调整；如果返回 `next_after` 或“下一页”，保留原用户/会话参数并添加 `--after CURSOR`。list 的游标是会话 ID，show 的游标是消息 ID，不能混用。单页历史最大 2 MiB，因此实际条数也可能受字节上限限制。

`--json` 对 chat 输出 NDJSON 事件流，对历史查询输出一个 JSON 对象。所有命令都支持 `--state-dir`。本次未提供独立创建、重命名或删除用户/会话的命令。

## 会话记忆（阶段 C）

重新编译后，先停止旧 daemon 再启动，正在运行的进程不会自动换成新代码。下面用 mock 验证整个存储与上下文链路：

```sh
./bin/rosetta stop personal
./bin/rosetta start personal --provider mock
./bin/rosetta chat personal --user alice --session memory -m "我喜欢 Go"
./bin/rosetta stop personal
./bin/rosetta start personal --provider mock
./bin/rosetta chat personal --user alice --session memory -m /recall
```

最后会显示“上一条用户消息：我喜欢 Go”。`/recall` 是 mock 演示口令，直接检查传入的上下文，不调用 AI，也不模拟自然语言理解。最多显示上一条消息的前 256 个字符；该命令本身也会成为新的聊天记录。

历史加载策略：从最近的成功 turn 往前取，最多 20 次、累计消息 JSON 最多 32 KiB，再按时间顺序交给模型。遇到放不下的 turn 就停止向更早处查找；不会只保留它的部分消息。failed/canceled/interrupted/running 不参与模型上下文，但仍可用 sessions show 查询。切换 provider/model 后，同一会话仍沿用这些历史；需要空白对话时换一个 session 名称。

每轮模型调用都会检查系统提示、历史、当前请求、工具定义和本次工具结果的总大小（最多 64 KiB），DeepSeek 还会计入实际 API 请求的包装字段。超出时逐次移除最旧的完整历史 turn；本次输入和本次工具结果不会被截断，单靠它们就超限时返回 input_limit。裁剪只作用于模型输入，不删除数据库记录。这里是 JSON 字节限制，不是模型 token 计数。

## DeepSeek 与配置

DeepSeek 在 start/serve 启动时读取密钥，优先级为：**已有 `DEEPSEEK_API_KEY` 环境变量 → 启动目录的 `.env`**。已设置为空的环境变量也优先，会导致缺少密钥错误；要启用文件中的值，可先执行 `unset DEEPSEEK_API_KEY`。

本地 `.env` 格式如下（不要把真实密钥提交到 Git）：

```dotenv
DEEPSEEK_API_KEY=你的本地密钥
```

`.gitignore` 已忽略 `.env` 和 `.env.*`，仅允许无真实密钥的 `.env.example` 模板。程序只读取 DEEPSEEK_API_KEY，支持空行、注释、可选 export 和单/双引号，不执行 shell 命令、不展开变量、不支持多行值。重复赋值取最后一条；文件最多 64 KiB。文件不存在时仍可只用环境变量。

```sh
./bin/rosetta stop personal
./bin/rosetta start personal --provider deepseek
```

若不在项目目录启动，可显式使用 `--env-file /绝对路径/.env`；`--env-file ''` 禁用文件读取。start 将文件路径转换成绝对路径交给子进程，daemon 改变工作目录不会影响读取位置。修改密钥后需要 stop/start，重复 start 不重载。mock 模式不读取凭证文件。

后台进程继承启动环境；文件中的密钥只交给模型适配器，不导入整个进程环境。其他终端执行 chat/status/stop 不需要密钥。默认模型 `deepseek-flash`，可用 `--model` 指定；`--max-tokens` 默认 512，范围 1–4096，限制每次模型调用的输出。沿用非思考模式，不自动重试模型请求。

首次 start/serve 创建默认配置 `~/.rosetta/agents/personal/agent.json`：

```json
{
  "version": 1,
  "agent": "personal",
  "provider": "mock",
  "model": "deepseek-flash",
  "max_tokens": 512
}
```

优先级：程序默认值 → agent.json → 显式启动参数。**启动参数仅影响本次运行，不写回配置**；首次使用 `--provider deepseek` 时，创建的配置仍是默认 mock。希望以后默认使用 DeepSeek，可手动将 agent.json 中的 provider 改为 deepseek，然后重启。已有进程收到重复 start 不会重载配置，命令会提示本次参数未应用。配置和实例文件不保存 API key。

## 状态目录与边界

```text
~/.rosetta/agents/personal/
├── agent.json          # 持久化的非敏感配置
├── state.db            # 用户、会话、运行和完整消息
├── state.db-wal        # SQLite 写入日志（运行时可能存在）
├── state.db-shm        # SQLite 协调文件（运行时可能存在）
├── run/
│   ├── daemon.sock     # 本地 HTTP 通信
│   ├── daemon.lock     # 生命周期独占锁，停止后保留文件
│   └── instance.json   # 当前 PID、实例 ID、启动时间；停止时移除
├── logs/
│   └── daemon.log      # 后台诊断日志，停止后保留
└── workspace/         # 默认只读文件工作目录
```

每条命令可传同一个 `--state-dir /短路径/私有目录`，该参数始终代表最外层根目录。目录要求私有权限，配置、数据库、锁、实例与日志文件使用 0600。数据库及辅助文件也拒绝符号链接；备份时先停止 Agent，再复制数据库与配置。拒绝 Agent 路径穿越、管理目录/文件的符号链接及过长 socket 路径。status/stop 不会创建不存在的 Agent。

状态查询结合真实进程锁与 socket；不把 PID 文件当作存活依据。stop 通过 socket 指定实例 ID，不会根据过期 PID 杀进程。服务无响应时明确报错，当前没有 force-stop。崩溃后下一次 start 可以清理残留 socket，但不会自动重启。也未实现开机自启、日志轮转或后台 turn 恢复；日志可在停止服务后手动归档。

每个 Agent 最多同时运行 4 个 turn，同一用户下的同一会话一次只能运行一个；不同用户的同名会话可同时运行。父 Runtime 最多 6 次模型调用、8 次工具调用，每轮最多 4 个工具；最多两个子任务，每个最多 3 次模型调用、4 次工具调用。普通工具超时 3 秒，subagent 最长 1 分钟，父请求整体仍为 2 分钟。工具结果最多 8 KiB，子任务最终结果最多 4096 字节，模型输入 JSON 最多 64 KiB。

本地协议升级为 v4。升级阶段 A 的 v3 后台进程时，先用新版 `rosetta stop [agent]` 关闭，再 start；仅关闭流程兼容 v3，聊天和查询要求 v4。更早的根目录 `<state-dir>/daemon.sock` 服务仍需回到旧 serve 终端停止，新版不接管旧目录。阶段 A 没有持久化消息，无旧聊天记录可迁移。status 的 ready 表示本地服务及数据库初始化完成，不验证远端鉴权和余额。

最终回答与 completed 状态在同一事务中保存，成功后才发送完成事件。取消或失败保留已经完整保存的消息；异常退出遗留的 running 在下次启动时标记为 interrupted，不会自动重做工具。流式文字片段和事件流不持久化。

## 阅读与验证

- [合并分支](docs/learning/09-merging-branches.md)：本次分支关系、冲突处理与 Git 命令。
- [文件工具与子 Agent](docs/learning/06-file-tools-and-subagents.md)：只读 workspace 和任务委派。
- [让助手记住前面的对话](docs/learning/08-session-context.md)：历史筛选、完整工具消息与上下文预算。
- [存储与身份](docs/learning/07-storage-and-identity.md)：阶段 B 的数据库关系和可运行练习。
- [后台 Daemon 与 Agent 目录](docs/learning/05-agent-daemon-lifecycle.md)：本轮修改、流程和源码阅读顺序。
- [ReAct 工具循环](docs/learning/04-react-loop.md)：模型与工具调用。
- [开发路线](docs/development-plan.md)：当前进度与下一阶段。
- [本地协议](docs/design/local-protocol.md)：身份、事件和关闭接口。
- [参考材料](docs/references/README.md)：原文与完整性检查。

```sh
make check
```

执行 vet 与 race tests；进程集成测试会构建真实 CLI，在临时目录启动 mock daemon，验证终端进程组隔离、聊天取消、双 Agent、并发 start、缺密钥启动失败、停止和崩溃恢复，以及文件读取、子任务委派、用户/会话/Agent 隔离、重启后历史保留，最后清理临时进程与目录。还通过重启后的 /recall 验证模型输入中的历史隔离。测试覆盖失败轮次排除、工具配对、整轮裁剪和实际请求体预算；新增整合测试验证“父历史 → 子任务隔离 → 父结果保存 → 下一次请求加载”，不调用真实模型。

```text
cmd/rosetta/          CLI 参数、配置覆盖和组件组装
internal/state/      Agent 名称、私有目录和配置文件
internal/daemon/     Daemon、HTTP 准入、后台进程和 socket 生命周期
internal/client/     CLI 侧 HTTP over Unix socket 客户端
internal/protocol/   本地请求、事件、健康状态和关闭请求
internal/identity/   用户与会话名称、默认值
internal/store/      SQLite 初始化、事务、历史查询
internal/conversation/ 加载当前会话历史，连接 Runtime 与消息记录、完成提交
internal/agent/      单个 turn 内的 ReAct 循环
internal/llm/        供应商中立的模型类型
internal/provider/   DeepSeek 适配器与 Mock
internal/tool/       工具注册表、时间、只读文件与子 Agent 工具
internal/task/       父 turn 内的子任务创建、状态查询与等待
```

`module rosetta` 为当前模块名；仓库为 AOIKIGA/ROSETTA，尚未配置自动发布。
