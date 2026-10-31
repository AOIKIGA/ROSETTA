# 存储与身份：聊天记录现在归谁、放在哪里

日期：2026-10-26。对应阶段 B，分支 `codex/storage-identity`。

> 2026-10-30 更新：本文保留该阶段开发时的说明，main 现已整合第六至八课的能力。父会话持久化、历史记忆和文件/子 Agent 工具可同时使用；子任务内部过程仍不独立落盘。整合说明见 [第九课](09-merging-branches.md)。

本课基于第五课的后台进程。文件工具与子 Agent 在独立分支上，第六课编号留给那部分工作；理解本课不需要先学它们。

## 先看能用起来的变化

以前，daemon 收到一句话就调用 Runtime，返回后局部消息列表会被丢弃。`--session main` 只帮助它拒绝同一会话的并发请求。

现在，daemon 在执行前先找到“这个 Agent 下，这个用户的这个会话”，把输入存起来。执行过程中保存完整消息，结束时保存结果。下次打开程序，仍能查到记录。

但有一个功能边界：**保存历史已经完成，把历史装入下一次模型请求还没做。** 例如先说“我叫小明”，下一次再问“我叫什么”，现在不能用模型是否答对来验收 B。B 的验收方式是查询历史，确认两条输入被正确保存。阶段 C 才处理模型记忆和上下文长度。

## 用十条命令认识数据归属

在项目目录运行，mock 不联网：

```sh
make build
./bin/rosetta start personal --provider mock
./bin/rosetta chat personal --user alice --session main -m "Alice 的第一条消息"
./bin/rosetta chat personal --user bob --session main -m "Bob 的第一条消息"
./bin/rosetta chat personal --user alice --session learning -m /time
./bin/rosetta sessions list personal --user alice
./bin/rosetta sessions show personal --user alice --session main
./bin/rosetta stop personal
./bin/rosetta start personal --provider mock
./bin/rosetta sessions show personal --user alice --session learning
```

如果 personal 已在运行，start 不会替换其配置；想明确切换到 mock，先 stop 再 start。升级前仍运行阶段 A 的进程，也先用新版 stop 关闭，再启动新版。

应该观察到：Alice 有 main 和 learning 两个会话；Bob 的 main 单独保存。learning 中能看到 get_time 的调用参数和返回结果。停止、重启以后这些记录依然存在。

省略参数相当于 `--user defaultuser --session main`。Agent 默认 personal。`--state-dir` 仍表示最外层状态根目录，所有命令使用同一值即可访问同一组数据。

## Agent、User、Session、Turn、Message

不要先背类型名。把它们对应到一个实际场景：

```text
personal（一个后台助手，一份数据库）
├── alice（本地用户标签）
│   ├── main（一个会话）
│   │   ├── turn 1：你输入一句话，助手处理并回答
│   │   │   ├── user 消息
│   │   │   └── assistant 消息
│   │   └── turn 2：你再次输入，助手再次处理
│   └── learning（另一个会话）
└── bob
    └── main（Bob 自己的 main）
```

这里的 turn 是**一次用户请求从开始到结束**。一次 turn 可能让模型工作多轮，中间调用工具；它不等于一次模型调用。

| 名称 | 用来回答的问题 | 本次怎么实现 |
|---|---|---|
| Agent | 在和哪个助手交互？ | 不同进程、目录、SQLite 文件 |
| User | 数据记在谁的名下？ | users 表，名字在当前 Agent 内唯一 |
| Session | 在这个人的哪段对话里？ | sessions 表，同一用户下名称唯一 |
| Turn | 是哪一次请求？成功了吗？ | turns 表，记录时间、状态和原因 |
| Message | 这次请求里谁说了什么？ | messages 表，保存正文和工具调用结构 |

所以，**会话名称 main 不是全局唯一身份**。系统通过 `(Agent, User, Session名称)` 找到会话，再使用数据库生成的内部 ID 关联记录。内部 session_id 在重启后不变；run_id 每次请求重新生成，它同时就是数据库的 turn ID。

