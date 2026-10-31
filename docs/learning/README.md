# ROSETTA 学习路线

当前项目方向：通用个人助手。2026-10-20 起进入小步实现阶段，保留 Daemon / Runtime 边界。

2026-10-22 工具循环：[从单次回答到 ReAct 工具循环](04-react-loop.md)。先用 mock 的 `/time` 跑通真实本地工具，再观察 DeepSeek 工具请求。配套 [设计说明](../design/react-loop.md) 记录职责、消息配对和停止条件。

阶段 B 的入口为 [存储与身份](07-storage-and-identity.md)，介绍 SQLite、用户/会话隔离与历史查询。后台启停与目录基础见 [第五课](05-agent-daemon-lifecycle.md)。文件工具和子任务见 [第六课](06-file-tools-and-subagents.md)。第四课讲解沿用的 ReAct 循环；[Daemon / Runtime 与第一次 DeepSeek 问答](03-daemon-runtime-deepseek.md) 是上一版本的基础链路。下面的长讲义仍可用作参考，其中完整配置设计尚未实现。

2026-10-29 阶段 C：[第八课：让助手记住前面的对话](08-session-context.md)。阶段 C 已将近期成功对话加入模型上下文，包含完整工具消息，并按整次请求裁剪历史。

2026-10-30：文件工具与子 Agent 已整合到 main。新增 [第九课：合并分支](09-merging-branches.md)，用本次真实提交关系解释领先/落后、冲突与合并。

## 当前主课

- [身份、配置与会话边界：Markdown](02-identity-and-configuration.md)
- [身份、配置与会话边界：单文件离线 HTML](02-identity-and-configuration.html)

该讲义是 2026-10-18 开始的设计课程第一步。文件名中的 02 只是保留已有文件顺序，不代表必须先阅读旧骨架课程。正文、例子和练习答案均已包含，外部链接仅作补充。

HTML 可在本机浏览器直接打开，不需要服务端、联网、外部字体或脚本。可以复制这个 HTML 文件到另一台设备离线阅读；使用浏览器打印时也有打印样式。

## 早期原型的补充阅读

- [跑通一次请求](01-first-request.md)：解释已有模拟通信骨架，不是本轮学习的前置要求。

旧的架构文档和开发路线保留了早期编码助手方向的部分决策与里程碑。本课明确以通用个人助手为目标，配置方案属于待讨论的设计草案，不表示代码已经实现。
