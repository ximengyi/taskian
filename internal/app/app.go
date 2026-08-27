package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"taskian.local/taskian/internal/config"
	"taskian.local/taskian/internal/message"
	"taskian.local/taskian/internal/outbox"
	"taskian.local/taskian/internal/runner"
	"taskian.local/taskian/internal/state"
)

type Dispatcher struct {
	cfg   *config.Config
	state *state.Store
	log   *log.Logger
}

func New(cfg *config.Config, logger *log.Logger) (*Dispatcher, error) {
	store, err := state.Load(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(cfg.InboxDir); err != nil {
		return nil, fmt.Errorf("Wechatian 收件箱不可用: %w", err)
	}
	if err := os.MkdirAll(cfg.OutboxDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建 Wechatian 发件箱: %w", err)
	}
	return &Dispatcher{cfg: cfg, state: store, log: logger}, nil
}

func (d *Dispatcher) Serve(ctx context.Context) error {
	d.log.Printf("Taskian 已启动，收件箱=%s，轮询=%s", d.cfg.InboxDir, d.cfg.PollInterval)
	if err := d.RunOnce(ctx); err != nil {
		d.log.Printf("首次扫描失败: %v", err)
	}
	ticker := time.NewTicker(d.cfg.PollDuration())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			d.log.Printf("Taskian 正在停止")
			return nil
		case <-ticker.C:
			if err := d.RunOnce(ctx); err != nil {
				d.log.Printf("扫描失败: %v", err)
			}
		}
	}
}

func (d *Dispatcher) RunOnce(ctx context.Context) error {
	messages, err := message.ScanInbox(d.cfg.InboxDir)
	if err != nil {
		return err
	}
	if d.state.Fresh && d.cfg.SkipExistingOnFirstRun {
		for _, item := range messages {
			d.state.Mark(item.ID, "bootstrap-skipped")
		}
		if err := d.state.Save(); err != nil {
			return err
		}
		d.log.Printf("首次运行已跳过 %d 条历史消息；现在可以从微信发送新任务", len(messages))
		return nil
	}

	for _, incoming := range messages {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.state.Has(incoming.ID) {
			continue
		}
		task, parseErr := message.ParseTask(incoming)
		if errors.Is(parseErr, message.ErrNotCommand) {
			d.state.Mark(incoming.ID, "ignored")
			continue
		}
		if parseErr != nil {
			d.state.Mark(incoming.ID, "invalid")
			_ = d.reply("invalid", "⚠️ Taskian 无法解析任务\n\n"+parseErr.Error())
			continue
		}
		if err := d.execute(ctx, task); err != nil {
			d.log.Printf("执行任务 %s 失败: %v", shortID(incoming.ID), err)
		}
	}
	return d.state.Save()
}

func (d *Dispatcher) execute(ctx context.Context, task message.Task) error {
	project, ok := d.cfg.Projects[task.Project]
	if !ok {
		d.state.Mark(task.Source.ID, "rejected")
		return d.reply("rejected", fmt.Sprintf("⛔ 未授权的项目：`%s`\n\n允许的项目：%s", task.Project, strings.Join(sortedProjectNames(d.cfg.Projects), "、")))
	}
	if !strings.EqualFold(project.Agent, task.Agent) {
		d.state.Mark(task.Source.ID, "rejected")
		return d.reply("rejected", fmt.Sprintf("⛔ 项目 `%s` 只允许使用 `%s`，不能使用 `%s`。", task.Project, project.Agent, task.Agent))
	}
	agent, ok := d.cfg.Agents[task.Agent]
	if !ok {
		d.state.Mark(task.Source.ID, "rejected")
		return d.reply("rejected", fmt.Sprintf("⛔ 未配置 agent：`%s`", task.Agent))
	}

	d.state.Mark(task.Source.ID, "accepted")
	if err := d.state.Save(); err != nil {
		return err
	}
	_ = d.reply("accepted", fmt.Sprintf("✅ Taskian 已接收任务\n\n- Agent：`%s`\n- 项目：`%s`\n- 编号：`%s`", task.Agent, task.Project, shortID(task.Source.ID)))
	d.log.Printf("开始任务 %s agent=%s project=%s", shortID(task.Source.ID), task.Agent, task.Project)

	result, runErr := runner.Run(ctx, task, agent, project)
	if runErr != nil {
		d.state.Mark(task.Source.ID, "failed")
		logs := truncate(result.Logs, 1500)
		message := fmt.Sprintf("❌ Taskian 任务失败\n\n- 项目：`%s`\n- 编号：`%s`\n- 原因：%s", task.Project, shortID(task.Source.ID), runErr)
		if logs != "" {
			message += "\n\n```text\n" + logs + "\n```"
		}
		_ = d.reply("failed", truncate(message, d.cfg.MaxReplyChars))
		return runErr
	}
	d.state.Mark(task.Source.ID, "completed")
	message := fmt.Sprintf("✅ Taskian 任务完成\n\n- 项目：`%s`\n- 用时：%s\n- 编号：`%s`\n\n%s", task.Project, result.Duration.Round(time.Second), shortID(task.Source.ID), result.Text)
	return d.reply("completed", truncate(message, d.cfg.MaxReplyChars))
}

func (d *Dispatcher) reply(label, content string) error {
	path, err := outbox.Write(d.cfg.OutboxDir, label, content)
	if err == nil {
		d.log.Printf("已写入 Wechatian 发件箱: %s", path)
	}
	return err
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func truncate(value string, max int) string {
	value = strings.TrimSpace(value)
	if max <= 0 || utf8.RuneCountInString(value) <= max {
		return value
	}
	runes := []rune(value)
	return string(runes[:max]) + "\n\n…（结果过长，已截断）"
}

func sortedProjectNames(projects map[string]config.ProjectConfig) []string {
	names := make([]string, 0, len(projects))
	for name := range projects {
		names = append(names, name)
	}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return names
}
