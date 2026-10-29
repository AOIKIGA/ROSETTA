# 本地通信协议

状态：2026-10-29，阶段 C。协议版本：4（字段兼容阶段 B）。仅用于同一 OS 账号的本机进程间通信。增加逻辑用户、持久化会话身份与历史查询；模型轮次和工具事件沿用上一版。聊天/历史要求 CLI 和 Daemon 版本匹配；新版 CLI 的 stop 单独兼容 v3 健康与关闭响应，便于升级旧进程。

## 传输

HTTP/1.1 运行在 Unix Socket 上，路径是 `<state-dir>/agents/<agent>/run/daemon.sock`。它不是 TCP 服务；客户端 URL 中的 `http://rosetta` 只是 HTTP 所需的主机占位，实际 Dial 连接本地 socket，不查询该域名。

state 目录要求私有权限，socket 权限 0600。该机制隔离其他普通 OS 用户，不隔离同一账号下的程序。

`GET /v1/health` 返回：

```json
{"status":"ready","protocol":4,"provider":"mock","agent":"personal","instance_id":"随机实例ID","pid":12345,"started_at":"2026-10-22T00:00:00Z"}
```

健康状态可以是 ready 或 stopping。CLI 的 status 在进程锁空闲时自行输出 stopped，不需要已停止的进程回复；锁占用但 socket 不可用则报错。PID 仅用于诊断，每次启动生成新的 instance_id。

`POST /v1/shutdown` 请求：

```json
{"instance_id":"刚从健康接口读取的实例ID"}
```

请求最多 4 KiB，拒绝未知字段和多个对象。实例不匹配返回 409 instance_mismatch。匹配时标记 stopping、拒绝新 turn，并返回同一实例的 stopping 健康状态。关闭动作由 Serve 主循环执行，取消所有活跃请求并等待 HTTP 收尾；CLI 等待实例释放锁后才报告 stopped。不能根据磁盘 PID 文件杀进程。重复请求在服务仍监听时返回同一 stopping 状态。

`POST /v1/chat` 请求：

```json
{"user":"defaultuser","session":"main","message":"你好"}
```

user/session 省略或为空时分别取 defaultuser/main；允许 1–64 位 ASCII 字母、数字、下划线、短横线，区分大小写。message 非空白、最多 16 KiB UTF-8 字节；请求体最多 64 KiB。拒绝未知字段和多个 JSON 对象。旧请求字段 session_id 不再接受。

服务按当前 Agent + user + session 查找或创建持久化会话，再创建 running 状态的 turn 并保存用户输入。数据库提交成功后才开始事件流。user 是本地命名空间，不是鉴权凭证；有 socket 访问权的同账号程序可以选择任意 user。

## 会话上下文（C）

chat 的 wire 字段不变。当前 turn 入库并发送 turn.started 后，读取同一数据库会话中更早的 completed turn；最多 20 次、累计消息 JSON 32 KiB。失败、取消、中断、其他仍在运行的 turn 均不进入上下文。模型输入按 system → 历史完整消息 → 本次 user/assistant/tool 消息排列；不会从历史记录执行工具。

每轮请求受 64 KiB 限制，含工具定义及 DeepSeek 实际包装字段。先按整次历史 turn 从旧到新移除，不拆散工具配对；当前 turn 独自超限则以 input_limit 结束。历史存储不受上下文裁剪影响。模型切换不会自动清空历史，空白对话可使用新的 session 名称。历史读取失败时返回 storage_error；不静默退回无历史请求。

这不改变 sessions show 的含义：它仍展示所有状态的完整记录，而非本次模型具体看到的上下文。模型输入选择当前未独立落盘，也没有 token 统计或摘要。

## 事件流

成功准入后返回 HTTP 200，Content-Type 为 `application/x-ndjson`。每行是一个完整 JSON 对象，每发一行就 flush。

```json
{"version":4,"run_id":"示意ID","session_id":"数据库会话ID","user":"defaultuser","session":"main","seq":1,"type":"turn.started"}
{"version":4,"run_id":"示意ID","session_id":"数据库会话ID","user":"defaultuser","session":"main","seq":2,"type":"model.round.started","round":1}
{"version":4,"run_id":"示意ID","session_id":"数据库会话ID","user":"defaultuser","session":"main","seq":3,"type":"tool.started","round":1,"tool_call_id":"call1","tool_name":"get_time"}
{"version":4,"run_id":"示意ID","session_id":"数据库会话ID","user":"defaultuser","session":"main","seq":4,"type":"tool.finished","round":1,"tool_call_id":"call1","tool_name":"get_time","status":"ok"}
{"version":4,"run_id":"示意ID","session_id":"数据库会话ID","user":"defaultuser","session":"main","seq":5,"type":"model.round.started","round":2}
{"version":4,"run_id":"示意ID","session_id":"数据库会话ID","user":"defaultuser","session":"main","seq":6,"type":"text.delta","round":2,"text":"查询到的当前时间是……"}
{"version":4,"run_id":"示意ID","session_id":"数据库会话ID","user":"defaultuser","session":"main","seq":7,"type":"turn.completed","round":2}
```