用户和会话名称区分大小写，只接受 1–64 位 ASCII 字母、数字、下划线、短横线。当前 user 是数据归属标签，没有密码或登录流程。同一个 OS 账号能主动用 `--user bob` 查询 Bob 的数据；这是个人工具的数据划分，还不是多租户权限系统。

## 为什么数据库放在 Agent 目录里

```text
~/.rosetta/agents/
├── personal/
│   ├── agent.json
│   ├── state.db
│   ├── run/...
│   └── logs/...
└── work/
    ├── agent.json
    ├── state.db
    ├── run/...
    └── logs/...
```

沿用 A 的边界：一个 Agent 的后台进程管理自己的数据库。CLI 通过 socket 请求它读写，CLI 自己不打开数据库。这样数据库初始化和异常状态收尾都有一个明确负责人；也意味着历史查询需要该 Agent 正在运行。

同一个 Agent 的所有用户和会话放在一份数据库里，用表关系划分。没有为每个会话创建一份 SQLite，也不再创建多层用户目录。数据库内用外键保证消息属于某次运行，运行属于某个会话，会话属于某个用户。

SQLite 是嵌入程序的数据库，不需要另启数据库服务。选用 `database/sql` 加 `modernc.org/sqlite`：前者是 Go 的标准数据库接口，后者提供纯 Go 的 SQLite 实现。代价是增加外部模块和编译体积，收益是普通构建不需要安装 C 版 SQLite 或 C 编译器。

当前 schema 版本为 1，保存在 SQLite 的 `user_version` 中。首次启动建表；后续启动检查版本。遇到程序不认识的新版数据库会报错，不会尝试重建覆盖。还没有实际跨 schema 版本的数据迁移，因为这是第一版数据库。

## 一次聊天是怎么流转的

可以按下面的顺序读代码：

```text
CLI 解析 agent / user / session / message
  → Client 通过对应 Agent 的 socket 发 HTTP 请求
  → Daemon 验证输入，检查该用户的该会话是否正在运行
  → Store.BeginTurn：找到或创建身份，保存本次输入
  → Conversation 调用 Runtime
      → Runtime 调模型，产生文字和完整消息
      → Conversation 把完整消息交给 Store 保存
      → Runtime 按需要继续执行工具、调用模型
  → Store.FinishTurn：保存最终回答，同时标记 completed
  → Daemon 向 CLI 发送 turn.completed
```

`internal/identity` 很小：负责默认名字和名字是否合法。`internal/store` 才知道 SQL 怎样写；它不调用模型。`internal/agent` 仍负责模型和工具循环，通过一个“完整消息已产生”的回调交出消息，不直接操作数据库。

新增的 `internal/conversation` 把执行和保存接起来：它接收 Runtime 的完整消息，决定什么时候写库、什么时候把整次请求标记为完成。这样不必把 SQL 塞进模型循环，也不必让 HTTP handler 包办所有执行细节。

可以把两个回调理解为：

- **事件回调**：告诉终端“现在进展到哪了”，例如文字片段、工具开始、工具结束。
- **记录回调**：告诉存储层“这一条完整消息已经拿到了”，例如完整 assistant 消息或完整工具结果。

流式输出适合及时显示；完整消息适合长期保存。当前不存每个 text.delta，也不把半截回复当作完整回答。

## 工具消息保存什么

`-m /time` 的一次成功请求通常有四条消息：

```text
user       “/time”
assistant  要调用 get_time，调用 ID 为 call_x，参数为 {...}
tool       call_x 的执行结果为 {...}
assistant  根据结果生成的最终回答
```

assistant 的 `tool_calls` 与 tool 的 `tool_call_id` 共同说明“这条结果对应哪个请求”。只保存最后一句自然语言会丢失这层关系，所以现在把完整结构留下。多工具时同样靠调用 ID 区分结果。

Runtime 先收齐并校验模型的工具请求，再保存 assistant 消息，然后执行工具并记录结果。中途取消或崩溃时，可能留下有请求、没结果的记录；它只说明执行过程不完整，不能据此断言工具从未执行。当前没有自动续跑。阶段 C 选取模型上下文时还要处理这些不完整轮次。

## 为什么“保存成功”要放在“通知完成”之前

