# ROSETTA

> 当前学习阶段（2026-10-20）：保留 Daemon / Runtime 分层，从一次真实 DeepSeek 问答开始逐步扩展。先读 [这一版如何运行和理解](docs/learning/03-daemon-runtime-deepseek.md)。身份与配置讲义中的多层配置、会话快照仍是后续设计。

用 Go 开发的个人本地 Agent 项目，同时记录架构决策和学习过程。参考 Agentix 的职责划分，按个人维护成本重新设计。

**当前进度：单次真实模型问答，支持 DeepSeek 与离线 mock。没有历史会话存储或文件/命令执行能力。** `--session` 目前只用于并发准入，不代表已经实现上下文记忆。

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
```

mock 输出以 `[模拟模型：未调用 AI]` 开头。`--json` 展示开始、文本片段、完成或失败事件。

要使用真实模型，先停止 mock 服务，在终端 A 使用以下 zsh 命令输入密钥（输入不回显，密钥不写进命令历史），再启动：

```zsh
read -rs "DEEPSEEK_API_KEY?DeepSeek API Key: "
printf '\n'
export DEEPSEEK_API_KEY
./bin/rosetta serve --provider deepseek
```

终端 B 的 status/chat 命令不变。默认模型 `deepseek-flash`，可以用 `--model` 显式选择；默认输出上限 512 token，可通过 `--max-tokens` 在 1–4096 范围调整。本版关闭思考模式、只展示回答文本，不自动重试收费请求。每次聊天只发送这次消息，没有历史上下文。

status 的 ready 仅表示本机服务已就绪，不代表远端鉴权或余额已经验证。

在终端 B 按 Ctrl+C 会取消当前请求；在终端 A 按 Ctrl+C 会停止服务并清理 socket。M1 的服务前台运行，暂不提供 `start/stop` 后台进程管理命令。

默认运行目录是 `~/.rosetta`。可以给三个命令都传同一个 `--state-dir /短路径/私有目录`。新目录自动创建为 0700；已有目录需要是当前用户的私有目录。Unix Socket 路径过长时会给出错误。

## 从哪里开始阅读

1. [Daemon / Runtime 与第一次 DeepSeek 问答](docs/learning/03-daemon-runtime-deepseek.md)：当前学习入口。
2. [架构与技术选型](docs/design/architecture.md)：目标、取舍及关键规则。
3. [开发路线与进度](docs/development-plan.md)：每阶段的实现、知识点和验收。
4. [本地通信协议](docs/design/local-protocol.md)：字段、事件和错误处理约定。
5. [原始参考材料](docs/references/README.md)：原文与完整性检查。

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
internal/agent/      执行一个 turn
internal/provider/   DeepSeek 模型适配器与离线模拟实现
docs/                设计、开发路线、课程和参考材料
```

`module rosetta` 是本地开发模块名；确定远程仓库地址后再统一调整。尚未配置远程仓库或发布流程。
