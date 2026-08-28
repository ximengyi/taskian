# Taskian 产品定位与优势

本文档用于统一 Taskian 的产品定位、对外介绍和后续版本决策。具体版本交付范围仍以对应的 `docs/<版本号>.md` 为准。

## 一句话定位

> Taskian 是连接微信与本地编程 Agent 的轻量、安全、可恢复的双向任务调度器，不在用户和 Agent 之间增加第二个大模型。

## 核心价值

Taskian 解决的不是“再造一个 AI”，而是让用户能够离开电脑后，继续安全地控制已经安装和登录的 Codex、Cursor 等编程 Agent：

```text
用户微信 ⇄ Taskian ⇄ Codex / Cursor / 其他 Agent ⇄ 代码仓库
```

Taskian 负责：

- 消息通道接入；
- 命令识别和权限检查；
- 项目与 Agent 白名单；
- 任务排队和并发控制；
- Agent 会话 ID 的保存与恢复；
- Agent 提问与用户回答的双向路由；
- 消息去重、故障恢复、状态查询和审计日志。

Taskian 不负责替代 Agent 进行推理、规划或编写代码。

## 1. 不增加第二层大模型

Taskian 的调度核心使用确定性程序逻辑，不需要配置额外的大模型 API Key，也不会为了理解每条微信消息而调用一个“中间模型”。

例如：

```text
#codex yuanze
修复登录页并运行测试。
```

执行链路是：

```text
微信消息
  ↓
Taskian 解析命令、检查权限、选择项目
  ↓
Codex 执行任务
  ↓
Taskian 转发结果
  ↓
微信
```

其中 Taskian 的收发、解析、排队、去重和状态查询不产生大模型 Token。真正执行任务的 Codex、Cursor 或其他 Agent 仍会消耗其自身的 Token、订阅额度或本地算力。

因此准确表述是：

> Taskian 自身不产生额外的大模型调用，整体成本接近直接使用目标 Agent 的成本。

不能宣传为“使用 Taskian 后整个任务完全不消耗 Token”。

## 2. 成本边界清晰、可预测

Taskian 的典型用量关系是：

```text
Taskian 总模型成本 ≈ 被调用 Agent 自身的模型成本
```

以下操作本身不需要模型调用：

- 接收或发送 iLink 消息；
- `#help`、`#status` 和任务列表；
- 项目、发送者与 Agent 白名单检查；
- 消息去重、任务排队和状态持久化；
- 将 Agent 问题转发给用户；
- 将用户的 `#reply` 路由到指定 Agent 会话。

以下操作会产生下游 Agent 用量：

- 创建 Codex、Cursor 或其他 Agent 任务；
- 使用 `#reply` 恢复 Agent 会话并让它继续推理；
- Agent 内部读取代码、调用工具、运行多轮推理或生成总结。

如果下游 Agent 使用 API Key，费用通常由其模型供应商按量计算；如果使用订阅登录，则消耗对应套餐额度；如果使用本地模型，则主要消耗本地硬件和电力。

## 3. 专注编程 Agent，而不是通用私人助理

Taskian 采用窄而明确的职责：远程调度代码任务。它不默认处理普通聊天，也不主动把所有消息交给模型。

这种设计带来的优势包括：

- 行为更容易预测和测试；
- 无效聊天不会触发昂贵的 Agent 执行；
- 更容易限定可以访问的代码仓库；
- 更容易审计“谁在什么时候让哪个 Agent 操作了哪个项目”；
- 出现故障时，可以明确区分通道、调度器与 Agent 的责任边界；
- 核心程序更小，适合无图形界面的 Linux 服务器长期运行。

## 4. 直接复用用户已有的 Agent 能力

用户可以继续使用已经安装、登录和配置好的 Codex、Cursor 等工具，包括它们自身的：

- 模型与账号体系；
- 代码理解和编辑能力；
- 沙箱与审批机制；
- Git、测试和终端工具；
- 会话历史与恢复能力。

Taskian 不复制这些能力，只通过标准 Agent 适配器调用它们。这可以减少重复实现，也避免为了远程控制而重新维护一套模型、工具和提示词系统。

## 5. 真正的双向会话

Taskian 不只是“发送任务并等待最终结果”。当 Agent 需要补充信息时：

```text
Agent 提问
  ↓
Taskian 保存 Agent 会话 ID，并把任务设为 waiting_user
  ↓
问题发送到微信
  ↓
用户回复 #reply <任务号> <回答>
  ↓
Taskian 恢复同一个 Agent 会话
```

这种方式直接使用 Agent 原生会话，而不是把历史文本重新拼接成一个新任务，因此能保留 Agent 已读取的代码、已经执行的工具和此前的推理上下文。

## 6. 安全边界比自然语言路由更明确

Taskian 使用确定性的本地规则决定消息如何路由：

