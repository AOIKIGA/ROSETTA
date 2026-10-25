# 本地通信协议

状态：2026-10-22 已实现。协议版本：3。仅用于同一用户本机进程间通信。本轮增加 Agent/进程实例身份及关闭接口；模型轮次和工具事件沿用上一版；CLI 和 Daemon 需要使用匹配版本。

## 传输

HTTP/1.1 运行在 Unix Socket 上，路径是 `<state-dir>/agents/<agent>/run/daemon.sock`。它不是 TCP 服务；客户端 URL 中的 `http://rosetta` 只是 HTTP 所需的主机占位，实际 Dial 连接本地 socket，不查询该域名。

state 目录要求私有权限，socket 权限 0600。该机制隔离其他普通 OS 用户，不隔离同一账号下的程序。

`GET /v1/health` 返回：

```json
{"status":"ready","protocol":3,"provider":"mock","agent":"personal","instance_id":"随机实例ID","pid":12345,"started_at":"2026-10-22T00:00:00Z"}
```

健康状态可以是 ready 或 stopping。CLI 的 status 在进程锁空闲时自行输出 stopped，不需要已停止的进程回复；锁占用但 socket 不可用则报错。PID 仅用于诊断，每次启动生成新的 instance_id。

`POST /v1/shutdown` 请求：

```json
{"instance_id":"刚从健康接口读取的实例ID"}
```

请求最多 4 KiB，拒绝未知字段和多个对象。实例不匹配返回 409 instance_mismatch。匹配时标记 stopping、拒绝新 turn，并返回同一实例的 stopping 健康状态。关闭动作由 Serve 主循环执行，取消所有活跃请求并等待 HTTP 收尾；CLI 等待实例释放锁后才报告 stopped。不能根据磁盘 PID 文件杀进程。重复请求在服务仍监听时返回同一 stopping 状态。

`POST /v1/chat` 请求：

```json
{"session_id":"main","message":"你好"}
```

session_id 允许 1–64 位 ASCII 字母、数字、下划线、短横线。message 非空白、最多 16 KiB UTF-8 字节；请求体最多 64 KiB。拒绝未知字段和多个 JSON 对象。

## 事件流

成功准入后返回 HTTP 200，Content-Type 为 `application/x-ndjson`。每行是一个完整 JSON 对象，每发一行就 flush。

```json
{"version":3,"run_id":"示意ID","session_id":"main","seq":1,"type":"turn.started"}
{"version":3,"run_id":"示意ID","session_id":"main","seq":2,"type":"model.round.started","round":1}
{"version":3,"run_id":"示意ID","session_id":"main","seq":3,"type":"tool.started","round":1,"tool_call_id":"call1","tool_name":"get_time"}
{"version":3,"run_id":"示意ID","session_id":"main","seq":4,"type":"tool.finished","round":1,"tool_call_id":"call1","tool_name":"get_time","status":"ok"}
{"version":3,"run_id":"示意ID","session_id":"main","seq":5,"type":"model.round.started","round":2}
{"version":3,"run_id":"示意ID","session_id":"main","seq":6,"type":"text.delta","round":2,"text":"查询到的当前时间是……"}
{"version":3,"run_id":"示意ID","session_id":"main","seq":7,"type":"turn.completed","round":2}
```

run_id 实际使用 16 个随机字节的十六进制编码；seq 在每个 run 内从 1 连续递增。同一次请求里 session_id、run_id 和 version 不变。

正常路径是 started → 一个或多个模型轮次（可包含文本和工具事件）→ completed。没有工具调用的问答也包含一个 model.round.started。round 表示模型调用序号，seq 表示整个 run 的事件序号。

tool.finished 的 status 为 ok 或 error；失败类别在 error 字段中，duration_ms 表示耗时，值为 0 时可省略。工具业务失败可以交给模型修正，因此 tool.finished:error 不一定导致 turn.failed。工具参数和原始结果默认不进入本地显示事件。

执行失败在连接可用时发 turn.failed。类别包括 provider_error、provider_http_*、provider_incomplete_response、provider_invalid_event、provider_invalid_tool_call、provider_response_limit、round_limit、tool_call_limit、input_limit、deadline_exceeded 和 canceled 等受控值；不会把原始 Provider 错误直接返回给客户端。

HTTP 200 只表示开始流，不表示 turn 成功。客户端必须收到 `turn.completed` 才认为成功；中途 EOF 视为不完整结果。断连时无法保证客户端收到 failed 事件。

## 准入前错误

```json
{"code":"session_busy","message":"this session already has an active turn"}
```

| HTTP 状态 | code | 含义 |
|---|---|---|
| 400 | invalid_request | 输入格式或大小不合法 |
| 409 | session_busy | 同 session 已有 turn |
| 503 | daemon_stopping | 服务正在关闭，拒绝新 turn |
| 429 | capacity_exceeded | daemon 已有四个 turn |
| 500 | internal_error | 无法建立执行标识等内部错误 |

未知路径和不匹配的方法使用 Go HTTP 路由器默认的 404/405；客户端能报告非 JSON 错误。

## 取消与重连

采用请求连接绑定的执行方式。客户端取消 HTTP 请求时，服务端 request context 取消，Provider 或工具协作退出，session 准入记录释放。

daemon 支持脱离终端运行，但单个 turn 没有 detach、事件持久化、自动重连或重放。用户重试会创建新的 run；当前工具包括只读时间/文件工具与阻塞式 subagent；子 Agent 也只使用只读工具。增加写文件或 exec 后，必须先实现工具执行记录，不能照搬自动重试。

限制：单 turn 两分钟；单次事件写入五秒；客户端单事件最大 1 MiB。父 Runtime 最多六轮模型、八次工具调用，单轮最多四个工具；子任务最多两个，每个最多三轮模型、四次工具调用。普通工具三秒，subagent 最长一分钟，仍受整个 turn 两分钟限制。子任务进度不作为独立协议事件发送，内部 Status/Wait 尚未映射到本地 HTTP/CLI。审批和用量事件留待后续扩展。
