# ROSETTA

用 Go 开发的个人本地 Agent 项目，同时记录架构决策和学习过程。参考 Agentix 的职责划分，按个人维护成本重新设计。

> 当前阶段（2026-10-26）：B，SQLite、用户与会话、历史查询。先读 [存储与身份](docs/learning/07-storage-and-identity.md)，后台进程基础见 [第五课](docs/learning/05-agent-daemon-lifecycle.md)。

支持 DeepSeek 结构化工具调用与离线 mock，内置 `get_time`。每个 Agent 有独立进程、配置、socket、日志和 SQLite 数据库。会话按 Agent → 用户 → 会话名称划分，保存消息、工具调用及运行状态。**当前能查询历史，但还不会把历史送入下一次模型调用；跨轮记忆属于阶段 C。**

文件工具与子 Agent 工作已单独保存在 `codex/file-tools-subagents`（`f99b7da`）；本分支 `codex/storage-identity` 从阶段 A 开始实现 B。

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

## DeepSeek 与配置

DeepSeek 从**启动服务时**的 `DEEPSEEK_API_KEY` 环境变量读取密钥，不自动读取 `.env`。在 zsh 中可以这样输入，输入不回显，也不会将密钥写入命令历史：

```zsh
read -rs "DEEPSEEK_API_KEY?DeepSeek API Key: "
printf '\n'
export DEEPSEEK_API_KEY
./bin/rosetta stop personal
./bin/rosetta start personal --provider deepseek
./bin/rosetta chat personal -m "请调用工具查询上海和 UTC 当前时间，并告诉我时差"
```

后台进程继承启动环境。其他终端执行 chat/status/stop 不需要密钥；下次启动 DeepSeek 仍需提供环境变量。默认模型 `deepseek-flash`，可用 `--model` 指定；`--max-tokens` 默认 512，范围 1–4096，限制每次模型调用的输出。沿用非思考模式，不自动重试模型请求。

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
└── logs/
    └── daemon.log      # 后台诊断日志，停止后保留
```

每条命令可传同一个 `--state-dir /短路径/私有目录`，该参数始终代表最外层根目录。目录要求私有权限，配置、数据库、锁、实例与日志文件使用 0600。数据库及辅助文件也拒绝符号链接；备份时先停止 Agent，再复制数据库与配置。拒绝 Agent 路径穿越、管理目录/文件的符号链接及过长 socket 路径。status/stop 不会创建不存在的 Agent。

状态查询结合真实进程锁与 socket；不把 PID 文件当作存活依据。stop 通过 socket 指定实例 ID，不会根据过期 PID 杀进程。服务无响应时明确报错，当前没有 force-stop。崩溃后下一次 start 可以清理残留 socket，但不会自动重启。也未实现开机自启、日志轮转或后台 turn 恢复；日志可在停止服务后手动归档。

每个 Agent 最多同时运行 4 个 turn，同一用户下的同一会话一次只能运行一个；不同用户的同名会话可同时运行。每个 turn 最多 6 次模型调用、8 次工具调用，每轮最多 4 个工具；整体超时 2 分钟，单工具 3 秒，工具结果最多 8 KiB，模型输入 JSON 最多 64 KiB。

本地协议升级为 v4。升级阶段 A 的 v3 后台进程时，先用新版 `rosetta stop [agent]` 关闭，再 start；仅关闭流程兼容 v3，聊天和查询要求 v4。更早的根目录 `<state-dir>/daemon.sock` 服务仍需回到旧 serve 终端停止，新版不接管旧目录。阶段 A 没有持久化消息，无旧聊天记录可迁移。status 的 ready 表示本地服务及数据库初始化完成，不验证远端鉴权和余额。

最终回答与 completed 状态在同一事务中保存，成功后才发送完成事件。取消或失败保留已经完整保存的消息；异常退出遗留的 running 在下次启动时标记为 interrupted，不会自动重做工具。流式文字片段和事件流不持久化。

## 阅读与验证

- [存储与身份](docs/learning/07-storage-and-identity.md)：本轮实现、数据库关系和可运行练习。
- [后台 Daemon 与 Agent 目录](docs/learning/05-agent-daemon-lifecycle.md)：本轮修改、流程和源码阅读顺序。
- [ReAct 工具循环](docs/learning/04-react-loop.md)：模型与工具调用。
- [开发路线](docs/development-plan.md)：当前进度与下一阶段。
- [本地协议](docs/design/local-protocol.md)：身份、事件和关闭接口。
- [参考材料](docs/references/README.md)：原文与完整性检查。

```sh
make check
```

执行 vet 与 race tests；进程集成测试会构建真实 CLI，在临时目录启动 mock daemon，验证终端进程组隔离、聊天取消、双 Agent、并发 start、缺密钥启动失败、停止和崩溃恢复，以及用户/会话/Agent 隔离、重启后历史保留，最后清理临时进程与目录。存储测试还验证完成提交、取消记录、中断标记和分页；不调用真实模型。

```text
cmd/rosetta/          CLI 参数、配置覆盖和组件组装
internal/state/      Agent 名称、私有目录和配置文件
internal/daemon/     Daemon、HTTP 准入、后台进程和 socket 生命周期
internal/client/     CLI 侧 HTTP over Unix socket 客户端
internal/protocol/   本地请求、事件、健康状态和关闭请求
internal/identity/   用户与会话名称、默认值
internal/store/      SQLite 初始化、事务、历史查询
internal/conversation/ 连接消息记录与 Runtime，协调完成提交
internal/agent/      单个 turn 内的 ReAct 循环
internal/llm/        供应商中立的模型类型
internal/provider/   DeepSeek 适配器与 Mock
internal/tool/       工具注册表与 get_time
```

`module rosetta` 为当前模块名；仓库为 AOIKIGA/ROSETTA，尚未配置自动发布。