run_id 实际使用 16 个随机字节的十六进制编码；seq 在每个 run 内从 1 连续递增。session_id 是持久化会话的内部 ID，session 是用户输入的会话名。同一次请求里 user、session、session_id、run_id 和 version 不变；run_id 同时是数据库 turn ID。客户端校验身份字段和连续序号。

正常路径是 started → 一个或多个模型轮次（可包含文本和工具事件）→ completed。没有工具调用的问答也包含一个 model.round.started。round 表示模型调用序号，seq 表示整个 run 的事件序号。

tool.finished 的 status 为 ok 或 error；失败类别在 error 字段中，duration_ms 表示耗时，值为 0 时可省略。工具业务失败可以交给模型修正，因此 tool.finished:error 不一定导致 turn.failed。工具参数和原始结果默认不进入本地显示事件。

执行失败在连接可用时发 turn.failed。类别包括 provider_error、provider_http_*、provider_incomplete_response、provider_invalid_event、provider_invalid_tool_call、provider_response_limit、round_limit、tool_call_limit、input_limit、deadline_exceeded、canceled、storage_error 等受控值；不会把原始 Provider 错误直接返回给客户端。

HTTP 200 只表示开始流，不表示 turn 成功。客户端必须收到 `turn.completed` 才认为成功；中途 EOF 视为不完整结果。最终回答与 completed 状态先在数据库中原子提交，再发送完成事件。数据库提交成功但事件传输失败时，历史仍显示 completed，客户端则可能报告不完整；不会把已提交结果改成失败。断连时无法保证客户端收到 failed 事件。

## 准入前错误

```json
{"code":"session_busy","message":"this session already has an active turn"}
```

| HTTP 状态 | code | 含义 |
|---|---|---|
| 400 | invalid_request | 输入格式或大小不合法 |
| 409 | session_busy | 同用户的同名会话已有 turn |
| 503 | daemon_stopping | 服务正在关闭，拒绝新 turn |
| 429 | capacity_exceeded | daemon 已有四个 turn |
| 500 | internal_error | 无法建立执行标识等内部错误 |
| 500 | storage_error | 数据库读写失败 |
| 503 | storage_unavailable | 存储尚未就绪 |
| 404 | session_not_found | 所选用户没有该会话（历史查询） |

未知路径和不匹配的方法使用 Go HTTP 路由器默认的 404/405；客户端能报告非 JSON 错误。

## 历史查询

`GET /v1/sessions?user=alice&limit=20&after=会话ID`：仅返回当前 Agent 下 alice 的会话。after 默认空，按内部会话 ID 升序分页，不表示创建时间顺序。不存在的用户返回空列表，不创建用户。

```json
{"sessions":[{"id":"会话ID","user":"alice","name":"main","created_at":"2026-10-26T00:00:00Z","turns":1}]}
```

还有下一页时增加字符串 `next_after` 字段，值为本页最后一个会话 ID。after 最多 64 字节。

`GET /v1/history?user=alice&session=main&limit=20&after=0`：仅返回所选用户/会话的消息。不存在时返回 404，不创建会话。after 是非负整数消息 ID，默认 0；按消息 ID 升序读取，排除游标本身。

```json
{
  "session":{"id":"会话ID","user":"alice","name":"main","created_at":"2026-10-26T00:00:00Z","turns":1},
  "messages":[
    {"id":1,"ordinal":1,"turn":{"id":"运行ID","session_id":"会话ID","status":"completed","provider":"mock","started_at":"2026-10-26T00:00:00Z","finished_at":"2026-10-26T00:00:01Z"},"role":"user","content":"你好"}
  ],
  "next_after":1
}
```

示例省略下一页 assistant 消息。`ordinal` 是同一 turn 内从 1 开始的顺序号；完整 assistant 消息可含 tool_calls，tool 消息含 tool_call_id。错误/取消运行的 turn 对象还包含受控 error 原因；running 没有 finished_at。状态有 running/completed/failed/canceled/interrupted。

两个查询均默认 user=defaultuser，history 默认 session=main；limit 默认 20，范围 1–100。history 的 limit 按消息条数计算，不按 turn；一个工具请求与结果可能跨页。history 响应最大 2 MiB，按完整消息提前分页，不截断正文；游标始终作用于所选用户/会话内，不能绕过归属筛选。分页读取当前已提交数据，跨请求不保证冻结快照。

响应为普通 JSON，Cache-Control: no-store。查询最多三秒、响应写入最多五秒；不是 NDJSON，也不读取模型。历史查询要求对应 daemon 运行。

## 取消与重连

采用请求连接绑定的执行方式。客户端取消 HTTP 请求时，服务端 request context 取消，Provider 或工具协作退出，用户/会话准入记录释放。使用独立的有界收尾 context 记录 canceled，整体超时记录 failed/deadline_exceeded。重启时把遗留 running 标记 interrupted，不续跑或重放工具。

daemon 支持脱离终端运行，但单个 turn 没有 detach、事件持久化、自动重连或重放。用户重试会创建新的 run；当前工具只有只读 get_time。增加写文件或 exec 后，必须先实现工具执行记录，不能照搬自动重试。

限制：单 turn 两分钟；单次事件写入五秒；客户端单事件最大 1 MiB。每个 turn 最多六轮模型、八次工具调用，单轮最多四个工具。审批和用量事件留待后续扩展。
