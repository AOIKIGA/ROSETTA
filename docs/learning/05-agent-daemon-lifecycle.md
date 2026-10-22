# 从前台 Server 到后台 Daemon

日期：2026-10-22。本课对应阶段 A 的实际代码。阶段 B/C 的用户、会话数据库与跨 turn 历史尚未实现。

## 1. 为什么改名

原来的 `daemon.Server` 不只提供 HTTP handler，还负责 socket、并发准入和服务关闭。现在增加后台进程管理后，叫 `daemon.Daemon` 更能表达职责，所以文件也从 `server.go` 改为 `daemon.go`。

它里面仍然有一个标准库 `http.Server`：

```text
daemon.Daemon              应用层：管理一次服务运行
    └── http.Server        传输层：接收 HTTP 请求、执行 handler
```

前者是我们写的服务对象，后者是 Go 标准库的 HTTP 组件。变量名称不是后台运行的原因；真正的后台运行来自创建独立操作系统进程。

## 2. start 和 serve 是什么关系

```text
终端执行 rosetta start personal
    │
    ├── 解析名称、目录与配置
    ├── 检查是否已有进程
    └── 启动新的 rosetta serve personal 子进程
              ├── 创建新的 OS session，脱离终端进程组
              ├── stdin → /dev/null
              ├── stdout/stderr → daemon.log
              ├── 持有 daemon.lock
              ├── 写入新的 instance.json
              └── 监听 daemon.sock
    │
    ├── 等待子进程通过专用管道报告就绪
    ├── 再通过 socket 验证实例
    └── 打印成功并退出
```

Go 的 `exec.Cmd.Start` 只表示进程成功创建，不表示配置有效、socket 已经监听。专用管道负责将“已就绪”或启动错误送回父进程。管道在子进程里是 fd 3；隐藏用途参数 `--ready-fd=3` 只用于 start 启动的 serve，不需要手工使用。

`exec.Command` 创建子进程，`SysProcAttr.Setsid` 在 macOS/Linux 上建立新的 OS session。这里的 OS session 是操作系统的进程分组，与聊天 session 没有关系。

不能用一个绑定 start 命令 context 的 `exec.CommandContext` 来维持后台进程。start 成功后退出是正常行为，不应该因此结束 daemon。

不过，启动未完成时取消 start，或者启动超过 10 秒，都应清理本次创建的子进程，避免失败命令留下半启动的服务。清理只针对刚刚创建并持有句柄的子进程；不使用磁盘 PID 文件。

## 3. 四种不同的“身份”

| 身份 | 示例 | 生命周期 |
|---|---|---|
| Agent 名称 | personal | 跨重启保持 |
| PID | 12345 | 当前操作系统进程，可能被系统重复使用 |
| instance_id | 随机标识 | 每次 daemon 启动重新生成 |
| run_id | 随机标识 | 每次用户请求重新生成 |

为什么有 PID 还需要 instance_id？因为 PID 会复用。旧文件中的 PID 可能已经属于别的程序；当前代码从不根据这个文件发 kill。

Agent 的真正独占依据是 daemon 持有的文件锁。进程退出，操作系统释放锁；磁盘上的 lock 文件仍然存在，这不意味着服务仍在运行。不能删掉 lock 文件再重新创建，否则不同进程可能锁住不同 inode，破坏互斥。

status 先查看锁，再连接 socket 验证版本和 Agent 身份。锁仍被占用但服务不响应时，报告不可用，不伪装成 stopped。

## 4. stop 怎么关闭服务

```text
CLI 查询健康状态，获得 instance_id
    ↓
POST /v1/shutdown {"instance_id":"..."}
    ↓
Daemon 校验身份，标记 stopping，拒绝新 turn
    ↓
通知 Serve 的主循环退出，返回关闭确认
    ↓
取消服务 context，让模型与工具退出
    ↓
http.Server.Shutdown 等待处理中的 handler 收尾
    ↓
删除 instance.json 和 socket，释放锁
    ↓
CLI 确认锁已释放，再返回 stopped
```

不能在 shutdown 的 HTTP handler 里直接等待 `http.Server.Shutdown`：它会等待所有 handler 完成，其中也包含它自己。当前做法是关闭一个 channel，让 Serve 所在的外层流程执行 shutdown。

