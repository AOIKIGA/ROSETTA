# 从单次回答到 ReAct 工具循环

日期：2026-10-22。本文对应已经落地的第一版工具循环。

## 1. 先跑起来

项目根目录执行：

```sh
make build
```

终端 A：

```sh
./bin/rosetta serve
```

终端 B：

```sh
./bin/rosetta status
./bin/rosetta chat -m /time
./bin/rosetta chat --json -m /time
```

如果旧版服务还在运行，先在它的终端 Ctrl+C 再启动。当前本地协议版本是 2；旧版的 1 不支持工具事件。

`/time` 是 Mock 的固定演示消息。它不请求真实模型，但会真实执行本机的 get_time 工具。普通消息仍然返回模拟回声：

```sh
./bin/rosetta chat -m "你好"
```

工具演示的进度类似：

```text
[第 1 轮模型调用]
[执行工具 get_time]
[工具 get_time：ok (0 ms)]
[第 2 轮模型调用]
[模拟模型：未调用 AI；已执行真实本地工具] Asia/Shanghai 当前时间：实际查询结果
```

进度写 stderr，回答写 stdout。可以用 `chat -m /time > answer.txt` 只保存回答。`--json` 则把全部事件写到 stdout，适合观察或接其他程序。

## 2. 一次 HTTP 请求为什么能做很多步

CLI 仍然只发送一个 POST /v1/chat。Daemon 调用一次 Runtime.Run，并保持原来的响应流。

```text
CLI 发来请求
  → Daemon 校验并准入
  → Runtime.Run
      → Provider 第 1 轮：请求 get_time
      → 本地工具执行
      → 将调用和结果追加到 messages
      → Provider 第 2 轮：根据真实结果回答
  → turn.completed
  → CLI 退出
```

模型和工具的多步往返发生在服务进程内部；客户端持续接收同一个 HTTP 响应。所有步骤共享同一个 run_id，事件 seq 在整个 turn 内递增。

## 3. 从输入输出读新的 Provider

先读 `internal/llm/types.go`。

旧接口的输入是一条 string，输出只有文本回调。新接口：

```go
Stream(ctx context.Context, req Request, onText func(string) error) (Response, error)
```

输入 Request：

- Messages：截至当前轮次，模型应知道的消息。
- Tools：允许使用的工具名称、描述和参数 schema。

输出有两条路径：

- onText：同步回调，收到文字就展示，不必等全轮结束。
- Response：完整 assistant 消息，包含最终收集的文本和工具调用。

Response 里的文本必须等于通过 onText 发出的全部文本。Runtime 会检查这个约定，防止显示内容和下一轮上下文不一致。

ToolCall.Arguments 保留为 string。模型返回错误 JSON 时，仍能完整保留 assistant 消息，并给对应调用返回参数错误；如果过早解码成对象，原始的错误调用就丢失了。

`llm` 表达模型交互数据，`protocol` 表达本地显示事件。两者分别服务于“模型接下来知道什么”和“用户现在看见什么”。

## 4. Runtime.Run 中的状态

再读 `internal/agent/runtime.go`。

Runtime 对象只持有共享组件：Provider、工具注册表和固定工具超时。每次 Run 自己创建：

```text
messages     这次请求的消息列表
seenCalls    已出现的工具调用 ID
executions   已执行的工具调用数
round        本次模型调用轮次
```

它们都是局部变量，所以同时运行的不同请求互不混用。

每轮先检查输入大小并发送 model.round.started，然后调用 Provider。Provider 完整成功返回后，Runtime 才决定是完成还是执行工具。

有多个工具调用时，先检查整批调用的数量、ID 和剩余预算，全部通过后才开始执行；工具按返回顺序串行执行。下一轮模型调用必须等这一批工具都返回。

工具阶段的核心流程：

```text
emit(tool.started)
  → Registry.Execute(toolCtx, call)
  → messages 追加 tool 结果
  → emit(tool.finished)
```

整个过程是普通函数调用，没有额外的工具工作进程或任务队列。

## 5. messages 如何增长

第一轮：

```text
system: 助手角色和工具使用约定
user: /time
```

第一轮结束并执行工具后：

```text
system: ...
user: /time
assistant: tool_calls=[{id: mock_time_1, name: get_time, arguments: ...}]
tool: tool_call_id=mock_time_1, content=工具结果
```

第二轮收到这份完整记录，再生成回答。

必须保留 assistant 的工具调用，并用 tool_call_id 配对结果。仅把“现在几点”拼成另一条 user 消息会丢失这个对应关系。

一个 turn 可以包含多个模型轮次；同一个 session 的下一个 turn 仍会重新创建 messages。跨请求记忆和 SQLite 持久化尚未加入。

