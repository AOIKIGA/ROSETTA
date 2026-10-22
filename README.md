# ROSETTA

用 Go 开发的个人本地 Agent 项目，同时记录架构决策和学习过程。参考 Agentix 的职责划分，按个人维护成本重新设计。

> 当前阶段（2026-10-22）：具名 Agent 与后台 daemon。先读 [从前台 Server 到后台 Daemon](docs/learning/05-agent-daemon-lifecycle.md)，ReAct 循环见 [第四课](docs/learning/04-react-loop.md)。

支持 DeepSeek 结构化工具调用与离线 mock，内置 `get_time`。每个 Agent 有独立进程、配置、socket 和日志。**现在持久化的是 Agent 配置；尚无用户命名空间、会话数据库、跨请求历史或文件/命令工具。** `--session` 仍只用于该 Agent 内的并发准入。

## 编译与后台运行

目标平台：macOS、Linux。Go 版本为 1.27.2；支持自动工具链选择的 Go 命令会按 `go.mod` 下载所需版本，不替换全局安装。当前仅用标准库。

```sh
make build
./bin/rosetta start personal
./bin/rosetta status personal
./bin/rosetta chat personal -m "你好，ROSETTA"
./bin/rosetta chat personal --json -m /time
./bin/rosetta stop personal
```

Agent 名称放在选项之前，允许 1–32 位 ASCII 字母、数字、下划线、短横线。省略时使用 `personal`，因此 `rosetta start` 和 `rosetta chat -m /time` 也可以使用。会话参数默认 `--session main`。

`start` 等待服务真正就绪后退出；此后关闭启动终端不会关闭 daemon。`chat` 中按 Ctrl+C 只取消这次请求，后台服务继续运行。`stop` 拒绝新请求、取消活跃 turn，并等待服务释放进程锁。重复 start 返回已运行，重复 stop 返回已停止。

需要前台调试时使用 `./bin/rosetta serve personal`，Ctrl+C 停止。`serve` 和 `start` 共享同一 Agent 的锁，不能重复启动。

多个 Agent 可以独立运行：

```sh
./bin/rosetta start work
./bin/rosetta chat work --session main -m /time
./bin/rosetta stop work
```

普通 mock 消息保留模拟回复；`/time` 是离线演示触发词，会走“请求工具 → 执行真实本地 get_time → 交回 Mock → 输出回答”两轮流程。Mock 不调用 AI。进度写 stderr、回答写 stdout；`--json` 将全部事件写为 NDJSON。

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
├── run/
│   ├── daemon.sock     # 本地 HTTP 通信
│   ├── daemon.lock     # 生命周期独占锁，停止后保留文件
│   └── instance.json   # 当前 PID、实例 ID、启动时间；停止时移除
└── logs/
    └── daemon.log      # 后台诊断日志，停止后保留
```

每条命令可传同一个 `--state-dir /短路径/私有目录`，该参数始终代表最外层根目录。目录要求私有权限，配置、锁、实例与日志文件使用 0600。拒绝 Agent 路径穿越、管理目录/文件的符号链接及过长 socket 路径。status/stop 不会创建不存在的 Agent。

状态查询结合真实进程锁与 socket；不把 PID 文件当作存活依据。stop 通过 socket 指定实例 ID，不会根据过期 PID 杀进程。服务无响应时明确报错，当前没有 force-stop。崩溃后下一次 start 可以清理残留 socket，但不会自动重启。也未实现开机自启、日志轮转或后台 turn 恢复；日志可在停止服务后手动归档。

每个 Agent 最多同时运行 4 个 turn，同一 session 一次只能运行一个。每个 turn 最多 6 次模型调用、8 次工具调用，每轮最多 4 个工具；整体超时 2 分钟，单工具 3 秒，工具结果最多 8 KiB，模型输入 JSON 最多 64 KiB。

本地协议升级为 v3。**升级前如有旧版 `<state-dir>/daemon.sock` 服务，请在旧 serve 终端按 Ctrl+C 停止。** 新版使用 Agent 子目录，不接管或删除旧服务，也不迁移旧布局。旧版没有会话数据库，无消息历史可迁移。status 的 ready 只说明本地服务就绪，不验证远端鉴权和余额。

## 阅读与验证

- [后台 Daemon 与 Agent 目录](docs/learning/05-agent-daemon-lifecycle.md)：本轮修改、流程和源码阅读顺序。
- [ReAct 工具循环](docs/learning/04-react-loop.md)：模型与工具调用。
- [开发路线](docs/development-plan.md)：当前进度与下一阶段。
- [本地协议](docs/design/local-protocol.md)：身份、事件和关闭接口。
- [参考材料](docs/references/README.md)：原文与完整性检查。

```sh
make check
```

执行 vet 与 race tests；进程集成测试会构建真实 CLI，在临时目录启动 mock daemon，验证终端进程组隔离、聊天取消、双 Agent、并发 start、缺密钥启动失败、停止和崩溃恢复，最后清理临时进程与目录。不调用真实模型。

```text
cmd/rosetta/          CLI 参数、配置覆盖和组件组装
internal/state/      Agent 名称、私有目录和配置文件
internal/daemon/     Daemon、HTTP 准入、后台进程和 socket 生命周期
internal/client/     CLI 侧 HTTP over Unix socket 客户端
internal/protocol/   本地请求、事件、健康状态和关闭请求
internal/agent/      单个 turn 内的 ReAct 循环
internal/llm/        供应商中立的模型类型
internal/provider/   DeepSeek 适配器与 Mock
internal/tool/       工具注册表与 get_time
```

`module rosetta` 为当前模块名；仓库为 AOIKIGA/ROSETTA，尚未配置自动发布。
