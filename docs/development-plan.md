# 开发路线与协作方式

> 2026-10-31 当前实现：main 整合文件工具/子 Agent、B、C 和 .env 启动凭证读取。合并过程见 [第九课](learning/09-merging-branches.md)。

## 2026-10-30 至 31：把工具分支合入 main

- 合并前 main 位于 4277e74，工具分支位于 f99b7da，共同祖先为 6165fd5。main 有 3 个独有提交，工具分支有 1 个独有提交。
- 保留 B 的用户/会话存储、C 的历史输入和预算、后续 .env / --env-file 功能；加入 workspace、list_files/read_files 和阻塞式 subagent。
- 解决 Runtime、CLI、路径与文档冲突。每个父 turn 的 task.Manager 统一在 RunWithHistory 建立，使普通 Run、带记录 Run 与带历史 Run 都能调用子任务。
- 子 Runtime 不读取父会话历史，也不使用父消息记录回调。父会话保存 subagent 调用和返回结果，后续上下文可以加载这组完整交换；历史工具不会重做。
- 新增跨模块测试验证历史、子任务、消息保存协作；原有 CLI 测试合并后同时覆盖文件读取、委派、重启历史与 .env 启动。
- 合并后 `make check`（vet + 全量 race tests）通过；测试使用 mock 和本地服务，未调用真实模型。
- 2026-10-31 完成本机 `make build` 与关闭 CGO 的 Linux amd64 交叉编译，均通过；Linux 未做运行验证。

## 2026-10-29：阶段 C，会话上下文

- B（7e89f76）与文件工具/子 Agent（f99b7da）分别推送到远端分支。C 在 `codex/session-context` 从 B 开发，不合并子 Agent 分支。
- Store.LoadContext 以数据库中的当前 turn 为边界，只读同一 session 更早的 completed；最近最多 20 次、消息 JSON 累计 32 KiB，按时间顺序送入 Runtime。数据库 schema 仍为 1，不需要迁移。
- Conversation 在 turn.started 后有界读取历史；失败时记录 storage_error，不静默丢掉历史继续请求模型。
- 校验完整 user/assistant/tool 消息结构及工具请求和结果配对，禁止把存储的 system 消息作为历史。工具历史只作为输入，不重新执行。
- Runtime 每轮组合 system + 历史 + 本次消息。超过 64 KiB 时先移除最旧的完整历史，保留本次输入和工具交换；仍超限则停止。DeepSeek 提供真实请求体大小计算，不额外调用模型。
- mock 新增 /recall，通过真实 CLI 验证重启后记忆、用户/会话/Agent 隔离。当前不增加摘要、向量检索、自动续跑或新的配置项；历史不会无限增长到模型输入里。
- 本地 HTTP 协议沿用 v4。升级仍须重启 daemon，旧 B 进程不会因替换二进制自动获得记忆。
- 验证通过：`make check`（vet + 全量 race tests），含真实 CLI 重启后 /recall 与身份隔离；本机 `make build`、CGO 关闭的 Linux amd64 交叉编译通过。Linux 未运行验证，未调用真实模型。

## 2026-10-26：阶段 B，存储与身份

- 从阶段 A 提交 `6165fd5` 开始，工作分支为 `codex/storage-identity`。文件工具与子 Agent 已保存在 `codex/file-tools-subagents`（`f99b7da`），未并入本次改动。
- 每个 Agent 独立 `state.db`，使用 `database/sql` 和纯 Go SQLite 驱动 modernc.org/sqlite v1.61.0。启用外键、WAL、FULL 同步和 schema 版本检查；只由持有 Agent 锁的 daemon 打开并初始化。
- 用户默认 defaultuser，会话默认 main；按 Agent / user / session 归属。首次聊天自动创建，读历史不创建。并发准入从会话名改为用户加会话名。
- 增加 `sessions list`、`sessions show`、`--user`、分页和 JSON 查询。保存用户输入、完整 assistant/tool 消息、工具调用 ID 和运行状态。
- 增加 conversation 协调层；先提交最终答案与 completed，再通知 CLI。取消/失败保留已完整记录的消息；重启将未结束运行标记 interrupted，不自动重做。
- 协议 v4 区分用户输入的 session 名称与数据库内部 session_id；新版 stop 兼容阶段 A 的 v3，便于先停止旧 daemon 再启动新版。
- 验证：vet、race tests、真实 CLI 重启与身份隔离通过；mock 与本地测试服务验证，无付费模型调用。
- 构建：本机 `make build` 和关闭 CGO 的 Linux amd64 交叉编译通过；Linux 未做运行验证。
- 范围：保存和查询历史。阶段 C 才把历史作为模型上下文，并确定失败/中断轮次如何选取、工具配对如何整理以及输入预算。