- 个人模式既支持明确的 `#codex`、`#cursor` 等路由命令，也支持将普通文本交给默认 Agent；
- 关机、重启等高风险内置操作必须使用短时有效的一次性确认码，不会仅凭一条自然语言消息立即执行；
- 个人模式允许使用项目别名或绝对目录，但仍受运行 Taskian 的操作系统用户权限约束；
- 微信消息只能选择本机已配置或已探测到的 Agent，不能指定任意可执行程序；
- `#reply` 只用于回答问题，不能借此扩大沙箱或系统权限；
- Taskian 不自动批准提交、推送、部署或高风险删除；
- Token、Cookie 和登录凭据不得写入微信回复或普通日志。

这不能消除 Agent 操作代码的固有风险，但可以减少由开放式自然语言入口带来的额外权限扩张。

## 7. 适合纯命令行和私有环境

Taskian 以跨平台单文件程序为目标，可在 Windows、Linux 和 macOS 上运行。腾讯 iLink 直连完成后，Rocky Linux amd64 等无桌面服务器不再依赖 Obsidian GUI。

代码仓库、Agent 登录凭据、任务状态和执行过程都可以保留在用户自己的机器上。除微信/iLink 和下游 Agent 本身需要的网络通信外，Taskian 不要求额外的 Taskian 云服务。

## 8. 可扩展但不让核心变复杂

Taskian 通过两个方向扩展：

- **通道适配器**：iLink、Wechatian 文件通道，以及未来可能增加的其他消息通道；
- **Agent 适配器**：Codex、Cursor、通用 CLI，以及未来的 Qwen、Pi、Kimi Code 等。

新 Agent 只需要实现启动、恢复、取消、事件输出和健康检查，不需要修改消息通道、任务状态机和权限核心。新 Agent 是否能实现完整双向交互，取决于其是否提供稳定的会话 ID、结构化输出和恢复会话能力。

## 与 OpenClaw 的定位差异

Taskian 和 OpenClaw 都可以把聊天通道连接到 Agent，但产品目标不同：

| 对比项 | Taskian | OpenClaw |
| --- | --- | --- |
| 核心定位 | 编程 Agent 的远程任务调度器 | 通用 AI Agent 平台与消息网关 |
| 是否内置 Agent 推理运行时 | 否 | 是 |
| 每条普通消息是否需要模型理解 | 默认不需要 | 通常需要由 Agent 处理 |
| 上下文与记忆 | 保存任务状态和外部 Agent 会话 ID | 管理自己的提示词、对话、记忆、技能和工具上下文 |
| 模型成本 | 不增加中间模型，成本主要来自目标 Agent | 主 Agent、工具循环、记忆或定时 Agent 回合都可能产生用量 |
| 主要使用场景 | 从微信安全控制 Codex/Cursor 完成代码任务 | 跨渠道的通用私人助理、多工具和多 Agent 自动化 |
| 部署复杂度 | 目标是小型单文件服务 | 功能更完整，相应配置和运行组件更多 |

这不是简单的“谁更好”，而是适用范围不同：

- 需要完整私人 AI 助理、自然语言路由、记忆、技能和多渠道生态时，OpenClaw 的能力更全面；
- 只希望通过微信可靠地控制现有编程 Agent，并尽量降低额外模型调用、权限面和部署复杂度时，Taskian 更聚焦。

OpenClaw 也可以使用便宜模型、订阅模型或本地模型，因此不能笼统表述为“OpenClaw 一定产生巨额费用”。更准确的差异是：OpenClaw 自己拥有模型驱动的 Agent 运行时，而 Taskian 不增加这一层。

参考资料：

- [OpenClaw Agent Runtime](https://docs.openclaw.ai/concepts/agent)
- [OpenClaw Context](https://docs.openclaw.ai/concepts/context)
- [OpenClaw Token use and costs](https://github.com/openclaw/openclaw/blob/main/docs/reference/token-use.md)
- [OpenClaw Heartbeat](https://docs.openclaw.ai/gateway/heartbeat)

## 对外表述建议

推荐使用：

> Taskian 不在用户和编程 Agent 之间增加第二个大模型。它使用确定性规则完成消息接入、权限检查、任务调度和会话恢复，让远程使用 Codex、Cursor 等 Agent 的成本更清晰、行为更可控。

避免使用：

- “Taskian 完全不消耗 Token”；
- “使用 Taskian 做任何任务都是免费的”；
- “OpenClaw 一定会产生巨额费用”；
- “Taskian 可以消除 Agent 的所有安全风险”。

## 产品原则

后续版本设计应持续遵守：

1. 能用确定性程序完成的事情，不增加大模型调用。
2. 让目标 Agent 负责代码推理，Taskian 负责连接、权限、状态和恢复。
3. 新功能必须说明是否会增加模型调用、网络服务或权限范围。
4. 默认拒绝不明确的任务路由和权限提升。
5. 保持无图形界面 Linux 环境可运行。
6. 产品宣传必须区分“Taskian 自身用量”和“下游 Agent 用量”。
