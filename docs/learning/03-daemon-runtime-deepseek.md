# 从一条真实问答理解 Daemon 与 Runtime

> 2026-10-22：本文描述上一版本的单次文本调用。当前代码已加入结构化消息和工具循环，运行与源码阅读请先看 [第四课](04-react-loop.md)。本文用于回顾基础通信链路。

日期：2026-10-20。当前范围：一个本机服务、一次模型请求、流式显示结果。接下来逐步增加历史消息与工具能力。

## 1. 这次为什么保留两个大模块

用户希望先看到程序跑通，同时保留参考项目中的 Daemon / Runtime 边界。我们直接使用已有骨架，增加一个真实模型适配器。所有模块都在同一个 Go module 中，不是微服务。

CLI 与 Daemon 是同一个可执行文件启动出的不同进程。Runtime 是 Daemon 进程内的 Go 对象，不是第三个进程。

```text
终端 B：rosetta chat                    终端 A：rosetta serve
┌────────── CLI 进程 ─────────┐        ┌─────── Daemon 进程 ─────────────┐
│ 读命令参数                 │        │ Daemon：接收、校验、准入请求   │
│ 本地客户端                 │ ────→  │   ↓                            │
│ 逐行接收结果、打印         │ ←────  │ Runtime：组织本次问答          │
└────────────────────────────┘ Socket │   ↓                            │
                                      │ DeepSeek Provider：发送 HTTP    │
                                      └──────────────┬──────────────────┘
                                                     ↓ HTTPS
                                                DeepSeek API
```

在本版中，serve 仍在前台运行，便于看到日志和 Ctrl+C 停止。后台进程托管稍后再做。

## 2. 跑起来

在项目根目录构建：

```sh
make build
```

终端 A（zsh）：

```zsh
read -rs "DEEPSEEK_API_KEY?DeepSeek API Key: "
printf '\n'
export DEEPSEEK_API_KEY
./bin/rosetta serve --provider deepseek
```

read 会等待你输入密钥，输入时不显示字符。不要把真实密钥写进源码、教程或共享截图。程序只读环境变量，不会自动加载 .env。服务启动后，在另一个终端修改变量，不会改变该服务已经取得的值。

终端 B：

```sh
./bin/rosetta status
./bin/rosetta chat -m "请用一句话介绍你自己。"
./bin/rosetta chat --json -m "请只回复你好。"
```

status 应显示 provider 为 deepseek。ready 表示本地服务已开始接收请求，不表示上游 Key、额度和模型已验证。