## 2026-10-24：只读工具与子任务

- 新增 list_files/read_files，固定 workspace 边界、大小限制、逐文件错误与完整 JSON 截断。
- start/serve 增加 --workspace，配置支持可选的绝对 workspace 路径，旧配置兼容。
- 新增 task.Manager 的 Start/Status/Wait；subagent 工具先固定 Start 后立即 Wait，子 Runtime 仅有时间和文件工具。
- 每父 turn 独立任务管理器；最多两个子任务，每个最多三轮模型/四次工具，子任务最长一分钟，整体仍为两分钟。
- 子任务只有显式任务说明，不继承父消息；仅最终结果进入父模型，取消时等待子任务退出。
- 验证使用 mock 和本地服务，覆盖目录越界、特殊文件、结构化输出、任务隔离/等待/取消/预算、真实 CLI 通过子 Agent 读文件。
- 还未实现跨 turn 任务查询、CLI 子任务 status/wait、异步执行、子任务持久化或会话数据库。


## 2026-10-22：阶段 A，Agent 与后台进程

- `daemon.Server` 改为 `daemon.Daemon`，文件改为 daemon.go；内部保留标准库 http.Server。
- CLI 支持可选 Agent 名称，默认 personal；start 使用独立进程和就绪管道，serve 保留前台运行。
- 每个 Agent 独立的配置、socket、进程锁、实例元数据和日志；显式启动参数不写回配置，密钥不落盘。
- status 验证进程锁和 socket；stop 根据实例 ID 请求关闭并等待释放锁，不对过期 PID 发信号。
- 协议升至 v3；Runtime 的 ReAct 循环保持不变。
- 验证：vet、race tests、真实 CLI 进程测试，覆盖双 Agent、并发启动、脱离启动进程组、聊天取消、失败启动与清理、正常关闭、崩溃恢复；均使用 mock/本地测试服务。
- `make build` 与 Linux amd64 交叉编译通过；Linux 未进行运行验证。
- 当时的后续计划：B 用户/会话身份与 SQLite，C 加载历史。当前 B 已同时完成完整消息记录、完成提交与中断标记，见上方最新记录；A 本身不创建 state.db。

## 2026-10-22：第一版工具循环

- 增加 `llm` 统一模型类型和 `tool` 注册表；每次 Run 在局部维护 messages 和调用计数，串行执行工具。
- Runtime 支持最多 6 轮模型调用、8 次工具调用、单轮 4 个工具，以及输入和输出限制；工具业务错误作为观察结果回传，取消和预算耗尽终止执行。
- 内置 `get_time`，默认 Asia/Shanghai，校验 IANA 时区；Mock `/time` 可离线演示两轮模型调用和真实本地工具。
- DeepSeek 适配器支持工具 schema、assistant/tool 消息与流式工具参数聚合；完整结束前不返回可执行的工具请求。
- 本地协议升级至 2；CLI 展示模型轮次、工具状态和耗时，进度写 stderr，`--json` 保持 NDJSON。
- 验证通过：`make check`（vet + race tests）与 `make build`；新增测试覆盖多工具配对、错误参数修正、轮次/工具预算、单工具超时、重复调用 ID、并发隔离、流式参数交错和异常断流。
- 编译后的 CLI 冒烟通过：health v2、普通问答、两轮 get_time、JSON 事件顺序、stderr 进度、四个并发 session、Ctrl+C 后复用 session，以及服务退出和 socket 清理。临时状态目录已清理。
- 本轮 DeepSeek 使用本地 HTTP 测试服务验证，没有发起真实付费模型请求；本阶段未加入跨请求历史、数据库、文件工具或后台进程管理。

> 2026-10-20 当前安排：用户已明确开始小步实现，先保留 Daemon / Runtime 边界，接入 DeepSeek 跑通单次问答。当前教程见 [真实问答调用链](learning/03-daemon-runtime-deepseek.md)。以下 M0–M6 为早期范围记录，不要求一次实现全部功能；下一小步是内存中的历史消息。

## 2026-10-20：真实模型接入

