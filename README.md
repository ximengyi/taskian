# Taskian

Taskian 是一个跨平台、本地优先的双向 AI 任务调度器。它让你通过微信控制本机或服务器上的 Codex、Cursor 等编程 Agent，并在 Agent 需要补充信息时把问题发回微信，收到回答后恢复同一个原生会话继续执行。

```text
微信 ⇄ 腾讯 iLink ⇄ Taskian ⇄ Codex / Cursor ⇄ 代码仓库
```

Taskian 不在用户和编程 Agent 之间增加第二个大模型。消息解析、权限检查、排队、去重和会话路由均使用确定性程序逻辑；模型用量来自实际执行任务的 Codex、Cursor 或其他 Agent。

## 0.4.2 能力

- 无参数运行进入首次启动向导：自动生成个人配置、探测 Agent、打印 iLink 二维码并选择后台运行。
- Windows 双击即可初始化，后台使用当前用户任务计划程序；Linux 使用 systemd user service。
- 项目既可直接使用绝对目录，也可注册成简短名称并设为当前项目。
- 微信发送 `help`、`#help` 或 `帮助`即可查看任务和项目管理命令。
- `#task` 使用默认 Agent；`#codex`、`#cursor` 可随任务切换 Agent。
- 个人模式可直接发送普通任务文字；未选择项目时，默认 Agent 在当前用户主目录工作。
- “帮我关一下机”等系统操作必须通过一次性确认码二次确认，避免误触。
- Windows CMD 与日志文件显示收到的消息、任务路由、Agent 启动、实时输出以及完整失败原因。
- 个人模式无需预先配置项目或发送者白名单；iLink 扫码身份是默认授权边界。

- `serve` 启动时自动预检 Agent 登录状态和项目目录，不再要求手动运行 `check`。
- 默认每 5 分钟健康检查；故障与恢复通过 iLink 提醒，不重复刷屏。
- `agents detect` 自动查找 PATH 和常见安装目录中的 Codex/Cursor CLI。
- 配置命令失效时自动采用探测到的同类型 CLI；一个 Agent 故障不影响其他 Agent。

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

2. 安装 Taskian：

   ```sh
   install -Dm755 taskian-linux-amd64 ~/.local/bin/taskian
   ```

3. 初始化、扫码并安装后台服务：

   ```sh
   taskian
   ```

   终端会显示二维码，同时在 `~/.taskian/ilink-login-qr.png` 保存高清备用图片。登录凭据写入 `~/.taskian/ilink.json`。

   直接回车选择 `Y` 后，Taskian 会安装并启动 systemd user service。Rocky Linux 用户如需退出 SSH 后继续运行，应按提示启用 linger。

仓库提供 [`deploy/taskian.service`](deploy/taskian.service) 作为 systemd user service 示例。

## Windows 与 macOS

Windows 可以直接双击 `taskian-windows-amd64.exe`。程序会自动生成配置、显示二维码，并自动打开高清二维码图片供微信扫描；选择默认的 `Y` 后，确认后台启动成功即可关闭窗口。macOS 根据芯片选择对应程序并在终端运行。

```text
taskian-windows-amd64.exe
```

## 微信命令

个人模式下可以直接发送自然语言任务，例如：

```text
帮我整理一下当前目录的 README
```

没有通过 `#use` 选择项目时，Taskian 使用默认 Agent，并以运行 Taskian 的用户主目录作为“全局”工作目录。

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

关机和重启属于内置高风险系统操作，不会直接交给 Agent。发送“帮我关一下机”后，Taskian 会返回一次性确认码；在 2 分钟内回复：

```text
#confirm A1B2C3
```

确认后 Windows 延迟 30 秒执行，Linux 延迟 1 分钟执行。回复 `#confirm cancel` 可取消尚未确认的操作。

注册和使用项目：

```text
#project add week-report D:\cursorwork\week-report
#project list
#use week-report
#cursor 写一下本周周报
#task week-report 写一下本周周报
```

也可以在任务中直接填写绝对目录。`#task` 使用默认 Agent，`#codex` 和 `#cursor` 明确选择 Agent。

当用户只有一个等待回答的任务时，可以直接发送普通文本作为回答，也可以省略 `#reply` 后的任务号；有多个等待任务时必须使用完整的 `#reply <任务号> <回答>`。

## 上下文与记忆

Taskian 保存任务状态、用户可见的提问/回答和 Agent 会话 ID，但不复制 Agent 的隐藏推理或完整内部 transcript。

```text
Taskian SQLite：任务、项目别名、会话偏好、消息路由、thread_id/session_id、状态
Codex/Cursor：完整 Agent 会话、代码读取和工具上下文
```

