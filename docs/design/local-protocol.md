# 本地通信协议

状态：2026-10-22 已实现。协议版本：2。仅用于同一用户本机进程间通信。升级增加了模型轮次和工具事件；CLI 和 Daemon 需要使用匹配版本。

## 传输

HTTP/1.1 运行在 Unix Socket 上，路径是 `<state-dir>/daemon.sock`。它不是 TCP 服务；客户端 URL 中的 `http://rosetta` 只是 HTTP 所需的主机占位，实际 Dial 连接本地 socket，不查询该域名。

state 目录要求私有权限，socket 权限 0600。该机制隔离其他普通 OS 用户，不隔离同一账号下的程序。

`GET /v1/health` 返回：

```json
{"status":"ready","protocol":2,"provider":"mock"}
```

`POST /v1/chat` 请求：

```json
{"session_id":"main","message":"你好"}
```

session_id 允许 1–64 位 ASCII 字母、数字、下划线、短横线。message 非空白、最多 16 KiB UTF-8 字节；请求体最多 64 KiB。拒绝未知字段和多个 JSON 对象。

## 事件流

成功准入后返回 HTTP 200，Content-Type 为 `application/x-ndjson`。每行是一个完整 JSON 对象，每发一行就 flush。

```json
{"version":2,"run_id":"示意ID","session_id":"main","seq":1,"type":"turn.started"}
{"version":2,"run_id":"示意ID","session_id":"main","seq":2,"type":"model.round.started","round":1}
{"version":2,"run_id":"示意ID","session_id":"main","seq":3,"type":"tool.started","round":1,"tool_call_id":"call1","tool_name":"get_time"}
{"version":2,"run_id":"示意ID","session_id":"main","seq":4,"type":"tool.finished","round":1,"tool_call_id":"call1","tool_name":"get_time","status":"ok"}
{"version":2,"run_id":"示意ID","session_id":"main","seq":5,"type":"model.round.started","round":2}
{"version":2,"run_id":"示意ID","session_id":"main","seq":6,"type":"text.delta","round":2,"text":"查询到的当前时间是……"}
{"version":2,"run_id":"示意ID","session_id":"main","seq":7,"type":"turn.completed","round":2}
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
| 429 | capacity_exceeded | daemon 已有四个 turn |
| 500 | internal_error | 无法建立执行标识等内部错误 |

未知路径和不匹配的方法使用 Go HTTP 路由器默认的 404/405；客户端能报告非 JSON 错误。

## 取消与重连

采用请求连接绑定的执行方式。客户端取消 HTTP 请求时，服务端 request context 取消，Provider 或工具协作退出，session 准入记录释放。

没有 detach、事件持久化、自动重连或重放。用户重试会创建新的 run；当前工具只有只读 get_time。增加写文件或 exec 后，必须先实现工具执行记录，不能照搬自动重试。

限制：单 turn 两分钟；单次事件写入五秒；客户端单事件最大 1 MiB。每个 turn 最多六轮模型、八次工具调用，单轮最多四个工具。审批和用量事件留待后续扩展。