如果先告诉终端完成，再保存时磁盘写入失败，用户看到的“成功”就没有可查询记录。因此我们把最终 assistant 消息和 completed 状态放在**同一个事务**里提交，提交成功后才发送完成事件。

事务可以先理解为“一组数据库修改要一起成功”。比如首次聊天创建用户、创建会话、创建运行、保存用户输入，也在同一个事务里：不会因为最后一步失败而只剩下半套新数据。

但保存后也可能刚好断开网络连接。此时数据库中的完成状态仍然成立，客户端却可能没收到完成事件。程序不会把已经完成的运行改成失败；你可以查询历史确认。当前没有请求去重，重新发送同一句话会产生新的 turn。

| 运行状态 | 含义 |
|---|---|
| running | 请求已记录，尚未结束 |
| completed | 最终回答已和完成状态一起提交 |
| failed | 执行失败，例如模型异常或整体超时 |
| canceled | 请求被取消，包括服务正常停止时取消活跃请求 |
| interrupted | 上一进程留下未结束记录，下次启动时识别到 |

取消时请求的 context 已经失效，直接用它写“已取消”也会被取消。所以收尾会临时使用一个独立、最多三秒的 context 保存状态。若磁盘本身不可写，仍不能保证这次收尾成功；遗留的 running 会在后续成功启动时标记 interrupted。

## 并发不会把用户混在一起

旧准入表只以 session 名称为键；现在以 `{User, Session}` 为键。Agent 本身已由独立进程隔离。

因此 Alice/main 与 Bob/main 可以同时工作；Alice/main 的第二个请求必须等第一个结束。全 Agent 同时最多四次运行的限制仍在。

数据库连接池当前限制为一个连接。这不代表四个请求只能逐个调用模型：数据库事务只覆盖很短的记录操作，模型生成期间不持有事务或连接。多个请求可以同时等待模型，轮到写库时短暂串行。以后有实际瓶颈，再考虑单写多读，不提前增加连接管理复杂度。

SQLite 开启 WAL，并用 FULL 同步保证提交的持久性。运行时可能看到 `state.db-wal` 和 `state.db-shm`，它们由 SQLite 管理。当前没有在线备份命令；需要复制数据时先正常 stop，再复制 state.db 和 agent.json。

## 历史如何分页

```sh
./bin/rosetta sessions show personal --user alice --session learning --limit 2 --json
```

如果有更多记录，输出会带 `next_after`。将返回值放进下一条命令的 `--after`，同时保持 agent、user、session 不变即可。show 的 limit 按**消息条数**计算，一次带工具的 turn 可能跨页；它不是“最近几轮对话”。从默认游标 0 开始，按写入顺序由早到晚阅读。

list 使用会话 ID 游标，按内部 ID 排序，不承诺按创建时间排序。两种游标不能互换。历史页还受 2 MiB 上限约束；即使 limit 还没用完，也可能提前分页，但不会截断一条消息。

历史读取的是当前已提交记录。请求仍在执行时查询，可能只看到用户输入或部分完整消息，并显示 running；不同分页请求之间也可能有新的消息写入。这是查询接口，不是冻结的导出快照，也不是模型上下文装配接口。

## 已验证与下一步

自动测试覆盖不同用户的同名会话、不同 Agent 的数据、重启后记录保留、完整工具消息配对、分页、取消状态、未完成记录的启动标记，以及写入失败时不发送完成事件。真实 CLI 测试在临时目录启动 mock daemon，正常结束后清理，不使用真实模型额度。

建议阅读顺序：`identity/identity.go` → `store/types.go` → `store/sqlite.go` 的 BeginTurn/FinishTurn → `conversation/service.go` → `daemon/daemon.go` 的 chat → `store/query.go`。先抓住“一次输入在什么时候保存、何时算完成”，不必先读懂全部 SQL。

下一步 C 将让 Runtime 的初始 messages 包含选中的历史。届时需要明确只读哪些 turn、如何保证 assistant/tool 配对、历史太长时如何裁剪，以及 system prompt 怎么与历史组合。B 已提供原始数据和完成状态，C 可以在这个基础上单独实现和验证。
