# 用这次合并理解 Git 分支

日期：2026-10-30 至 31。本次把 `codex/file-tools-subagents` 合入 `main`。

## 先读懂真实关系

合并前，提交关系是：

```text
                      B            C             .env
6165fd5 ─────────── 7e89f76 ──── 8877234 ──── 4277e74   main
   └─────────────── f99b7da                              工具分支
```

`6165fd5` 是共同祖先，也就是两边最后共同拥有的版本。main 在它后面增加了 B、C、.env 三个提交；工具分支在它后面增加了一个提交。

所以工具分支是 **落后 main 三个提交，同时领先一个提交**。这两个数字可以同时成立，因为它们分别数“对方有、我没有”的提交。

“只有工具分支尚有独有改动”这个判断是对的。B 分支虽然停在旧位置，但它的改动已包含在 main 中，不需要再次合并。合并前本地 C 分支已经与 main 同为 4277e74，远端 C 分支仍停在 8877234；这只是分支指针的位置不同。

分支本身可以理解为贴在某个提交上的名字。提交记录文件快照和父提交；Git 沿父提交关系判断谁包含了谁，不靠提交日期判断新旧。

## 三种情况，用的操作不同

| 当前关系 | 合入 main 时会怎样 |
|---|---|
| 工具分支只是 main 的旧版本，没有独有提交 | 已经被 main 包含，merge 会提示 Already up to date |
| main 是工具分支的祖先，工具分支独自往前走 | 可以快进，只把 main 指针移到工具分支位置 |
| 两边从共同祖先各自往前走 | 合并双方相对共同祖先的改动，可能需要解决冲突 |

本次属于第三种。完成后出现一个同时连接两边的新提交 M：

```text
6165fd5 ── B ── C ── .env ────── M   main
   └────────── 工具提交 ──────────┘
```

M 有两个父提交，保存整合后的代码。原来的 B、C、工具提交都保留，没有改写历史。

## Git 怎么合并，为什么会冲突

Git 比较三个版本：共同祖先、main 当前版本、工具分支版本。它先看两边各自改了什么，再尝试组合。

不同位置的改动通常能自动组合。如果双方都改了同一段，Git 不能判断程序应当怎样运行，就留下冲突标记，要求我们决定最终内容。

这不是“拿较新的分支覆盖旧分支”。main 更晚加入的历史记录，与工具分支更早写的子任务，都是需要保留的功能。

本次有六个文件发生文本冲突：README、CLI 入口、Runtime、状态目录定义、开发路线、学习路线。

| 位置 | 最终怎样整合 |
|---|---|
| Runtime | 保留历史加载接口和完整消息回调；把子任务管理器接到 RunWithHistory，让实际聊天入口也能使用 subagent |
| CLI | 同时解析并传递 --env-file 和 --workspace；保留用户、会话与历史查询命令 |
| 状态目录 | 同时保留 Database 和 Workspace，分别保存会话数据与提供文件工具读取目录 |
| Mock 与测试 | 同时保留 /recall、文件读取、任务委派与 .env 测试 |
| 文档 | 当前说明更新为已整合；旧课程保留日期和补充说明 |

子任务接收明确的任务说明，不自动继承父会话历史。它的中间消息不直接进入父会话数据库；父会话保存 subagent 调用及返回结果。下次请求可以把这些作为历史读入，但不会重新执行子任务。

文本冲突消失并不等于功能正确。因此增加了一个跨模块测试，走真实的 Conversation → RunWithHistory 路径，验证父历史、子任务隔离、结果记录和下次加载能够一起工作。

## 平时可以使用的命令

先看状态、更新远端信息、查看提交关系：

```sh
git status
git fetch origin
git branch -vv
git log --all --graph --oneline --decorate
git rev-list --left-right --count main...codex/file-tools-subagents
```

最后一个命令在合并前输出 `3 1`：左边是 main 独有提交数，右边是工具分支独有提交数。注意 `branch -vv` 的 ahead/behind 默认比较的是该分支自己的远端跟踪分支，不一定是 main。

通用合并流程如下。执行前应提交或妥善保存自己的工作区改动：

```sh
git switch main
git merge --ff-only origin/main
git merge --no-ff --no-commit codex/file-tools-subagents

# 若有冲突，编辑文件，保留最终正确内容并移除冲突标记
git status
git diff --name-only --diff-filter=U
git add <已经解决冲突的文件路径>

make check
git diff --cached --check
git commit -m "Merge file tools and subagents into main"
git push origin main
```

本次 main 开始时已与 origin/main 一致，不需要先快进。使用 `--no-commit` 是为了在最终提交之前完成检查；`--no-ff` 明确要求保留合并节点。只有测试通过、冲突全部处理完，才创建合并提交。

如果合并尚未提交，决定放弃这次尝试，可使用 `git merge --abort`。不要在有未保存修改时随意尝试破坏性恢复命令。

只想把最新 main 同步到工具分支，则方向相反：先 `git switch codex/file-tools-subagents`，再 `git merge main`。**merge 的参数是来源，当前所在分支是目标。** 本次直接把工具合入 main，无须先做这一步；解决一次冲突即可。

本机 Git/Go 工具链使用 `DEVELOPER_DIR=/Library/Developer/CommandLineTools` 环境设置执行上述命令，以避开当前 Xcode 工具链的许可提示。这是本机环境设置，与 Git 合并机制无关。

## 合并之后怎么确认

`git branch --merged main` 可以列出已包含在 main 中的本地分支。工具分支指针仍停在原来的 f99b7da 是正常现象；Git 不会因为它被合并，就自动移动或删除这个名字。

此时再次比较 main 与工具分支，工具分支右侧独有提交数应为 0，说明它的全部历史已进入 main。以后想继续开发，可以从最新 main 新建分支；旧分支是否删除可以另行决定。

数据库 schema 与本地协议仍沿用现有版本。重新编译、重启 daemon 后，文件工具、子任务、会话持久化与历史输入可以同时使用。验证使用 mock 与本地测试服务，不调用真实模型。
