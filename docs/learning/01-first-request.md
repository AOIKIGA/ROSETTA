# 第一课：跑通一次请求

目标：你能运行 ROSETTA，并指着代码解释“我输入一句话，文字为什么会逐段显示”。这一课暂不涉及模型推理、数据库或工具执行。

## 1. 先看到结果

在项目目录执行 `make build`，然后分别打开两个终端。

终端 A：

```sh
./bin/rosetta serve
```

这是长时间运行的服务进程。当前它在前台运行，你能直接看到日志。Daemon 描述的是服务职责；是否脱离终端在后台运行，是后续的进程管理问题。

终端 B：

```sh
./bin/rosetta chat --session main -m "帮我理解一次请求"
```

文字分批出现，开头明确显示模拟模型。这个实现按 Unicode 字符切片，因此中文不会被按字节拆坏。

再执行：

```sh
./bin/rosetta chat --session main --json -m "看看事件"
```

你会看到同一个 run_id、多条递增的 seq，以及 started/delta/completed。CLI 平时只是把这些 JSON 里的 text 字段展示出来。

## 2. 按这条路径读代码

```text
cmd/rosetta/main.go：run
    → internal/client/client.go：Chat
    → HTTP 请求穿过 Unix Socket
    → internal/daemon/daemon.go：chat
    → internal/agent/runtime.go：Run
    → internal/provider/mock.go：Stream
    → emit 回调一层层返回文本事件
    → 客户端扫描每一行 JSON
    → 终端显示
```

不要从所有目录开始读。先沿着上面这一条路径阅读，每次只问“输入是什么、输出是什么、谁调用它”。

## 3. interface：只约定需要的能力

Runtime 需要一个会输出文本的对象，所以定义：

```go
type TextProvider interface {
    Stream(ctx context.Context, message string, emit func(string) error) error
}
```

这里的 `emit` 是回调函数：Provider 每产生一段文本，就调用 `emit(text)`。它返回 error，让输出失败能立即传回 Provider，停止继续生成。

`Mock` 实现了同名同签名的方法，因此能作为 TextProvider 传入。Go 不需要写“implements TextProvider”。入口调用 `agent.New(provider.Mock{...})` 完成组装。

这也解释了为什么现在没有 API Key 仍能开发：Runtime 的调度和事件行为可以用可控实现验证。未来真实 Provider 负责网络请求和供应商协议转换，不把 SDK 类型散布到所有包。

接口目前只支持文本。工具调用需要结构化事件，届时再扩展接口，而不是假装这个签名已经能覆盖完整 Agent。

## 4. context：取消需要逐层配合

假设用户按 Ctrl+C，程序不能继续默默生成。

```text
CLI 收到信号
  → CLI context 取消
  → HTTP 请求被取消
  → 服务端 request context 取消
  → Runtime / Provider 观察到 ctx.Done()
  → 返回错误并释放资源
```

context 不会强行杀掉 goroutine。执行代码必须检查取消；等待时通过 select 同时等定时器和 ctx.Done()。将来 exec 工具也必须显式停止并回收子进程。

`defer cancel()` 表示函数退出时释放这个 context 的计时等资源；它不是“等到进程结束才运行”。

## 5. 两种锁保护不同的事

**内存互斥锁**保护 `active` map。两个 HTTP 请求同时到来时，检查和登记 session 必须作为一个不可交错的小步骤。实际模型执行发生在解锁之后，因此不同 session 可以并发。

登记 `active[session] = true` 后，即使 mutex 已解锁，第二个相同 session 的请求也会被拒绝；第一个请求结束时，defer 删除登记。不要把“持有 session 写权”误解为“整个模型调用期间持有全局 mutex”。

**文件锁**保护 daemon 的进程唯一性。两个独立进程不能共用 Go 内存中的 mutex，因此需要 OS 提供的跨进程机制。这里锁从服务启动一直持有到退出；操作系统会在进程结束后释放锁。

锁文件本身保留是正常的。不能只看文件存在就判断 daemon 活着，也不能在运行时删除锁文件重新创建。

## 6. 为什么已经有 HTTP，还要完成事件

HTTP 200 可以在第一个字输出前发送。接下来模型可能失败、服务可能退出、网络可能中断。因此“连接成功”与“任务完成”是两件事。

客户端把 `turn.completed` 当作本次成功完成的证据。只有 started 和部分 delta 就断流时，它返回错误，不把半段回复当作完整答案。

M2 引入存储后还会增加一条规则：先成功提交完成状态，再对外报告完成。这个顺序决定重启后的记录是否与用户看到的一致。

## 7. 三个小实验

1. 保持服务 A 运行，再启动一个同目录服务。应看到锁冲突，原服务仍能响应。
2. 给某个 session 发一段较长消息，输出过程中按 Ctrl+C，再向同 session 发短消息。新请求应能成功；这验证写权被释放。
3. 观察 `--json` 输出，找到一次 run 的第一个和最后一个事件。思考：如果最后一行缺失，为什么不能默认成功？

这些实验只是帮助理解；自动测试已经覆盖相关行为。你不需要通过手工操作来替代验收。

## 8. 下一课要解决什么

当前程序重启后没有聊天历史，甚至同一个 session 连续两次请求也不共享上下文。下一阶段会引入 SQLite，并用一个具体例子解释事务：怎样保证“用户消息”和“这次 turn 的记录”一起写入，避免只保存了一半。

本课只需读懂调用链、接口和取消传播。热重载、MCP、子 Agent、向量检索以后各用独立例子讲解。
