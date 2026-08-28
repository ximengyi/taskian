# Taskian

Taskian 是一个跨平台、本地优先的双向 AI 任务调度器。它让你通过微信控制本机或服务器上的 Codex、Cursor 等编程 Agent，并在 Agent 需要补充信息时把问题发回微信，收到回答后恢复同一个原生会话继续执行。

```text
微信 ⇄ 腾讯 iLink ⇄ Taskian ⇄ Codex / Cursor ⇄ 代码仓库
```

Taskian 不在用户和编程 Agent 之间增加第二个大模型。消息解析、权限检查、排队、去重和会话路由均使用确定性程序逻辑；模型用量来自实际执行任务的 Codex、Cursor 或其他 Agent。

## 0.2 能力

- 在终端打印二维码，直接登录腾讯 iLink，不依赖 Obsidian 或图形界面。
- 支持微信文本消息长轮询、上下文 token、消息去重、断线恢复和长消息分段。
- Codex 使用原生 `thread_id` 恢复会话。
- Cursor 使用原生 `session_id` 恢复会话。
- Agent 可返回问题；用户通过 `#reply` 回答后继续原任务。
- SQLite 持久化任务、用户可见消息、状态变化和通道 cursor。
- Taskian 重启后，等待回答的任务仍可继续。
- 多个任务同时等待时必须指定任务号，避免回答被送错项目。
- Agent、项目和发送者白名单；正文不经过 shell。
- 支持 Windows amd64、Linux amd64、macOS amd64/arm64。
- 保留 0.1 的 Wechatian/Obsidian 文件通道。

## 快速开始：Rocky Linux / Linux

1. 安装并登录需要使用的 Agent：

   ```sh
   codex --version
   agent status
   ```

2. 安装 Taskian 并准备配置：

   ```sh
   install -Dm755 taskian-linux-amd64 ~/.local/bin/taskian
   mkdir -p ~/.taskian
   cp config.linux.example.json ~/.taskian/config.json
   ```

3. 修改 `~/.taskian/config.json` 中的项目路径和允许的 Agent。

4. 在纯命令行终端完成微信扫码绑定：

   ```sh
   taskian ilink login
   ```

   终端会显示二维码，同时在 `~/.taskian/ilink-login-qr.png` 保存备用图片。登录凭据写入 `~/.taskian/ilink.json`。

5. 检查并启动：

   ```sh
   taskian check
   taskian serve
   ```

仓库提供 [`deploy/taskian.service`](deploy/taskian.service) 作为 systemd user service 示例。

## Windows 与 macOS

Windows 使用 `taskian-windows-amd64.exe` 和 [`config.windows.example.json`](config.windows.example.json)，macOS 根据芯片选择 `taskian-darwin-arm64` 或 `taskian-darwin-amd64` 并使用 [`config.macos.example.json`](config.macos.example.json)。配置完成后同样运行：

```text
taskian ilink login
taskian check
taskian serve
```

## 微信命令

创建 Codex 任务：

```text
#codex yuanze
检查登录页并运行测试。如果需要决定交互方案，先问我。
```

选择 Cursor：

```text
#taskian cursor yuanze
修复移动端布局。如果有多个可行方案，先问我。
```

Agent 提问时，Taskian 返回类似：

```text
❓ [T-12AB34CD] codex 等待你的回答
登录方式要保留密码登录，还是只保留验证码？

回复：#reply T-12AB34CD <你的回答>
```

回答并恢复原会话：

```text
#reply T-12AB34CD 两种都保留，验证码作为默认方式。
```

其他命令：

- `#status`：查看当前任务。
- `#status T-12AB34CD`：查看指定任务。
- `#cancel T-12AB34CD`：取消任务和本地 Agent 进程。
- `#help`：显示帮助。

当用户只有一个等待回答的任务时，可以直接发送普通文本作为回答，也可以省略 `#reply` 后的任务号；有多个等待任务时必须使用完整的 `#reply <任务号> <回答>`。

## 上下文与记忆

Taskian 保存任务状态、用户可见的提问/回答和 Agent 会话 ID，但不复制 Agent 的隐藏推理或完整内部 transcript。

```text
Taskian SQLite：任务、消息路由、thread_id/session_id、状态
Codex/Cursor：完整 Agent 会话、代码读取和工具上下文
```

