# ROSETTA

> 当前学习阶段（2026-10-22）：已实现有限轮次的 ReAct 工具循环。先读 [从单次回答到工具循环](docs/learning/04-react-loop.md)，可用 mock 离线运行完整示例。身份与配置讲义中的多层配置、会话快照仍是后续设计。

用 Go 开发的个人本地 Agent 项目，同时记录架构决策和学习过程。参考 Agentix 的职责划分，按个人维护成本重新设计。

**当前进度：支持 DeepSeek 结构化工具调用与离线 mock，内置 `get_time`。一次用户请求内可多次调用模型和工具；尚无跨请求历史、持久化或文件/命令工具。** `--session` 目前只用于并发准入。

## 运行第一版

目标平台：macOS、Linux。项目最低 Go 版本为 1.27.2；支持自动工具链选择的 Go 命令会按 `go.mod` 下载所需版本，不替换全局安装。当前仅用 Go 标准库。mock 不需要密钥；DeepSeek 从服务进程的 `DEEPSEEK_API_KEY` 环境变量读取密钥，不自动读取 `.env`。

在项目根目录执行：

```sh
make build
```

终端 A（默认 mock，不联网）：

```sh
./bin/rosetta serve
```

终端 B：

```sh
./bin/rosetta status
./bin/rosetta chat --session main -m "你好，ROSETTA"
./bin/rosetta chat --session main --json -m "查看流式事件"
./bin/rosetta chat -m /time
./bin/rosetta chat --json -m /time
```

普通 mock 消息保留模拟回复；`/time` 是固定的离线演示触发词，会走“请求工具 → 执行真实本地 get_time → 将结果交回 Mock → 输出回答”两轮流程。Mock 不调用 AI。进度写 stderr、回答写 stdout；`--json` 将全部事件写为 NDJSON。

要使用真实模型，先停止 mock 服务，在终端 A 使用以下 zsh 命令输入密钥（输入不回显，密钥不写进命令历史），再启动：

```zsh
read -rs "DEEPSEEK_API_KEY?DeepSeek API Key: "
printf '\n'
export DEEPSEEK_API_KEY
./bin/rosetta serve --provider deepseek
```

终端 B 可执行 `./bin/rosetta chat -m "请调用工具查询上海和 UTC 当前时间，并告诉我时差"`。DeepSeek 根据消息和工具定义决定是否调用工具，不依赖 `/time` 触发词。默认模型 `deepseek-flash`，可用 `--model` 显式选择；单次模型调用的输出上限默认 512 token，可通过 `--max-tokens` 在 1–4096 范围调整。多轮调用会累计用量；沿用非思考模式，不自动重试模型请求。

每次用户请求最多 6 次模型调用、8 次工具调用、每轮最多 4 个工具，整体超时 2 分钟。单工具超时 3 秒；工具结果最多 8 KiB，模型输入 JSON 最多 64 KiB。预算耗尽明确报错。一轮内保留 assistant 工具调用和对应结果，但不会记住上一次 chat 的内容。

本地协议已升级为 v2。编译后请在旧服务终端按 Ctrl+C，再启动新的 serve；`status` 会识别协议版本不匹配。

status 的 ready 仅表示本机服务已就绪，不代表远端鉴权或余额已经验证。

在终端 B 按 Ctrl+C 会取消当前请求；在终端 A 按 Ctrl+C 会停止服务并清理 socket。M1 的服务前台运行，暂不提供 `start/stop` 后台进程管理命令。

默认运行目录是 `~/.rosetta`。可以给三个命令都传同一个 `--state-dir /短路径/私有目录`。新目录自动创建为 0700；已有目录需要是当前用户的私有目录。Unix Socket 路径过长时会给出错误。

## 从哪里开始阅读

1. [从单次回答到工具循环](docs/learning/04-react-loop.md)：当前学习入口、运行示例和源码阅读顺序。
2. [第一版 ReAct 设计](docs/design/react-loop.md)：职责、消息配对、预算和错误策略。
3. [Daemon / Runtime 与第一次 DeepSeek 问答](docs/learning/03-daemon-runtime-deepseek.md)：上一版的基础调用链。
4. [开发路线与进度](docs/development-plan.md)：每阶段的实现、知识点和验收。
5. [本地通信协议](docs/design/local-protocol.md)：字段、事件和错误处理约定。
6. [原始参考材料](docs/references/README.md)：原文与完整性检查。

## 检查代码

```sh
make check
```

包含 `go vet` 与带竞争检测的测试。正常请求之外，还验证取消、会话冲突、容量限制、异常流、重复启动和残留 socket 恢复。

## 代码入口

```text
cmd/rosetta/          命令行入口和组件组装
internal/protocol/   CLI 与服务共用的数据约定
internal/client/     CLI 侧的本地 HTTP 客户端
internal/daemon/     请求校验、并发准入、socket 和服务生命周期
internal/agent/      在一个 turn 内推进模型/工具循环
internal/llm/        供应商中立的模型接口和结构化消息
internal/provider/   DeepSeek 模型适配器与离线模拟实现
internal/tool/       工具注册表、参数校验和 get_time
docs/                设计、开发路线、课程和参考材料
```

`module rosetta` 是当前模块名。代码仓库为 AOIKIGA/ROSETTA；尚未配置自动发布流程。