恢复时 Taskian 使用保存的 ID 调用 Codex `resume` 或 Cursor `--resume`。如果用户删除了 Agent 本地会话数据，Taskian 会明确报告无法恢复，不会静默创建新会话。

## 配置

最小 iLink 配置结构：

```json
{
  "mode": "personal",
  "default_agent": "codex",
  "database_path": "~/.taskian/taskian.db",
  "channel": {
    "type": "ilink",
    "state_path": "~/.taskian/ilink.json"
  },
  "health": {
    "enabled": true,
    "interval": "5m"
  }
}
```

常用字段：

- `database_path`：SQLite 状态库路径。
- `mode`：默认 `personal`；旧版带项目配置时自动保持 `controlled` 受控模式。
- `default_agent`：未明确指定 Agent 时使用的默认值。
- `channel.type`：`ilink` 或 `wechatian-files`。
- `channel.allowed_senders`：可选发送者白名单；未配置时只允许扫码绑定用户。
- `max_concurrent_tasks`：最大并发 Agent 数，默认 2。
- `waiting_user_timeout`：等待微信回答的最长时间，默认 72 小时。
- `health.enabled`：是否定时检查，默认开启；启动预检不受此项影响。
- `health.interval`：健康检查间隔，默认 5 分钟。
- `health.notify_senders`：可选提醒接收者；iLink 默认使用扫码绑定用户。
- `agents`、`projects`：个人模式可以省略；受控模式用于限制 Agent、项目和允许组合。

Cursor 默认不会自动添加 `--force`；只有明确设置 `"force": true` 时才启用。Codex 默认使用 `workspace-write` 沙箱。

## 运行日志

Windows 前台窗口会显示以下处理过程：收到微信消息、任务号、Agent、项目目录、Agent 实时输出、等待回答、完成或详细错误。后台服务会把相同内容同时写入：

```text
%USERPROFILE%\.taskian\logs\taskian.log
```

日志超过 5 MiB 后会在下次启动时轮换为 `taskian.log.1`。日志会包含用户主动发送的任务文字，便于本机排错，但不会打印 iLink 登录令牌或 Agent 环境变量。

## 通用 Agent

0.2 正式保证 Codex 和 Cursor 的双向恢复。其他 CLI 可以通过 `generic` 类型继续执行单次任务；若需要双向会话，需要配置稳定的 `resume_args`，或增加实现 `Start`、`Resume`、`Cancel`、`Events`、`HealthCheck` 的专用适配器。

## 0.1 文件通道兼容

需要继续使用 Obsidian/Wechatian 时，可参考 [`config.wechatian-files.example.json`](config.wechatian-files.example.json)。Taskian 首次打开 0.2 SQLite 状态库时会导入 0.1 `state.json` 的消息去重记录，并默认跳过已有历史消息。

## 本地命令

```text
taskian serve                 持续运行
taskian init                  初始化、扫码并选择后台运行
taskian once                  接收一轮消息
taskian check                 手动检查配置、Agent 登录状态和项目
taskian agents detect         自动探测本机 Codex/Cursor CLI
taskian service install       安装后台服务
taskian service start         启动后台服务
taskian service stop          停止后台服务
taskian service restart       重启后台服务
taskian service status        查看后台状态
taskian service logs          查看后台日志
taskian service uninstall     移除后台服务（保留数据）
taskian status                查看本地任务统计
taskian ilink login           扫码登录
taskian ilink status          查看绑定状态
taskian ilink logout          清除绑定
taskian example-config        输出示例配置
taskian version               显示版本
```

## 安全边界

- iLink 个人模式只接受扫码绑定身份；受控模式可额外限制发送者、项目和 Agent。
- 个人模式允许绝对项目目录，但微信消息不能指定任意可执行程序。
- Taskian 直接启动进程，不通过 shell 解释任务正文。
- `#reply` 只回答普通问题，不能扩大沙箱或系统权限。
- 不自动批准提交、推送、部署或高风险删除。
- iLink Token 不写入日志；状态目录和凭据文件使用仅当前用户权限。
- 同一 iLink 身份只运行一个 Taskian 实例。

Taskian 能限制入口和调度权限，但 Agent 最终能做什么仍取决于 Agent 自身配置与操作系统权限。

## 当前限制

- iLink 当前只处理文本和语音转写文本；图片、文件、视频和原始语音不会传给 Agent。
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
- [0.3 需求与验收标准](docs/0.3.md)
- [0.4 规划与验收标准](docs/0.4.md)
- [产品定位与优势](docs/product-advantages.md)
- [版本文档约定](docs/README.md)

## License

Taskian 使用 [MIT License](LICENSE)。iLink 协议实现参考的上游项目信息见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
