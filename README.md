# Taskian

Taskian 是一个跨平台、本地优先的 AI 任务调度器。它监听 Obsidian 中由
[Wechatian](https://github.com/laruence/wechatian) 写入的微信消息，将明确标记的任务交给
Codex 等命令行 agent，然后把执行结果写回 `Wechatian/outbox`，由 Wechatian 发回微信。

> 当前稳定版本为 0.1。腾讯 iLink 直连、Codex/Cursor 双向会话正在
> [0.2 版本需求](docs/0.2.md)中规划。

```text
微信 → Wechatian 每日笔记 → Taskian → Codex → Wechatian/outbox → 微信
```

## 当前能力

- Windows、Linux 与 macOS 使用同一套 Go 代码，可构建为无运行时依赖的单文件程序。
- 只读取每日笔记中标记为“接收”的引用块。
- 支持 `#codex <项目>` 和 `#taskian <agent> <项目>` 两种命令格式。
- agent 和项目均须出现在配置白名单中。
- 直接启动命令，不经过 shell，微信正文不会成为 shell 命令。
- Codex 默认使用 `workspace-write` 沙箱，只能修改目标项目工作区。
- 首次启动默认跳过历史消息，避免误执行旧聊天。
- 使用状态文件去重；任务即使失败也不会自动重复执行。
- 先回执“已接收”，完成或失败后再把结果发回微信。

## 微信命令

```markdown
#codex yuanze
检查首页的移动端布局问题，修复后运行测试。
不要提交、推送或部署。
```

等价的完整格式：

```markdown
#taskian codex yuanze
检查首页的移动端布局问题，修复后运行测试。
```

Taskian 只处理第一行符合上述格式的微信消息。普通聊天会被记录为已忽略，不会交给 AI。

## 快速开始（Windows）

1. 安装并登录 Codex CLI，确认 `codex --version` 可用。
2. 复制 `config.windows.example.json` 为 `%USERPROFILE%\.taskian\config.json`。
3. 修改 `vault_path`、项目名称和项目路径。
4. 检查配置：

   ```powershell
   .\taskian-windows-amd64.exe check
   ```

5. 启动：

   ```powershell
   .\taskian-windows-amd64.exe serve
   ```

首次启动会把现有微信消息登记为历史消息。看到“现在可以从微信发送新任务”后，再发送测试任务。

如需开机启动，可在 Windows“任务计划程序”中创建“用户登录时”运行的任务，程序参数为：

```text
serve -config C:\Users\你的用户名\.taskian\config.json
```

## 快速开始（Linux）

1. 把 `taskian-linux-amd64` 放到 `~/.local/bin/taskian` 并添加执行权限。
2. 复制 `config.linux.example.json` 为 `~/.taskian/config.json` 并修改路径。
3. 确认 Obsidian vault 已同步到本机，且 Codex CLI 已登录。
4. 运行 `taskian check`，然后运行 `taskian serve`。

仓库提供 `deploy/taskian.service` 作为 systemd user service 示例：

```sh
mkdir -p ~/.config/systemd/user
cp deploy/taskian.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now taskian
```

## 快速开始（macOS）

根据 Mac 芯片选择 `taskian-darwin-arm64`（Apple Silicon）或
`taskian-darwin-amd64`（Intel），复制为 `~/.local/bin/taskian` 并添加执行权限。
复制 `config.macos.example.json` 为 `~/.taskian/config.json`，修改 vault 和项目路径后运行：

```sh
taskian check
taskian serve
```

## 配置说明

- `vault_path`：Obsidian vault 根目录。
- `inbox_dir` / `outbox_dir`：相对路径默认相对于 vault，也可写绝对路径。
- `state_path`：任务去重状态；不要放进多人共享目录。
- `poll_interval`：扫描间隔，例如 `10s`。
- `skip_existing_on_first_run`：建议保持 `true`。
- `agents`：允许调用的本地 agent 命令及参数模板。
- `projects`：微信可访问的项目白名单；每个项目绑定一个 agent。

agent 参数支持以下占位符：

- `{prompt}`：微信任务正文，作为单个进程参数传入。
- `{output}`：Taskian 创建的临时结果文件。
- `{project}`：白名单中的项目绝对路径。

## 安全边界

Taskian 会让本地 AI 根据微信指令修改代码，因此应当把它视为远程控制入口：

- 仅配置确实需要远程操作的项目。
- 保持 Codex `workspace-write` 沙箱；不要无故改成 `danger-full-access`。
- 在任务正文中明确禁止提交、推送、部署和删除。
- 不要在微信任务中发送密码、令牌或其他机密信息。
- Taskian 不会自动执行 `git push` 或部署，但 agent 能做什么最终取决于其自身权限与配置。
- 同一 vault 建议只运行一个 Taskian 实例。

## 开发与构建

```text
go test ./...
```

Windows：

```powershell
.\scripts\build.ps1
```

Linux/macOS：

```sh
./scripts/build.sh
```

产物写入 `dist/`：

- `taskian-windows-amd64.exe`
- `taskian-linux-amd64`
- `taskian-darwin-amd64`（Intel Mac）
- `taskian-darwin-arm64`（Apple Silicon Mac）

## 版本规划

每个版本的需求、验收标准和 Release Note 摘要统一保存在 [`docs`](docs/README.md)
目录，并以版本号命名，例如 [`docs/0.2.md`](docs/0.2.md)。Git Tag `0.2`
对应需求文档 `docs/0.2.md`。

Taskian 的长期定位、成本边界和相对于通用 AI Agent 平台的优势记录在
[`docs/product-advantages.md`](docs/product-advantages.md)。

## Tag 自动发布

推送任意 Git Tag 后，GitHub Actions 会先运行测试和静态检查，再构建以下版本：

- Windows amd64
- Linux amd64
- macOS amd64
- macOS arm64

全部构建成功后，工作流自动创建同名 GitHub Release，并将四个程序作为下载附件上传。
普通 `main` 分支推送和 Pull Request 只运行测试与静态检查，不创建 Release。

## 当前限制

- 当前使用定时轮询而非文件系统事件，优先保证 Windows/Linux 行为一致。
- 任务串行执行；一个长任务运行期间，新任务会留在收件箱等待。
- 微信网关有主动消息限流，Taskian 只发送接收、完成或失败等少量通知。
- Cursor 等其他 agent 可以通过新增 `agents` 配置接入，但需要该工具提供可脚本调用的命令行模式。
