# 让助手记住前面的对话

日期：2026-10-29。阶段 C，分支 `codex/session-context`，基于阶段 B 的 `7e89f76`。

这次没有合并文件工具与子 Agent。它们仍在独立分支 `codex/file-tools-subagents`。第七课的用户、会话、SQLite 和消息记录是本课基础。

## B 到 C，真正增加的是哪一步

B 已经能把消息保存下来，但每次调用模型时只提供系统提示和本次输入。即使数据库里有一万条消息，模型没收到它们，也不会知道你以前说过什么。

C 增加的是**从数据库选出合适的历史，装进这次模型请求**。模型服务本身不需要认识我们的 session_id；session_id 是 ROSETTA 用来找对历史的。

例如第一次输入“我喜欢 Go”，第二次输入“我喜欢什么语言”，第二次调用模型的 messages 变成：

```text
system     你是 ROSETTA……
user       我喜欢 Go
assistant  好的，知道了。
user       我喜欢什么语言
```

模型看到了前文，才有依据回答。每一次新的请求都要重新组装上下文；daemon 活着、socket 没断，并不会自动让远端模型保留对话。

## 先离线验证一遍

在项目目录编译，再重新启动 daemon。编译只生成新二进制，不会替换已经运行的旧进程。

```sh
make build
./bin/rosetta stop personal
./bin/rosetta start personal --provider mock
./bin/rosetta chat personal --user alice --session memory -m "我喜欢 Go"
./bin/rosetta stop personal
./bin/rosetta start personal --provider mock
./bin/rosetta chat personal --user alice --session memory -m /recall
```

最后应包含“上一条用户消息：我喜欢 Go”。Mock 不理解自然语言，`/recall` 只是检查实际传入的 messages，返回此前最后一条用户消息的预览。最多显示 256 个字符；它不直接读数据库，不保存额外内存。因此重启后还能答出，说明 CLI → daemon → SQLite → Runtime → Provider 这条链路走通了。

`/recall` 本身也是普通输入，会被记录。连续两次调用，第二次可能看到上一条输入正是 `/recall`，这是预期行为。

换成此前没有使用过的用户或会话：

```sh
./bin/rosetta chat personal --user bob --session memory -m /recall
./bin/rosetta chat personal --user alice --session another -m /recall
```

它们应显示当前上下文没有更早的用户消息。如果这些身份以前已经聊过，则会返回各自的历史，而不会返回 Alice/memory 的内容。

使用真实模型时，直接用自然语言连续对话即可，无须 `/recall`。本次自动测试都使用 mock 或本地测试服务，没有消耗真实模型额度。

## 新增流程放在哪里

```text
Daemon 保存本次 user 输入，得到 turn ID
  ↓
Conversation 发送 turn.started
  ↓
Store.LoadContext 根据该 turn 查到所属 session
  ↓
读取同一 session 更早的成功对话，结束数据库事务
  ↓
Runtime 组装 system + 历史 + 本次 user 消息
  ↓
Provider 收到完整上下文，生成回答或工具请求
  ↓
继续沿用 B 的完整消息保存、完成提交和终端通知
```

`LoadContext` 使用数据库中的当前 turn 作为查询边界。它从中取得 session_id，再找历史，不接受用户从 CLI 传一个任意内部 session_id 来选数据。每个 Agent 有自己的数据库，而用户与会话在 B 中已对应不同的内部 session_id，这些已有边界继续生效。

读取历史放在短事务里，并限制三秒。读完后才开始模型调用，等待模型生成期间不持有数据库事务。若读取失败，本次运行记录为失败并返回受控错误，不会悄悄把一段有历史的对话当作新对话继续。

Runtime 不直接查 SQLite。Conversation 负责取数据，Runtime 只接收 `[]llm.HistoryTurn`；一个 HistoryTurn 保存某次完整请求内的消息列表。

## 历史记录与模型上下文不是同一份列表

`sessions show` 用于查看实际发生过什么，所以保留成功、失败、取消和中断记录。

模型上下文目前只选 `completed`。例如：

| 请求 | 状态 | 本次模型会看到吗 |
|---|---|---|
| 你说喜欢 Go，助手完整回答 | completed | 在近期范围且放得下时，会 |
| 你提问后模型服务报错 | failed | 不会 |
| 你按 Ctrl+C 取消 | canceled | 不会 |
| 进程崩溃，重启后识别到未完成 | interrupted | 不会 |
| 本次正在处理的输入 | running | 单独作为当前输入加入一次 |

最后一行很重要：本次输入已经保存进数据库，但不能又被当作历史读回来，再追加一次。查询用当前 turn 第一条消息的 ID 作为时间边界，只读取它之前的成功请求。

这里的策略是保守且可解释的：失败输入仍能查阅，但不会影响后续模型对话。以后如果希望保留失败轮次中的某些信息，可以单独设计，不在这一版里猜测如何补齐。