## 6. get_time 是怎样的 Go 工具

读 `internal/tool/time.go` 和 `registry.go`。

Time.Spec() 返回工具说明；Time.Execute() 执行查询。工具接受一个 JSON 对象：

```json
{"timezone":"Asia/Shanghai"}
```

也可以传 `{}`，默认 Asia/Shanghai。时区需要是有效的 IANA 名称；未知字段、非字符串、null、路径和多个 JSON 值会返回错误结果。

工具先做参数校验，再查询本机系统时间，并格式化为 RFC3339。二进制内包含标准库时区数据，避免依赖目标机器是否安装时区文件。它使用本机时钟，不会向网络时间服务校时。

Registry 根据工具名称找实现。模型请求不存在的工具时，只返回 unknown_tool，不会调用任意系统函数。工具的意外内部错误统一转换为安全错误；业务错误则由工具主动返回 Result。

工具结果的 content 是供模型读取的数据；界面默认只展示工具名、状态和耗时。完整工具内容不会自动写进日志。

## 7. DeepSeek 适配器多做了什么

最后读 `internal/provider/deepseek.go` 和 `deepseek_stream.go`。

适配器把 llm.Message 转换成 API 消息，把工具说明转换成 function schema。返回的 SSE 中，工具参数可能被分成多个片段，多个调用还可能交错。

例如：

```text
调用 index=0：arguments 片段 {"time
调用 index=1：arguments 片段 {
调用 index=0：arguments 片段 zone":"UTC"}
调用 index=1：arguments 片段 }
```

适配器按 index 分开聚合。只有收到合法的结束原因和 [DONE] 后，才返回完整 ToolCalls。断流时返回空 Response 和错误，Runtime 无法拿到可以执行的半截工具请求。

流正常结束分两种：stop 表示有效文本回答；tool_calls 表示请求工具，可以完全没有文本。输出达到上限、未知结束原因和身份不完整的调用都不算成功。

## 8. 用真实模型运行

先停掉 Mock。在终端 A 用 zsh 输入密钥：

```zsh
read -rs "DEEPSEEK_API_KEY?DeepSeek API Key: "
printf '\n'
export DEEPSEEK_API_KEY
./bin/rosetta serve --provider deepseek
```

终端 B：

```sh
./bin/rosetta chat -m "请调用工具查询上海和 UTC 当前时间，并告诉我时差"
```

DeepSeek 根据自然语言和工具定义选择动作。Mock 的 `/time` 是确定性的学习入口，不是模型端的命令协议。真实模型也可能直接回答；验证工具执行要看 tool.started / tool.finished，不能只看它是否声称“已经查询”。

## 9. 停止条件与错误

| 条件 | 行为 |
|---|---|
| 有有效回答且无工具调用 | turn.completed |
| 未知工具或参数错误 | 生成错误工具结果，模型可修正 |
| 6 次模型调用仍请求工具 | round_limit，不执行最后一批工具 |
| 工具总数超过 8 或单轮超过 4 | tool_call_limit，整批不执行 |
| 输入超过 64 KiB | input_limit |
| 单工具结果超过 8 KiB | 截断并在结果中标记 truncated |
| 模型正文与工具数据超过响应限制 | provider_response_limit |
| 单工具超过 3 秒或 turn 超过 2 分钟 | 终止 turn，返回 deadline_exceeded |
| Ctrl+C、客户端断开或输出写入失败 | 停止后续工具和模型调用 |

context 的期限需要执行代码配合，不能强行杀死任意 Go 函数。当前工具有界且检查 context；增加新工具时也要遵守这个约定。

每次模型调用的 max_tokens 仍默认 512；多轮会累计模型用量。输入字节限制不等于精确 token 计费或上下文管理。

## 10. 建议阅读顺序与练习

1. `llm/types.go`：先确定输入输出有哪些数据。
2. `provider/mock.go`：找出为什么 /time 第一次返回调用、第二次返回回答。
3. `tool/time.go`：看一个请求如何进入普通 Go 函数。
4. `agent/runtime.go`：把以上三部分连成循环。
5. `protocol/types.go` 和 `cmd/rosetta/main.go`：看内部动作如何显示到终端。
6. `provider/deepseek_stream.go`：最后理解真正的网络流聚合。

可以先回答三个问题：为什么工具调用轮不一定有文字？为什么最后一轮还请求工具时不执行？为什么 Runtime 上没有 messages 字段？答案分别对应：结构化动作、执行预算和并发隔离。

`make check` 执行 vet 和 race 测试；测试中的 DeepSeek 是本地 HTTP 测试服务，不使用真实凭证或产生模型费用。