恢复时 Taskian 使用保存的 ID 调用 Codex `resume` 或 Cursor `--resume`。如果用户删除了 Agent 本地会话数据，Taskian 会明确报告无法恢复，不会静默创建新会话。

## 配置

最小 iLink 配置结构：

```json
{
  "database_path": "~/.taskian/taskian.db",
  "channel": {
    "type": "ilink",
    "state_path": "~/.taskian/ilink.json"
  },
  "agents": {
    "codex": {
      "type": "codex",
      "command": "codex",
      "sandbox": "workspace-write"
    },
    "cursor": {
      "type": "cursor",
      "command": "agent"
    }
  },
  "projects": {
    "yuanze": {
      "path": "/srv/code/yuanze",
      "allowed_agents": ["codex", "cursor"]
    }
  }
}
```

常用字段：

- `database_path`：SQLite 状态库路径。
- `channel.type`：`ilink` 或 `wechatian-files`。
- `channel.allowed_senders`：可选发送者白名单；未配置时只允许扫码绑定用户。
- `max_concurrent_tasks`：最大并发 Agent 数，默认 2。
- `waiting_user_timeout`：等待微信回答的最长时间，默认 72 小时。
- `agents.<name>.type`：`codex`、`cursor` 或 `generic`。
- `projects.<name>.allowed_agents`：项目允许使用的 Agent。

Cursor 默认不会自动添加 `--force`；只有明确设置 `"force": true` 时才启用。Codex 默认使用 `workspace-write` 沙箱。

## 通用 Agent

0.2 正式保证 Codex 和 Cursor 的双向恢复。其他 CLI 可以通过 `generic` 类型继续执行单次任务；若需要双向会话，需要配置稳定的 `resume_args`，或增加实现 `Start`、`Resume`、`Cancel`、`Events`、`HealthCheck` 的专用适配器。

## 0.1 文件通道兼容

需要继续使用 Obsidian/Wechatian 时，可参考 [`config.wechatian-files.example.json`](config.wechatian-files.example.json)。Taskian 首次打开 0.2 SQLite 状态库时会导入 0.1 `state.json` 的消息去重记录，并默认跳过已有历史消息。

## 本地命令

```text
taskian serve                 持续运行
taskian once                  接收一轮消息
taskian check                 检查配置、Agent 和项目
taskian status                查看本地任务统计
taskian ilink login           扫码登录
taskian ilink status          查看绑定状态
taskian ilink logout          清除绑定
taskian example-config        输出示例配置
taskian version               显示版本
```

## 安全边界

- 只允许配置中的发送者、项目和 Agent。
- 微信消息不能提供任意工作目录或可执行程序。
- Taskian 直接启动进程，不通过 shell 解释任务正文。
- `#reply` 只回答普通问题，不能扩大沙箱或系统权限。
- 不自动批准提交、推送、部署或高风险删除。
- iLink Token 不写入日志；状态目录和凭据文件使用仅当前用户权限。
- 同一 iLink 身份只运行一个 Taskian 实例。

Taskian 能限制入口和调度权限，但 Agent 最终能做什么仍取决于 Agent 自身配置与操作系统权限。

## 当前限制

- iLink 0.2 只处理文本和语音转写文本；图片、文件、视频和原始语音不会传给 Agent。
- 腾讯 iLink 接口和服务规则可能变化，升级前应阅读 Release Note。
- Taskian 崩溃时无法安全接管仍在运行的孤儿 Agent 进程；任务会标记为 `resume_failed`，不会重复原始操作。
- 原生会话只能在保存该会话的同一系统用户和 Agent 数据目录中恢复。

## 开发与构建

```text
go test ./...
go vet ./...
```

Windows：

```powershell
.\scripts\build.ps1
```

Linux/macOS：

```sh
./scripts/build.sh
```

Tag 推送后，GitHub Actions 自动测试并构建 Windows amd64、Linux amd64、macOS amd64/arm64，然后使用对应的 [`docs/release-notes`](docs/release-notes) 文档创建 GitHub Release。

## 文档

- [0.2 需求与验收标准](docs/0.2.md)
- [产品定位与优势](docs/product-advantages.md)
- [版本文档约定](docs/README.md)

## License

Taskian 使用 [MIT License](LICENSE)。iLink 协议实现参考的上游项目信息见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