- 新增 DeepSeek 文本流适配器，以环境变量提供密钥；固定官方 API，拒绝重定向，不自动重试。
- serve 可选择 mock/deepseek、模型和输出上限；status 展示当前 Provider。
- Runtime 仍只组织一次问答，Daemon 管理连接与并发；未添加历史、持久化、工具执行和复杂配置。
- `make check`（vet + race tests）及构建通过；测试中的凭证均为虚构占位值。
- 真实调用验证通过：编译后的 CLI → Unix Socket → Daemon → Runtime → DeepSeek 返回“ROSETTA连接成功”，收到 started、文本增量和 completed，CLI 退出码为 0。只进行了这一次真实问答，输出上限 64 token。临时服务已停止，临时 state 目录已清理，密钥未落盘。

日期：2026-10-17。

## 协作约定

用户负责表达使用需求、运行体验和希望理解的问题；助手负责提出默认技术决策、实现、验证和讲解。遇到可逆的代码组织与依赖选择直接推进并记录理由；涉及真实账号、模型费用、发布、删除数据或不可推断的产品需求时，明确说明具体影响。

每个阶段交付四件东西：可运行的增量、关键设计解释、验收结果、准确的进度记录。课程以代码为线索，允许用户随时打断询问。不会要求先学完整本 Go 教材再开始，也不会一次堆完全部功能。

## 里程碑

| 阶段 | 交付物 | 验收标准 | 对应知识点 | 状态 |
|---|---|---|---|---|
| M0 设计基线 | 技术选型、职责边界、协议与路线 | 能说明一条请求经过哪些层、当前功能范围 | 模块化、依赖方向、迭代开发 | 已完成 |
| M1 通信骨架 | serve/status/chat、mock 流、取消、准入、进程锁 | 双终端可运行；异常流不误报成功；重复启动与同会话冲突被拒绝 | interface、context、HTTP、流、互斥锁、文件锁 | 已实现，验证结果见下方 |
| M2 会话与配置 | SQLite schema/migration、session create/list/history、工作区和模型默认值 | 重启保留消息；失败/中断可识别；同会话提交有序 | SQL 事务、持久化、状态机、配置快照 | 下一阶段 |
| M3 真实模型与只读工具 | Responses 适配、结构化消息、read_file/list_files、工具循环 | 模型读取受限工作区文件后回答；工具结果配对；请求可取消；轮数与预算有上限 | SDK 适配、工具协议、Turn/Round、输入输出预算 | 待实现 |
| M4 受控修改 | 文件补丁、审批交互、受限 exec、执行记录 | 拒绝审批不产生操作；冲突不覆盖；取消清理子进程；明确不确定执行状态 | 副作用、授权、乐观并发检查、进程生命周期 | 待实现；完成后为 v0.1 |
| M5 日常可用性 | 上下文压缩、恢复交互、后台 start/stop、配置检查 | 长会话可继续；残留状态可诊断；重启与停止行为明确 | 摘要预算、恢复、进程身份、可观测性 | 待实现 |
| M6 选择性扩展 | 按实际需求逐个添加 MCP、Skills、子任务或界面 | 每次扩展都复用核心，附带权限与生命周期验收 | 协议适配、资源管理、扩展边界 | 后置 |

Checkpoint/Rewind 需在文件修改记录可靠后独立设计；长期记忆需在真实使用中确认需求后再做。不会为了对齐参考文档目录而全部实现。

## 第一轮阅读与实践

先读 [第一课](learning/01-first-request.md)，运行两个终端，再看 `--json` 输出。当前只需掌握三个问题：

1. CLI 为什么不直接调用模型？因为服务要统一持有会话、调度和运行时，后续多个入口可以复用。
2. Provider 接口解决什么问题？调用者不需要知道具体模型服务如何工作，测试也能替换实现。
3. Ctrl+C 为什么能停止？取消信号必须从 CLI 请求一路传到服务和 Provider，并由执行代码主动响应。

理解这些后进入 M2，不需要先理解热重载或子 Agent。

## M1 验收记录

代码包含以下验证场景，最终执行结果在本次工作完成时更新：

- Runtime：部分输出后 Provider 失败，不能再发送完成事件；消费者失败能向上返回。
- HTTP：拒绝非法消息、未知字段、多个 JSON 对象和过大输入。
- 并发：同 session 拒绝第二个 turn；全局额度生效；取消后释放准入记录。
- 客户端：检查事件身份和序号；没有终态的断流返回错误。
- 生命周期：同 state 重复启动失败；退出后可重启；残留 socket 可恢复；普通文件不会被误删。
- 真实进程冒烟：构建二进制、启动服务、检查状态、发送普通和 JSON 消息、取消与退出。

测试不依赖真实 AI；因此它们不证明模型工具调用的正确性，那属于 M3 的验收范围。