HTTP 收尾最多等待 6 秒，stop 命令最多等待 10 秒。超时明确报错，不自动强制杀某个 PID。如果旧实例退出期间另一个实例已经启动，stop 会报告实例已经变化，不继续关闭新的实例。

对 chat 按 Ctrl+C，取消的是这次 HTTP 请求；对前台 serve 按 Ctrl+C，取消的是整个服务。对启动终端发出的 Ctrl+C/SIGHUP 不会影响已经脱离其进程组的 daemon。

## 5. 目录与配置

每个 Agent 使用 `<root>/agents/<agent>/`。它有独立配置、运行目录和日志；root 默认 `~/.rosetta`。

```text
agents/personal/
├── agent.json
├── run/daemon.sock
├── run/daemon.lock
├── run/instance.json
└── logs/daemon.log
```

现在不会创建 state.db，因为会话持久化要等下一阶段。

配置优先级是内置默认值 → agent.json → 本次显式启动参数。例如 agent.json 中 `max_tokens` 为 512，执行 `start personal --max-tokens 64`，只让这次进程使用 64，文件仍为 512。

首次启动生成的是默认配置，即使传入 `--provider deepseek`，文件中仍保存默认 mock。希望默认配置改变时，编辑 agent.json 并重启。重复 start 不修改已有进程的配置。

配置文件使用临时文件写完、同步，再发布的方式初始化。并发 start 不应覆盖已有配置，也不应让其他进程读到写了一半的 JSON。日志只记录运行诊断，不作为会话数据库；当前未实现日志轮转。

API key 只从启动环境读取。它不会放进 agent.json、instance.json，也不会作为子进程命令行参数传递。子进程直接继承环境。

## 6. 自己验证

```sh
make build
./bin/rosetta start personal
./bin/rosetta start work
./bin/rosetta status personal
./bin/rosetta status work
```

此时可以关闭原来的终端，在新终端进入项目目录：

```sh
./bin/rosetta chat personal -m /time
./bin/rosetta stop personal
./bin/rosetta status work
./bin/rosetta stop work
```

personal 的关闭不会影响 work。再次启动 personal 时，Agent 名称和配置保留，instance_id 变化。但新请求不会记得上一条消息，因为本轮没有实现聊天历史。

如升级前仍有旧版 serve 运行，先在旧终端 Ctrl+C。旧版 socket 在 root 下，新版在 Agent 的 run 目录下，不能用新版 stop 去接管旧版服务。

## 7. 源码阅读顺序

1. `cmd/rosetta/main.go`：命令分流、可选 Agent 名称、显式参数覆盖、start/serve 组件组装。
2. `internal/state/state.go`：名称与目录边界、配置读写。
3. `internal/daemon/process.go`：Start/Status/Stop 和启动握手。
4. `internal/daemon/process_unix.go`：Setsid 与文件锁状态探测。
5. `internal/daemon/daemon.go`：Daemon 对象、关闭接口、服务 context 与 HTTP 收尾。
6. `internal/daemon/listener_unix.go`：独占锁与残留 socket 恢复。

`internal/agent/runtime.go` 的模型/工具循环没有改动。阶段 B/C 再加会话服务、SQLite 存储，以及“先提交完成记录、再发送 turn.completed”的边界。

## 8. 已验证与未覆盖的边界

测试通过实际编译后的 CLI 验证：启动程序退出、进程组脱离、聊天 Ctrl+C、多个 Agent、并发 start、缺密钥错误回传、正常停止、配置不被启动参数覆盖，以及 SIGKILL 后的残留 socket 恢复。另有测试覆盖启动中途取消、过期实例关闭请求、关闭时取消活跃 turn、私有目录和符号链接校验。

这些测试使用 mock 和本地测试服务，不调用真实 DeepSeek。测试在 macOS 执行；Linux 的同一 Unix 实现会做交叉编译检查，但不能把交叉编译当成 Linux 运行验证。

本阶段没有开机自启、进程崩溃自动拉起、聊天断线后继续后台执行、强制停止、会话历史恢复或操作系统级多租户隔离。