本版默认模型取自本次核对的接口参考，为 deepseek-flash。Quick Start 的模型名称示例与接口参考可能更新不同步，因此保留显式 `--model` 参数；不要把别名当作永久不变的版本。[DeepSeek 接口参考](https://api-docs.deepseek.com/zh-cn/api/create-chat-completion)

可以明确设置：

```sh
./bin/rosetta serve --provider deepseek --model deepseek-flash --max-tokens 512
```

先停止同一 state 目录里的旧服务再运行。为了控制学习时的输出量，本版限制 max-tokens 在 1–4096；这是项目自己的限制。它不是整个账号或任务的费用上限。

使用模拟模式时只需 `./bin/rosetta serve --provider mock`，不调用外部 API。

## 3. 三个模块各做什么

| 模块 | 输入 | 工作 | 输出 |
|---|---|---|---|
| Daemon | CLI 发来的 message 与 session_id | 校验输入、检查是否已有执行、设置期限、向客户端传事件 | HTTP 上的逐行 JSON |
| Runtime | 本次消息与取消上下文 | 发 started，调用 Provider，把文本变成事件，成功后发 completed | 内部 Event 或 error |
| DeepSeek Provider | 消息、模型设置、密钥 | 构造请求、读取 SSE、提取回答文本、检查结束原因 | 文本片段或 error |

一个简单的判断方法：修改 DeepSeek 的网络请求格式，只应该主要影响 Provider；改变同会话并发规则，主要影响 Daemon；增加模型与工具之间的循环，则会扩展 Runtime。

本次的 Stream 接口只支持文本，不预先宣称覆盖全部 Agent 能力。下一阶段加入消息历史时会改为传递结构化消息列表；工具调用阶段再增加工具请求与结果类型。

## 4. 只沿着这条调用链读

1. `cmd/rosetta/main.go` 的 `run`：读取 serve 参数和环境变量，创建 DeepSeek，再把它传给 Runtime，把 Runtime 传给 Daemon。
2. `internal/daemon/server.go` 的 `chat`：收到本地请求后，调用 `s.runtime.Run(...)`。
3. `internal/agent/runtime.go` 的 `Run`：调用 `r.provider.Stream(...)`。
4. `internal/provider/deepseek.go` 的 `Stream`：真正发送 HTTPS 请求。
5. 同文件的 `readDeepSeekStream`：解析返回片段，调用 emit。
6. 文本事件经 Daemon 返回 CLI，由 `internal/client/client.go` 读取，再由入口打印。

先不研究文件锁的全部细节，也不要求一口气读完测试。先找到“请求在哪里发出”和“文本在哪里打印”，再沿着中间的函数回溯。

## 5. Provider 为什么能替换

Runtime 依赖的是一个接口：

```go
type TextProvider interface {
    Name() string
    Stream(ctx context.Context, message string, emit func(string) error) error
}
```

Mock 和 DeepSeek 都具有这些方法，所以 Runtime 可以使用任意一种。入口负责决定本次创建哪个对象；Runtime 无需判断“当前是不是 DeepSeek”。

emit 是一个回调函数。模型返回一段文本时，Provider 调用它；Runtime 把文本包装成内部事件；Daemon 把内部事件编码成客户端理解的 JSON。

回调返回 error 的原因是：客户端已经断开或写入失败时，上游应能停止，不能继续无意义地生成。取消则通过 context 传递到 HTTP 请求。

## 6. 两段不同的流

DeepSeek 到本地 Provider 使用 SSE；Daemon 到 CLI 使用项目自己的 NDJSON 事件流。它们不是同一个协议，Provider 与 Daemon 各自负责一段转换。

```text
DeepSeek SSE 数据
    → 提取 choices 中的文本增量
    → Runtime 的 text.delta 事件
    → 本地 NDJSON 帧
    → CLI 的终端文本
```

DeepSeek SSE 以 `[DONE]` 结束。本版还检查 finish_reason，要求正常 stop；达到输出上限或中途断开会报告失败，已显示的文本可能只是部分结果。[DeepSeek 流式接口](https://api-docs.deepseek.com/zh-cn/api/create-chat-completion)

本版显式使用 `thinking.type = disabled`，让第一次学习集中在最终回答的流式过程。思考模式的请求与内容处理留到以后。[DeepSeek 思考模式](https://api-docs.deepseek.com/guides/thinking_mode)

我们暂时使用 Go 标准库 net/http 和 encoding/json，便于直接看到请求体与流的转换。以后协议和工具调用越来越复杂时，可以评估 SDK；当前不需要为了这一条调用链引入完整 Agent 框架。

## 7. 当前行为与限制

- 每次发送且仅发送这一条用户消息。相同 session_id 不会带来历史记忆；它目前只是并发管理键。
- 同 session 同时只能一个 Turn，不同 session 最多四个并发。
- 没有文件读取、命令执行、计划或定时任务；模型说“已经保存”也不代表程序真的操作了文件。
- 不自动重试真实模型请求，避免把不确定失败变成多次收费调用。
- 默认输出上限为 512 token，达到上限会报告不完整；用量展示和费用预算尚未实现。
- 密钥只用于访问固定的官方 HTTPS API；本版不接受自定义 API 地址，并拒绝跟随重定向。
- Ctrl+C 取消客户端请求，服务端会取消对应上游 HTTP 请求。取消不能保证供应商端尚未产生的费用为零。

## 8. 遇到错误如何判断

| 现象或错误码 | 含义与下一步 |
|---|---|
| 启动时要求 DEEPSEEK_API_KEY | 启动服务的进程环境里没有密钥 |
| daemon lock 冲突 | 同一 state 目录已经有服务；先停止旧实例 |
| status 显示 mock | 当前连接的是模拟服务，需要停止后以 deepseek 启动 |
| provider_http_401 | 上游拒绝鉴权，检查密钥是否有效 |
| provider_http_402 | 上游返回付款/余额相关状态，去账号控制台核对 |
| provider_http_400 | 上游认为参数无效，例如模型 ID 不受支持；应按官方文档核对 |
| provider_http_429 | 上游拒绝当前速率或额度；本版不会自动重试 |
| provider_connection_error | 上游连接或请求失败，检查网络；输出不包含原始敏感错误 |
| provider_output_limit | 达到输出限制，收到的是部分结果 |
| provider_incomplete_response | 流没有完整的正常结束证据，不当作成功 |

错误码保留 HTTP 状态或明确类别，不转发可能包含敏感信息的原始上游响应体。代价是故障细节较少；后续可以增加严格脱敏的诊断。

## 9. 本次验证与下一步

离线测试覆盖请求参数、中文文本、SSE 注释与多行数据、截断、输出上限、HTTP 错误脱敏、取消传播、重定向拒绝，以及已有的 Runtime 和 Daemon 检查。通过 `make check` 运行，不调用真实模型。

真实模型验证单独进行，使用临时 state 目录和最多 64 个输出 token 的简单消息；密钥没有保存到项目文件。具体本次结果见 `docs/development-plan.md` 的最新记录。

下一步建议只增加一件事：在 Runtime 侧组织消息历史，让“我叫小明”之后的“我叫什么”能够正确关联。先在内存里实现，再决定怎样保存和切换 Session；届时 Session 就有了实际可观察的作用。