## 为什么历史要按一次请求分组

一段带工具的历史可能是：

```text
user       查询时间
assistant  调用 get_time，ID 为 call_1
tool       call_1 的结果
assistant  根据结果生成最终回答
```

如果随意取“最后三条消息”，可能只剩工具结果，丢了它对应的请求。模型协议需要知道结果归谁。因此 C 在加载时检查：

- 第一条是 user，最后一条是没有工具请求的完整 assistant 回答。
- 工具请求和结果按调用 ID 一一配对，中间不能插入未配对的新模型消息。
- 不允许孤立或重复结果，也不允许把 system/developer 消息作为历史读入。

工具参数格式错误不一定意味着历史损坏。例如模型发出了不合法 JSON，工具返回“参数错误”，模型随后修正，这仍可能是一段完整成功的对话。我们检查的是消息之间的结构关系，不要求每次工具业务都成功。

历史中的工具调用只作为消息交给模型，**不会再次执行**。只有这一次模型新返回的 ToolCall 才进入工具执行流程。

## 上下文太长时怎么办

目前有两层限制。

第一层在读取数据库时：从最新的成功 turn 开始，最多 20 次，累计消息 JSON 最多 32 KiB。每次要么完整装入，要么不装。遇到装不下的 turn 就停止向更早处读取，保留最近的一段成功对话；如果最近一次本身过大，可能一条历史也不加载。之后把选出的列表翻转为从早到晚的顺序。

第二层在每次调用模型前：把 system、历史、本次输入、本次已产生的工具消息和工具定义一起计算大小，最多 64 KiB。

为什么每一轮都算？因为模型可能先请求工具，再根据结果继续生成。本次消息列表会逐渐变大。第一轮能装下，不代表第二轮仍然能装下。

```text
第一轮：system + 旧对话 A + 旧对话 B + 本次输入
第二轮：system + 旧对话 A + 旧对话 B + 本次输入 + 工具请求 + 结果

若第二轮超限：移除整个旧对话 A，再重新计算。
```

旧历史按从旧到新的顺序逐次移除，本次用户输入和本次工具消息完整保留。全部旧历史都移除后仍然超限，就返回 input_limit；不截断工具参数，也不悄悄丢掉本次问题。

DeepSeek 的 API 格式还会增加 model、stream、工具 function 包装等字段。因此适配器新增 `InputSize`，使用实际编码方式计算请求体字节数，Runtime 在组装时也考虑这部分。这个方法只编码数据，不联网、不调用模型。

这些是 **JSON 字节预算**，不是 token 预算，也不是“模型最多只能记住 20 次”。20 次和 32 KiB 是这一版程序选择的上限，目的是先得到明确、可测试的行为。

裁剪不会删除 SQLite 里的任何消息。历史查询仍然能看到旧记录，只是这次模型没有收到它们。当前也不生成摘要、不做向量检索、不单独记录每次实际选中的上下文清单。

## 和已有行为怎样衔接

同一用户的同一会话仍然一次只允许一个活跃 turn。前一个请求完成并提交后，下一个才会读取到它。不同用户、会话、Agent 的历史不会混用。

同一会话切换 provider 或 model 时仍沿用成功历史。例如 mock 的模拟回答也可能被随后启用的真实模型看到；想开始一段干净对话，就换一个新的 session 名称。当前没有自动按模型隔离历史。

数据库 schema 继续使用版本 1，本地 HTTP 协议仍是 v4。已有 B 数据可以直接使用；但旧 B 进程即使支持 v4，也不会自动执行 C 的历史加载逻辑，所以编译后仍需 stop/start。

完成状态的含义保持 B 的约定：最终答案和 completed 已成功写入数据库。即使最后通知终端时连接断开，该成功对话仍可能进入下一次模型上下文，用户可通过 sessions show 确认。

## 代码阅读顺序

1. [conversation/service.go](../../internal/conversation/service.go)：先读历史，再调用 Runtime；原有保存与失败收尾继续复用。
2. [store/context.go](../../internal/store/context.go)：按当前 turn 选取同会话的近期成功对话，控制读取量。
3. [llm/history.go](../../internal/llm/history.go)：HistoryTurn 的含义，以及工具消息完整性检查。
4. [agent/context.go](../../internal/agent/context.go)：组合模型输入，超限时移除整个旧 turn。
5. [agent/runtime.go](../../internal/agent/runtime.go)：每一轮调用前重新组装，当前 turn 的消息单独积累。
6. [provider/mock.go](../../internal/provider/mock.go)：/recall 怎样从真正收到的上下文读取上一条输入。

自动测试验证了重启后的记忆、不同用户/会话/Agent 的隔离、当前输入只加入一次、失败轮次排除、工具消息配对、上下文增长后的整轮裁剪，以及读取失败时不继续调用模型。
