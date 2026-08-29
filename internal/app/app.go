package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gofrs/flock"
	"github.com/ximengyi/taskian/internal/agent"
	"github.com/ximengyi/taskian/internal/config"
	"github.com/ximengyi/taskian/internal/feishu"
	"github.com/ximengyi/taskian/internal/ilink"
	"github.com/ximengyi/taskian/internal/message"
	"github.com/ximengyi/taskian/internal/store"
	"github.com/ximengyi/taskian/internal/transport"
)

type work struct {
	task   store.Task
	answer string
	resume bool
}

type Dispatcher struct {
	cfg          *config.Config
	store        *store.Store
	transport    transport.Transport
	agents       map[string]agent.Adapter
	log          *log.Logger
	queue        chan work
	runningMu    sync.Mutex
	running      map[string]context.CancelFunc
	workerWG     sync.WaitGroup
	healthMu     sync.Mutex
	health       map[string]bool
	systemRunner func(string) error
}

func New(cfg *config.Config, logger *log.Logger) (*Dispatcher, error) {
	state, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	if err := state.ImportLegacy(cfg.StatePath); err != nil {
		_ = state.Close()
		return nil, err
	}
	if err := initializeProjects(cfg, state); err != nil {
		_ = state.Close()
		return nil, err
	}
	channels := make([]transport.Transport, 0, len(cfg.Channels))
	for _, channelCfg := range cfg.Channels {
		var item transport.Transport
		switch channelCfg.Type {
		case "ilink":
			item, err = ilink.New(channelCfg, state)
		case "feishu":
			item, err = feishu.New(channelCfg, state, logger)
		case "wechatian-files":
			item, err = transport.NewFiles(cfg.InboxDir, cfg.OutboxDir)
			if err == nil && cfg.SkipExistingOnFirstRun {
				bootstrapped, stateErr := state.GetChannelState("wechatian-files.bootstrapped")
				if stateErr != nil {
					err = stateErr
				} else if bootstrapped == "" {
					history, pollErr := item.Poll(context.Background())
					if pollErr != nil {
						err = pollErr
					} else {
						for _, old := range history {
							_, _ = state.ClaimInbound(old.Channel, old.ID, old.Sender, old.Body, old.ReceivedAt)
							_ = state.MarkInbound(old.Channel, old.ID, "bootstrap-skipped")
						}
						err = state.SetChannelState("wechatian-files.bootstrapped", "true")
						logger.Printf("首次运行已跳过 %d 条 Wechatian 历史消息", len(history))
					}
				}
			}
		default:
			err = fmt.Errorf("不支持的通道 %s", channelCfg.Type)
		}
		if err != nil {
			for _, opened := range channels {
				_ = opened.Close()
			}
			_ = state.Close()
			return nil, err
		}
		channels = append(channels, item)
	}
	channel, err := transport.NewMulti(channels...)
	if err != nil {
		_ = state.Close()
		return nil, err
	}
	if cfg.Mode == "personal" && len(cfg.Agents) == 0 {
		cfg.Agents = map[string]config.AgentConfig{}
		for _, found := range agent.Detect() {
			if _, exists := cfg.Agents[found.Type]; exists {
				continue
			}
			item := config.AgentConfig{Type: found.Type, Command: found.Path, Timeout: "45m"}
			if found.Type == "codex" {
				item.Sandbox = "workspace-write"
			}
			cfg.Agents[found.Type] = item
		}
	}
	if _, exists := cfg.Agents[cfg.DefaultAgent]; !exists && len(cfg.Agents) > 0 {
		names := make([]string, 0, len(cfg.Agents))
		for name := range cfg.Agents {
			names = append(names, name)
		}
		sort.Strings(names)
		cfg.DefaultAgent = names[0]
	}
	adapters := make(map[string]agent.Adapter, len(cfg.Agents))
	for name, agentCfg := range cfg.Agents {
		resolved, found := agent.ResolveCommand(agentCfg.Type, agentCfg.Command)
		if found && resolved != agentCfg.Command {
			logger.Printf("自动找到 Agent %s: %s", name, resolved)
			agentCfg.Command = resolved
			cfg.Agents[name] = agentCfg
		}
		a, e := agent.New(agentCfg)
		if e != nil {
			_ = channel.Close()
			_ = state.Close()
			return nil, e
		}
		adapters[name] = a
	}
	return newDispatcher(cfg, state, channel, adapters, logger), nil
}

func initializeProjects(cfg *config.Config, state *store.Store) error {
	for name, project := range cfg.Projects {
		if _, err := state.Project(name); errors.Is(err, sql.ErrNoRows) {
			if _, err := state.PutProjectRecord(name, project.Path); err != nil {
				return fmt.Errorf("导入配置项目 %s: %w", name, err)
			}
		} else if err != nil {
			return err
		}
	}
	projects, err := state.Projects()
	if err != nil {
		return err
	}
	if len(projects) == 0 && cfg.Mode == "personal" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		project, err := state.PutProjectRecord("global", home)
		if err != nil {
			return err
		}
		projects = []store.Project{project}
	}
	if _, err := state.ActiveProject(); errors.Is(err, sql.ErrNoRows) && len(projects) > 0 {
		selected := projects[0]
		if cfg.DefaultProject != "" {
			if project, findErr := state.Project(cfg.DefaultProject); findErr == nil {
				selected = project
			}
		} else if project, recentErr := state.MostRecentConversationProject(); recentErr == nil {
			selected = project
		}
		return state.SetActiveProject(selected.ID)
	} else {
		return err
	}
}

func newDispatcher(cfg *config.Config, state *store.Store, channel transport.Transport, adapters map[string]agent.Adapter, logger *log.Logger) *Dispatcher {
	return &Dispatcher{cfg: cfg, store: state, transport: channel, agents: adapters, log: logger, queue: make(chan work, 128), running: map[string]context.CancelFunc{}, health: map[string]bool{}, systemRunner: runSystemAction}
}

func (d *Dispatcher) Close() error { _ = d.transport.Close(); return d.store.Close() }

func (d *Dispatcher) Serve(ctx context.Context) error {
	defer d.Close()
	instanceLock := flock.New(d.cfg.DatabasePath + ".lock")
	locked, err := instanceLock.TryLock()
	if err != nil {
		return fmt.Errorf("获取单实例锁: %w", err)
	}
	if !locked {
		return fmt.Errorf("另一个 Taskian 实例正在使用状态库 %s", d.cfg.DatabasePath)
	}
	defer instanceLock.Unlock()
	workerCtx, stopWorkers := context.WithCancel(ctx)
	defer d.workerWG.Wait()
	defer stopWorkers()
	if err := d.store.RecoverInterrupted(); err != nil {
		return err
	}
	d.log.Printf("Taskian 0.5 已启动，通道=%s，并发=%d", d.transport.Name(), d.cfg.MaxConcurrentTasks)
	d.runHealthCheck(ctx, true)
	_ = d.store.SetChannelState("service.heartbeat", time.Now().UTC().Format(time.RFC3339Nano))
	d.workerWG.Add(1)
	go func() {
		defer d.workerWG.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case now := <-ticker.C:
				_ = d.store.SetChannelState("service.heartbeat", now.UTC().Format(time.RFC3339Nano))
			}
		}
	}()
	if d.cfg.HealthEnabled() {
		d.workerWG.Add(1)
		go func() {
			defer d.workerWG.Done()
			d.healthLoop(workerCtx)
		}()
	}
	for i := 0; i < d.cfg.MaxConcurrentTasks; i++ {
		d.workerWG.Add(1)
		go func() {
			defer d.workerWG.Done()
			d.worker(workerCtx)
		}()
	}
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := d.poll(ctx, true); err != nil {
			if errors.Is(err, ilink.ErrSessionExpired) && d.transport.Name() == "ilink" {
				d.log.Printf("%v", err)
				return err
			}
			d.log.Printf("接收消息失败: %v", err)
			if d.transport.Name() == "ilink" {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(backoff):
				}
				if backoff < 30*time.Second {
					backoff *= 2
				}
				continue
			}
		}
		backoff = time.Second
		d.expireWaiting(ctx)
		if d.transport.Name() == "wechatian-files" {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(d.cfg.PollDuration()):
			}
		}
	}
}

func (d *Dispatcher) RunOnce(ctx context.Context) error {
	defer d.Close()
	if err := d.store.RecoverInterrupted(); err != nil {
		return err
	}
	return d.poll(ctx, false)
}

func (d *Dispatcher) poll(ctx context.Context, async bool) error {
	messages, err := d.transport.Poll(ctx)
	if err != nil {
		return err
	}
	if d.cfg.SkipExistingOnFirstRun {
		bootstrapped, e := d.store.GetChannelState("wechatian-files.bootstrapped")
		if e != nil {
			return e
		}
		if bootstrapped == "" {
			kept := messages[:0]
			skipped := 0
			for _, m := range messages {
				if m.Channel != "wechatian-files" {
					kept = append(kept, m)
					continue
				}
				_, _ = d.store.ClaimInbound(m.Channel, m.ID, m.Sender, m.Body, m.ReceivedAt)
				_ = d.store.MarkInbound(m.Channel, m.ID, "bootstrap-skipped")
				skipped++
			}
			if skipped > 0 {
				_ = d.store.SetChannelState("wechatian-files.bootstrapped", "true")
				d.log.Printf("首次运行已跳过 %d 条 Wechatian 历史消息", skipped)
			} else if d.transport.Name() == "wechatian-files" && len(messages) == 0 {
				_ = d.store.SetChannelState("wechatian-files.bootstrapped", "true")
			}
			messages = kept
		}
	}
	for _, incoming := range messages {
		claimed, e := d.store.ClaimInbound(incoming.Channel, incoming.ID, incoming.Sender, incoming.Body, incoming.ReceivedAt)
		if e != nil {
			return e
		}
		if !claimed {
			continue
		}
		d.log.Printf("收到消息：通道=%s 发送者=%s 消息=%s 内容=%q", incoming.Channel, incoming.Sender, incoming.ID, logText(incoming.Body, 500))
		if e := d.handle(ctx, incoming, async); e != nil {
			d.log.Printf("处理消息 %s 失败: %v", incoming.ID, e)
			_ = d.store.MarkInbound(incoming.Channel, incoming.ID, "failed")
		}
	}
	return nil
}

func (d *Dispatcher) handle(ctx context.Context, in message.Incoming, async bool) error {
	command, err := message.ParseCommand(in)
	if errors.Is(err, message.ErrNotCommand) {
		if d.cfg.Mode == "personal" {
			if action := detectSystemAction(in.Body); action != "" {
				return d.requestSystemConfirmation(ctx, in, action)
			}
		}
		waiting, waitErr := d.store.WaitingFor(in.Sender)
		if waitErr != nil {
			return waitErr
		}
		if len(waiting) == 1 {
			return d.handleReply(ctx, in, message.Command{Kind: message.CommandReply, TaskID: waiting[0].ID, Text: strings.TrimSpace(in.Body)}, async)
		}
		if len(waiting) == 0 {
			if id, parseErr := strconv.ParseInt(strings.TrimSpace(in.Body), 10, 64); parseErr == nil && id > 0 {
				if kind, active, contextErr := d.store.SelectionContext(in.Channel, in.Sender, time.Now()); contextErr != nil {
					return contextErr
				} else if active && kind == "project" {
					return d.handleUse(ctx, in, strconv.FormatInt(id, 10))
				}
			}
		}
		if len(waiting) == 0 && d.cfg.Mode == "personal" {
			return d.handleTask(ctx, in, message.Task{Agent: d.cfg.DefaultAgent, Prompt: strings.TrimSpace(in.Body), Source: in}, async)
		}
		_ = d.store.MarkInbound(in.Channel, in.ID, "ignored")
		return nil
	}
	if err != nil {
		_ = d.store.MarkInbound(in.Channel, in.ID, "invalid")
		return d.send(ctx, in.Sender, "⚠️ Taskian 无法解析命令\n\n"+err.Error(), "", "invalid")
	}
	switch command.Kind {
	case message.CommandTask:
		return d.handleTask(ctx, in, command.Task, async)
	case message.CommandReply:
		return d.handleReply(ctx, in, command, async)
	case message.CommandStatus:
		return d.handleStatus(ctx, in, command.TaskID)
	case message.CommandCancel:
		return d.handleCancel(ctx, in, command.TaskID)
	case message.CommandHelp:
		_ = d.store.MarkInbound(in.Channel, in.ID, "help")
		return d.send(ctx, in.Sender, d.help(in, command.Text), "", "help")
	case message.CommandProject:
		return d.handleProject(ctx, in, command)
	case message.CommandUse:
		return d.handleUse(ctx, in, command.Text)
	case message.CommandConfirm:
		return d.handleConfirmation(ctx, in, command.Text)
	default:
		return nil
	}
}

func (d *Dispatcher) handleTask(ctx context.Context, in message.Incoming, task message.Task, async bool) error {
	if task.Agent == "task" || task.Agent == "taskian" || task.Agent == "" {
		task.Agent = d.cfg.DefaultAgent
	}
	projectID, projectName, projectPath, prompt, err := d.resolveTaskTarget(in, task)
	if err != nil {
		_ = d.store.MarkInbound(in.Channel, in.ID, "rejected")
		return d.send(ctx, in.Sender, "⚠️ "+err.Error(), "", "rejected")
	}
	if configured, ok := d.cfg.Projects[projectName]; d.cfg.Mode == "controlled" && (!ok || !configured.Allows(task.Agent)) {
		_ = d.store.MarkInbound(in.Channel, in.ID, "rejected")
		return d.send(ctx, in.Sender, fmt.Sprintf("⛔ 项目 %s 不允许使用 %s。", projectName, task.Agent), "", "rejected")
	}
	if _, ok := d.agents[task.Agent]; !ok {
		_ = d.store.MarkInbound(in.Channel, in.ID, "rejected")
		return d.send(ctx, in.Sender, "⛔ 未配置 Agent："+task.Agent, "", "rejected")
	}
	record, err := d.store.CreateTask(store.Task{SourceMessageID: in.ID, Channel: in.Channel, Sender: in.Sender, Conversation: in.Conversation, Agent: task.Agent, ProjectID: projectID, Project: projectName, ProjectPath: projectPath, Prompt: prompt, Status: store.StatusQueued})
	if err != nil {
		return err
	}
	_ = d.store.MarkInbound(in.Channel, in.ID, "accepted")
	if err := d.send(ctx, in.Sender, fmt.Sprintf("✅ [%s] 已接收\nAgent：%s\n项目：%s", record.ID, record.Agent, record.Project), record.ID, "accepted"); err != nil {
		return err
	}
	d.log.Printf("任务已接收：任务=%s Agent=%s 项目=%s 目录=%q", record.ID, record.Agent, record.Project, record.ProjectPath)
	item := work{task: record}
	if async {
		d.queue <- item
	} else {
		d.execute(ctx, item)
	}
	return nil
}

func (d *Dispatcher) resolveTaskTarget(in message.Incoming, task message.Task) (projectID int64, name, path, prompt string, err error) {
	target := strings.TrimSpace(task.Project)
	prompt = strings.TrimSpace(task.Prompt)
	resolve := func(value string) (int64, string, string, bool) {
		key := strings.ToLower(strings.TrimSpace(value))
		if p, ok := d.cfg.Projects[key]; ok {
			registered, _ := d.store.Project(key)
			return registered.ID, key, p.Path, true
		}
		if p, e := d.store.Project(key); e == nil {
			_ = d.store.TouchProject(key)
			return p.ID, p.Name, p.Path, true
		}
		if filepath.IsAbs(value) {
			info, e := os.Stat(value)
			if e == nil && info.IsDir() {
				return 0, filepath.Base(filepath.Clean(value)), filepath.Clean(value), true
			}
		}
		return 0, "", "", false
	}
	if target != "" {
		if id, name, path, ok := resolve(target); ok {
			return id, name, path, prompt, nil
		}
	}
	var current store.Project
	var e error
	current, e = d.currentProject(in.Sender)
	if errors.Is(e, sql.ErrNoRows) && d.cfg.DefaultProject != "" {
		current, e = d.store.Project(d.cfg.DefaultProject)
	}
	if e == nil {
		if id, name, path, ok := resolve(strconv.FormatInt(current.ID, 10)); ok {
			if target != "" {
				prompt = strings.TrimSpace(target + " " + prompt)
			}
			if prompt == "" {
				return 0, "", "", "", fmt.Errorf("任务内容不能为空")
			}
			return id, name, path, prompt, nil
		}
	}
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return 0, "", "", "", e
	}
	if target == "" {
		return 0, "", "", "", fmt.Errorf("尚未选择项目，请发送 项目列表 后选择项目")
	}
	return 0, "", "", "", fmt.Errorf("找不到项目或目录 %q；可先发送 #project add <名称> <绝对路径>", target)
}

func (d *Dispatcher) handleUse(ctx context.Context, in message.Incoming, reference string) error {
	p, err := d.store.Project(reference)
	if errors.Is(err, sql.ErrNoRows) {
		if configured, ok := d.cfg.Projects[reference]; ok {
			p, err = d.store.PutProjectRecord(reference, configured.Path)
			if err != nil {
				return err
			}
		} else {
			return d.send(ctx, in.Sender, "⚠️ 找不到项目 "+reference, "", "project")
		}
	} else if err != nil {
		return err
	}
	if d.cfg.Mode == "personal" {
		if err := d.store.SetActiveProject(p.ID); err != nil {
			return err
		}
	} else {
		if err := d.store.SetSenderProject(in.Sender, p.ID); err != nil {
			return err
		}
	}
	_ = d.store.TouchProject(strconv.FormatInt(p.ID, 10))
	_ = d.store.ClearSelectionContext(in.Channel, in.Sender)
	_ = d.store.MarkInbound(in.Channel, in.ID, "project-use")
	return d.send(ctx, in.Sender, fmt.Sprintf("✅ 已全局切换项目\n[%d] %s\n目录：%s\n微信和飞书后续任务均使用此项目。", p.ID, p.Name, p.Path), "", "project")
}

func (d *Dispatcher) currentProject(sender string) (store.Project, error) {
	if d.cfg.Mode == "personal" {
		project, err := d.store.ActiveProject()
		if !errors.Is(err, sql.ErrNoRows) {
			return project, err
		}
		projects, listErr := d.store.Projects()
		if listErr != nil {
			return store.Project{}, listErr
		}
		if len(projects) == 0 {
			home, homeErr := os.UserHomeDir()
			if homeErr != nil {
				return store.Project{}, homeErr
			}
			project, err = d.store.PutProjectRecord("global", home)
			if err != nil {
				return store.Project{}, err
			}
		} else {
			project = projects[0]
		}
		if err := d.store.SetActiveProject(project.ID); err != nil {
			return store.Project{}, err
		}
		return project, nil
	}
	project, err := d.store.SenderProject(sender)
	if errors.Is(err, sql.ErrNoRows) && d.cfg.DefaultProject != "" {
		return d.store.Project(d.cfg.DefaultProject)
	}
	return project, err
}

func (d *Dispatcher) handleProject(ctx context.Context, in message.Incoming, command message.Command) error {
	args := command.Args
	switch command.Action {
	case "add":
		if len(args) < 2 {
			return d.send(ctx, in.Sender, "格式：#project add <名称> <绝对路径>", "", "project")
		}
		name, path := strings.ToLower(args[0]), filepath.Clean(strings.Join(args[1:], " "))
		if !filepath.IsAbs(path) {
			return d.send(ctx, in.Sender, "⚠️ 项目目录必须是绝对路径", "", "project")
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			return d.send(ctx, in.Sender, "⚠️ 项目目录不存在或不是目录："+path, "", "project")
		}
		if existing, lookupErr := d.store.ProjectByPath(path); lookupErr == nil && !strings.EqualFold(existing.Name, name) {
			return d.send(ctx, in.Sender, fmt.Sprintf("⚠️ 该目录已属于项目 [%d] %s", existing.ID, existing.Name), "", "project")
		}
		project, err := d.store.PutProjectRecord(name, path)
		if err != nil {
			return err
		}
		_ = d.store.MarkInbound(in.Channel, in.ID, "project-add")
		return d.send(ctx, in.Sender, fmt.Sprintf("✅ 已注册项目\n[%d] %s\n%s", project.ID, project.Name, project.Path), "", "project")
	case "list":
		projects, err := d.store.Projects()
		if err != nil {
			return err
		}
		current, _ := d.currentProject(in.Sender)
		lines := []string{fmt.Sprintf("项目列表（%d 个）：", len(projects))}
		for _, p := range projects {
			mark := "  "
			if p.ID == current.ID {
				mark = " ★"
			}
			lines = append(lines, fmt.Sprintf("[%d] %s%s  %s", p.ID, p.Name, mark, p.Path))
		}
		if len(lines) == 1 {
			lines = append(lines, "尚未注册项目。使用：#project add <名称> <绝对路径>")
		}
		lines = append(lines, "", "5 分钟内可直接回复项目 ID，例如：3")
		_ = d.store.SetSelectionContext(in.Channel, in.Sender, "project", time.Now().Add(5*time.Minute))
		_ = d.store.MarkInbound(in.Channel, in.ID, "project-list")
		return d.send(ctx, in.Sender, strings.Join(lines, "\n"), "", "project")
	case "current":
		p, err := d.currentProject(in.Sender)
		if err != nil {
			return d.send(ctx, in.Sender, "⚠️ 当前没有可用项目，请发送 项目列表。", "", "project")
		}
		_ = d.store.MarkInbound(in.Channel, in.ID, "project-current")
		return d.send(ctx, in.Sender, fmt.Sprintf("当前项目\n[%d] %s\n目录：%s\n默认 Agent：%s\n来源：全局选择", p.ID, p.Name, p.Path, d.cfg.DefaultAgent), "", "project")
	case "show":
		if len(args) != 1 {
			return d.send(ctx, in.Sender, "格式：#project show <名称>", "", "project")
		}
		p, err := d.store.Project(args[0])
		if err != nil {
			return d.send(ctx, in.Sender, "⚠️ 找不到项目 "+args[0], "", "project")
		}
		return d.send(ctx, in.Sender, fmt.Sprintf("项目：[%d] %s\n目录：%s\n最近使用：%s", p.ID, p.Name, p.Path, p.LastUsedAt.Local().Format("2006-01-02 15:04")), "", "project")
	case "rename":
		if len(args) != 2 {
			return d.send(ctx, in.Sender, "格式：#project rename <旧名称> <新名称>", "", "project")
		}
		if err := d.store.RenameProject(args[0], args[1]); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return d.send(ctx, in.Sender, "⚠️ 找不到项目 "+args[0], "", "project")
			}
			return err
		}
		return d.send(ctx, in.Sender, fmt.Sprintf("✅ 项目 %s 已重命名为 %s", args[0], args[1]), "", "project")
	case "path", "set-path":
		if len(args) < 2 {
			return d.send(ctx, in.Sender, "格式：#project path <ID或名称> <绝对路径>", "", "project")
		}
		path := strings.Trim(strings.TrimSpace(strings.Join(args[1:], " ")), `"`)
		path = filepath.Clean(path)
		if !filepath.IsAbs(path) {
			return d.send(ctx, in.Sender, "⚠️ 项目目录必须是绝对路径", "", "project")
		}
		info, statErr := os.Stat(path)
		if statErr != nil || !info.IsDir() {
			return d.send(ctx, in.Sender, "⚠️ 项目目录不存在或不是目录："+path, "", "project")
		}
		targetProject, targetErr := d.store.Project(args[0])
		if targetErr != nil {
			return d.send(ctx, in.Sender, "⚠️ 找不到项目 "+args[0], "", "project")
		}
		if existing, lookupErr := d.store.ProjectByPath(path); lookupErr == nil && existing.ID != targetProject.ID {
			return d.send(ctx, in.Sender, fmt.Sprintf("⚠️ 该目录已属于项目 [%d] %s", existing.ID, existing.Name), "", "project")
		}
		project, updateErr := d.store.UpdateProjectPath(args[0], path)
		if updateErr != nil {
			if errors.Is(updateErr, sql.ErrNoRows) {
				return d.send(ctx, in.Sender, "⚠️ 找不到项目 "+args[0], "", "project")
			}
			return d.send(ctx, in.Sender, "⚠️ 无法修改项目路径，可能已被其他项目使用："+updateErr.Error(), "", "project")
		}
		_ = d.store.MarkInbound(in.Channel, in.ID, "project-path")
		return d.send(ctx, in.Sender, fmt.Sprintf("✅ 项目路径已更新\n[%d] %s\n目录：%s\n后续新任务立即使用此路径。", project.ID, project.Name, project.Path), "", "project")
	case "remove":
		if len(args) != 1 {
			return d.send(ctx, in.Sender, "格式：#project remove <ID或名称>", "", "project")
		}
		project, lookupErr := d.store.Project(args[0])
		if lookupErr != nil {
			return d.send(ctx, in.Sender, "⚠️ 找不到项目 "+args[0], "", "project")
		}
		current, _ := d.currentProject(in.Sender)
		if current.ID == project.ID {
			return d.send(ctx, in.Sender, "⚠️ 不能删除当前项目。请先切换到其他项目，再删除。", "", "project")
		}
		if err := d.store.RemoveProject(args[0]); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return d.send(ctx, in.Sender, "⚠️ 找不到项目 "+args[0], "", "project")
			}
			return err
		}
		return d.send(ctx, in.Sender, fmt.Sprintf("✅ 已删除项目 [%d] %s；磁盘文件未改动。", project.ID, project.Name), "", "project")
	case "find":
		if len(args) < 2 {
			return d.send(ctx, in.Sender, "格式：#project find <目录名> <搜索根目录>", "", "project")
		}
		matches, err := findDirectories(ctx, args[0], strings.Join(args[1:], " "), 10)
		if err != nil {
			return d.send(ctx, in.Sender, "⚠️ 查找失败："+err.Error(), "", "project")
		}
		if len(matches) == 0 {
			return d.send(ctx, in.Sender, "没有找到名称为 "+args[0]+" 的目录。", "", "project")
		}
		lines := []string{"找到以下目录："}
		for i, value := range matches {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, value))
		}
		lines = append(lines, "\n注册：#project add "+args[0]+" <选择的路径>")
		return d.send(ctx, in.Sender, strings.Join(lines, "\n"), "", "project")
	default:
		return d.send(ctx, in.Sender, "未知项目命令。发送 help project 查看用法。", "", "project")
	}
}

func findDirectories(ctx context.Context, name, root string, limit int) ([]string, error) {
	root = filepath.Clean(root)
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("搜索根目录不可用：%s", root)
	}
	var matches []string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if walkErr != nil {
			return filepath.SkipDir
		}
		if !entry.IsDir() {
			return nil
		}
		base := entry.Name()
		if path != root && (base == ".git" || base == "node_modules" || base == ".cache") {
			return filepath.SkipDir
		}
		if path != root && strings.EqualFold(base, name) {
			matches = append(matches, path)
			if len(matches) >= limit {
				return filepath.SkipAll
			}
		}
		return nil
	})
	return matches, err
}

func (d *Dispatcher) help(in message.Incoming, topic string) string {
	current := "未设置"
	if project, err := d.currentProject(in.Sender); err == nil {
		current = fmt.Sprintf("[%d] %s", project.ID, project.Name)
	}
	agents := make([]string, 0, len(d.agents))
	for name := range d.agents {
		agents = append(agents, name)
	}
	sort.Strings(agents)
	header := fmt.Sprintf("Taskian 0.5 帮助\n默认 Agent：%s\n可用 Agent：%s\n当前项目：%s\n", d.cfg.DefaultAgent, strings.Join(agents, "、"), current)
	switch strings.ToLower(strings.TrimSpace(topic)) {
	case "project":
		return header + "\n当前项目\n项目列表\n切换项目 <ID>\n修改项目路径 <ID> <绝对路径>\n#project add <名称> <绝对路径>\n#project show <ID或名称>\n#project rename <ID或名称> <新名>\n#project path <ID或名称> <路径>\n#project remove <ID或名称>\n#project find <目录名> <根目录>\n#use <ID或名称>"
	case "task", "agent":
		return header + "\n个人模式可直接发送普通文本，使用默认 Agent。\n#task <项目> <任务>\n#codex <项目> <任务>\n#cursor <项目> <任务>\n#reply <任务号> <回答>\n#confirm <确认码>  确认关机/重启\n#status [任务号]\n#cancel <任务号>"
	case "channel":
		channels := make([]string, 0, len(d.cfg.Channels))
		for _, channel := range d.cfg.Channels {
			channels = append(channels, channel.Type)
		}
		return header + "\n已启用通道：" + strings.Join(channels, "、") + "\n本机诊断：taskian status\n微信登录：taskian ilink status\n飞书绑定：taskian feishu status\n详细运行日志：taskian service logs"
	case "examples":
		return header + "\n示例：\n#project add week-report D:\\work\\week-report\n#use week-report\n#cursor 写一下本周周报\n#task /srv/code/demo 运行测试\n#reply T-12345678 选择 B"
	default:
		return header + "\n当前项目    查看当前项目\n项目列表    按数字选择项目\n切换项目 3  全局切换\n直接发送文字即可在当前项目执行\n任务：#task / #codex / #cursor\n回答：#reply <任务号> <回答>\n状态：#status / #cancel\n更多：help task、help project、help channel"
	}
}

type pendingConfirmation struct {
	Code, Action string
	ExpiresAt    time.Time
}

func detectSystemAction(text string) string {
	value := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(text), " ", ""))
	value = strings.Trim(value, "。！!?，,；;")
	value = strings.TrimSuffix(value, "吧")
	for _, phrase := range []string{"帮我关一下机", "请帮我关一下机", "关一下机", "帮我关机", "请帮我关机", "现在关机", "立即关机", "关机", "shutdown", "poweroff"} {
		if value == phrase {
			return "shutdown"
		}
	}
	for _, phrase := range []string{"帮我重启", "请帮我重启", "现在重启", "立即重启", "重启", "reboot"} {
		if value == phrase {
			return "reboot"
		}
	}
	return ""
}

func (d *Dispatcher) requestSystemConfirmation(ctx context.Context, in message.Incoming, action string) error {
	random := make([]byte, 3)
	_, _ = rand.Read(random)
	code := strings.ToUpper(hex.EncodeToString(random))
	pending := pendingConfirmation{Code: code, Action: action, ExpiresAt: time.Now().Add(2 * time.Minute)}
	data, _ := json.Marshal(pending)
	if err := d.store.SetChannelState("confirm."+in.Sender, string(data)); err != nil {
		return err
	}
	label := map[string]string{"shutdown": "关机", "reboot": "重启"}[action]
	_ = d.store.MarkInbound(in.Channel, in.ID, "confirmation-required")
	return d.send(ctx, in.Sender, fmt.Sprintf("⚠️ %s会中断当前任务和远程连接。\n如确定执行，请在 2 分钟内回复：\n#confirm %s\n\n取消：#confirm cancel", label, code), "", "confirmation")
}

func (d *Dispatcher) handleConfirmation(ctx context.Context, in message.Incoming, answer string) error {
	key := "confirm." + in.Sender
	if strings.EqualFold(answer, "cancel") {
		_ = d.store.SetChannelState(key, "")
		_ = d.store.MarkInbound(in.Channel, in.ID, "confirmation-cancelled")
		return d.send(ctx, in.Sender, "已取消系统操作。", "", "confirmation")
	}
	value, err := d.store.GetChannelState(key)
	if err != nil {
		return err
	}
	var pending pendingConfirmation
	if value == "" || json.Unmarshal([]byte(value), &pending) != nil || time.Now().After(pending.ExpiresAt) || !strings.EqualFold(answer, pending.Code) {
		_ = d.store.MarkInbound(in.Channel, in.ID, "confirmation-invalid")
		return d.send(ctx, in.Sender, "⚠️ 确认码无效或已过期，系统操作未执行。", "", "confirmation")
	}
	_ = d.store.SetChannelState(key, "")
	_ = d.store.MarkInbound(in.Channel, in.ID, "confirmed")
	label := map[string]string{"shutdown": "关机", "reboot": "重启"}[pending.Action]
	if err := d.send(ctx, in.Sender, fmt.Sprintf("✅ 已确认%s，系统将在短暂延迟后执行。", label), "", "confirmation"); err != nil {
		return err
	}
	if err := d.systemRunner(pending.Action); err != nil {
		return d.send(ctx, in.Sender, "❌ 无法执行系统操作："+err.Error(), "", "confirmation")
	}
	return nil
}

func runSystemAction(action string) error {
	if runtime.GOOS == "windows" {
		flag := "/s"
		if action == "reboot" {
			flag = "/r"
		}
		return exec.Command("shutdown.exe", flag, "/t", "30", "/c", "Taskian confirmed system action").Run()
	}
	flag := "-h"
	if action == "reboot" {
		flag = "-r"
	}
	return exec.Command("shutdown", flag, "+1", "Taskian confirmed system action").Run()
}

func (d *Dispatcher) handleReply(ctx context.Context, in message.Incoming, command message.Command, async bool) error {
	taskID := command.TaskID
	if taskID == "" {
		waiting, err := d.store.WaitingFor(in.Sender)
		if err != nil {
			return err
		}
		if len(waiting) != 1 {
			return d.send(ctx, in.Sender, "⚠️ 当前没有唯一的待回答任务，请使用 #reply T-任务号 <回答>。", "", "invalid")
		}
		taskID = waiting[0].ID
	}
	record, err := d.store.Task(taskID)
	if err != nil {
		return d.send(ctx, in.Sender, "⚠️ 找不到任务 "+taskID, "", "invalid")
	}
	if record.Sender != in.Sender {
		return d.send(ctx, in.Sender, "⛔ 你无权回答该任务。", "", "rejected")
	}
	if record.Status != store.StatusWaitingUser {
		return d.send(ctx, in.Sender, fmt.Sprintf("⚠️ [%s] 当前状态是 %s，不能接收回答。", record.ID, record.Status), record.ID, "invalid")
	}
	if err := d.store.AddMessage(record.ID, "in", "answer", command.Text, in.ID); err != nil {
		return err
	}
	if err := d.store.SetStatus(record.ID, store.StatusResuming, ""); err != nil {
		return err
	}
	_ = d.store.MarkInbound(in.Channel, in.ID, "answer")
	if err := d.send(ctx, in.Sender, fmt.Sprintf("▶️ [%s] 已收到回答，正在恢复 %s 会话。", record.ID, record.Agent), record.ID, "resuming"); err != nil {
		return err
	}
	item := work{task: record, answer: command.Text, resume: true}
	if async {
		d.queue <- item
	} else {
		d.execute(ctx, item)
	}
	return nil
}

func (d *Dispatcher) handleStatus(ctx context.Context, in message.Incoming, id string) error {
	_ = d.store.MarkInbound(in.Channel, in.ID, "status")
	if id != "" {
		task, err := d.store.Task(id)
		if err != nil || task.Sender != in.Sender {
			return d.send(ctx, in.Sender, "⚠️ 找不到任务 "+id, "", "status")
		}
		return d.send(ctx, in.Sender, formatTaskStatus(task), task.ID, "status")
	}
	tasks, err := d.store.ActiveFor(in.Sender)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return d.send(ctx, in.Sender, "当前没有运行中或等待回答的任务。", "", "status")
	}
	lines := []string{"当前任务："}
	for _, task := range tasks {
		lines = append(lines, fmt.Sprintf("- [%s] %s / %s / %s", task.ID, task.Agent, task.Project, task.Status))
	}
	return d.send(ctx, in.Sender, strings.Join(lines, "\n"), "", "status")
}

func (d *Dispatcher) handleCancel(ctx context.Context, in message.Incoming, id string) error {
	task, err := d.store.Task(id)
	if err != nil || task.Sender != in.Sender {
		return d.send(ctx, in.Sender, "⚠️ 找不到任务 "+id, "", "cancel")
	}
	if task.Status == store.StatusCompleted || task.Status == store.StatusFailed || task.Status == store.StatusCancelled {
		return d.send(ctx, in.Sender, fmt.Sprintf("[%s] 已经处于 %s。", task.ID, task.Status), task.ID, "cancel")
	}
	if err := d.store.SetStatus(task.ID, store.StatusCancelled, "用户取消"); err != nil {
		return err
	}
	d.runningMu.Lock()
	cancel := d.running[task.ID]
	d.runningMu.Unlock()
	if cancel != nil {
		cancel()
	}
	_ = d.store.MarkInbound(in.Channel, in.ID, "cancelled")
	return d.send(ctx, in.Sender, fmt.Sprintf("⏹️ [%s] 已取消。", task.ID), task.ID, "cancelled")
}

func (d *Dispatcher) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-d.queue:
			d.execute(ctx, item)
		}
	}
}

func (d *Dispatcher) execute(parent context.Context, item work) {
	current, currentErr := d.store.Task(item.task.ID)
	if currentErr == nil && current.Status == store.StatusCancelled {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	d.runningMu.Lock()
	d.running[item.task.ID] = cancel
	d.runningMu.Unlock()
	defer func() { cancel(); d.runningMu.Lock(); delete(d.running, item.task.ID); d.runningMu.Unlock() }()
	status := store.StatusRunning
	if item.resume {
		status = store.StatusResuming
	}
	if err := d.store.SetStatus(item.task.ID, status, ""); err != nil {
		d.log.Printf("更新任务 %s: %v", item.task.ID, err)
		return
	}
	adapter := d.agents[item.task.Agent]
	output := &taskLogWriter{logger: d.log, taskID: item.task.ID}
	request := agent.Request{Prompt: item.task.Prompt, ProjectPath: item.task.ProjectPath, SessionID: item.task.AgentSessionID, Output: output}
	if item.resume {
		request.Prompt = item.answer
	}
	var result agent.Result
	var err error
	if item.resume {
		d.log.Printf("恢复 Agent：任务=%s Agent=%s 目录=%q 会话=%s", item.task.ID, item.task.Agent, item.task.ProjectPath, item.task.AgentSessionID)
		result, err = adapter.Resume(ctx, request)
	} else {
		d.log.Printf("启动 Agent：任务=%s Agent=%s 目录=%q", item.task.ID, item.task.Agent, item.task.ProjectPath)
		result, err = adapter.Start(ctx, request)
	}
	output.Flush()
	if result.SessionID != "" {
		_ = d.store.SetSession(item.task.ID, result.SessionID)
		item.task.AgentSessionID = result.SessionID
	}
	if err != nil {
		current, e := d.store.Task(item.task.ID)
		if e == nil && current.Status == store.StatusCancelled {
			return
		}
		detail := agentFailure(err, result.Logs)
		d.log.Printf("任务失败：任务=%s Agent=%s 错误=%s", item.task.ID, item.task.Agent, logText(detail, 4000))
		_ = d.store.SetResult(item.task.ID, store.StatusFailed, "", detail)
		_ = d.send(context.Background(), item.task.Sender, truncate(fmt.Sprintf("❌ [%s] 任务失败\n%s", item.task.ID, detail), d.cfg.MaxReplyChars), item.task.ID, "failed")
		return
	}
	if result.Status == agent.NeedsInput {
		if result.SessionID == "" {
			err = fmt.Errorf("Agent 请求用户回答，但没有返回可恢复的会话 ID")
			_ = d.store.SetResult(item.task.ID, store.StatusFailed, "", err.Error())
			_ = d.send(context.Background(), item.task.Sender, fmt.Sprintf("❌ [%s] %v", item.task.ID, err), item.task.ID, "failed")
			return
		}
		_ = d.store.SetWaiting(item.task.ID, result.SessionID, result.Question)
		d.log.Printf("Agent 等待回答：任务=%s 会话=%s 问题=%q", item.task.ID, result.SessionID, logText(result.Question, 1000))
		text := fmt.Sprintf("❓ [%s] %s 等待你的回答\n%s\n\n回复：#reply %s <你的回答>", item.task.ID, item.task.Agent, result.Question, item.task.ID)
		_ = d.send(context.Background(), item.task.Sender, truncate(text, d.cfg.MaxReplyChars), item.task.ID, "question")
		return
	}
	_ = d.store.SetResult(item.task.ID, store.StatusCompleted, result.Message, "")
	d.log.Printf("任务完成：任务=%s Agent=%s 结果=%q", item.task.ID, item.task.Agent, logText(result.Message, 1000))
	_ = d.send(context.Background(), item.task.Sender, truncate(fmt.Sprintf("✅ [%s] 任务完成\n%s", item.task.ID, result.Message), d.cfg.MaxReplyChars), item.task.ID, "completed")
}

type taskLogWriter struct {
	mu     sync.Mutex
	logger *log.Logger
	taskID string
	buffer strings.Builder
}

func (w *taskLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buffer.Write(p)
	value := w.buffer.String()
	for {
		index := strings.IndexByte(value, '\n')
		if index < 0 {
			break
		}
		w.print(value[:index])
		value = value[index+1:]
	}
	w.buffer.Reset()
	w.buffer.WriteString(value)
	return len(p), nil
}

func (w *taskLogWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buffer.Len() > 0 {
		w.print(w.buffer.String())
		w.buffer.Reset()
	}
}

func (w *taskLogWriter) print(value string) {
	value = strings.TrimSpace(value)
	if value != "" {
		w.logger.Printf("Agent 输出：任务=%s %s", w.taskID, logText(value, 4000))
	}
}

func agentFailure(err error, logs string) string {
	detail := strings.TrimSpace(logs)
	runes := []rune(detail)
	if len(runes) > 2000 {
		detail = string(runes[len(runes)-2000:])
	}
	if detail == "" {
		return err.Error()
	}
	return err.Error() + "\n" + detail
}

func logText(value string, limit int) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\r", "")
	value = strings.ReplaceAll(value, "\n", " ")
	runes := []rune(value)
	if len(runes) > limit {
		value = string(runes[:limit]) + "…"
	}
	return value
}

func (d *Dispatcher) send(ctx context.Context, to, text, taskID, kind string) error {
	if taskID != "" {
		_ = d.store.AddMessage(taskID, "out", kind, text, "")
	}
	return d.transport.Send(ctx, to, text)
}

func (d *Dispatcher) expireWaiting(ctx context.Context) {
	tasks, err := d.store.ExpireWaiting(time.Now().Add(-d.cfg.WaitingDuration()))
	if err != nil {
		d.log.Printf("清理超时任务: %v", err)
		return
	}
	for _, task := range tasks {
		_ = d.send(ctx, task.Sender, fmt.Sprintf("⌛ [%s] 等待回答超时，任务已结束。", task.ID), task.ID, "expired")
	}
}

func (d *Dispatcher) Check() error {
	for name, a := range d.agents {
		if err := a.Check(); err != nil {
			return fmt.Errorf("Agent %s 不可用: %w", name, err)
		}
	}
	for name, p := range d.cfg.Projects {
		info, err := os.Stat(p.Path)
		if err != nil {
			return fmt.Errorf("项目 %s 路径不可用: %w", name, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("项目 %s 路径不是目录", name)
		}
	}
	return nil
}

func (d *Dispatcher) healthLoop(ctx context.Context) {
	ticker := time.NewTicker(d.cfg.HealthDuration())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.runHealthCheck(ctx, false)
		}
	}
}

func (d *Dispatcher) runHealthCheck(ctx context.Context, initial bool) {
	names := make([]string, 0, len(d.agents))
	for name := range d.agents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		d.updateHealth(ctx, "agent:"+name, "Agent "+name, d.agents[name].Check(), initial)
	}
	projects := sortedProjectNames(d.cfg.Projects)
	checkedProjects := map[string]bool{}
	for _, name := range projects {
		checkedProjects[name] = true
		info, err := os.Stat(d.cfg.Projects[name].Path)
		if err == nil && !info.IsDir() {
			err = fmt.Errorf("路径不是目录")
		}
		d.updateHealth(ctx, "project:"+name, "项目 "+name, err, initial)
	}
	registered, err := d.store.Projects()
	if err != nil {
		d.log.Printf("检查注册项目失败: %v", err)
		return
	}
	for _, project := range registered {
		if checkedProjects[project.Name] {
			continue
		}
		info, statErr := os.Stat(project.Path)
		if statErr == nil && !info.IsDir() {
			statErr = fmt.Errorf("路径不是目录")
		}
		d.updateHealth(ctx, "project:"+project.Name, "项目 "+project.Name, statErr, initial)
	}
}

func (d *Dispatcher) updateHealth(ctx context.Context, key, label string, err error, initial bool) {
	available := err == nil
	d.healthMu.Lock()
	previous, known := d.health[key]
	d.health[key] = available
	d.healthMu.Unlock()
	if initial {
		if available {
			d.log.Printf("启动预检：%s 可用", label)
		} else {
			d.log.Printf("启动预检：%s 不可用: %v", label, err)
			d.notifyHealth(ctx, fmt.Sprintf("⚠️ Taskian 启动预检：%s 不可用\n%s", label, err))
		}
		return
	}
	if known && previous == available {
		return
	}
	if available {
		d.log.Printf("%s 已恢复", label)
		d.notifyHealth(ctx, fmt.Sprintf("✅ Taskian：%s 已恢复可用。", label))
	} else {
		d.log.Printf("%s 变为不可用: %v", label, err)
		d.notifyHealth(ctx, fmt.Sprintf("⚠️ Taskian：%s 已不可用\n%s", label, err))
	}
}

func (d *Dispatcher) notifyHealth(ctx context.Context, text string) {
	recipients := append([]string(nil), d.cfg.Health.NotifySenders...)
	if len(recipients) == 0 {
		if source, ok := d.transport.(interface{ NotificationRecipients() []string }); ok {
			recipients = source.NotificationRecipients()
		}
	}
	for _, recipient := range recipients {
		if err := d.transport.Send(ctx, recipient, text); err != nil {
			d.log.Printf("发送健康提醒给 %s 失败: %v", recipient, err)
		}
	}
}

func formatTaskStatus(t store.Task) string {
	value := fmt.Sprintf("[%s]\nAgent：%s\n项目：%s\n状态：%s", t.ID, t.Agent, t.Project, t.Status)
	if t.PendingQuestion != "" {
		value += "\n等待回答：" + t.PendingQuestion
	}
	if t.Error != "" {
		value += "\n错误：" + t.Error
	}
	return value
}
func truncate(value string, max int) string {
	value = strings.TrimSpace(value)
	if max <= 0 || utf8.RuneCountInString(value) <= max {
		return value
	}
	r := []rune(value)
	return string(r[:max]) + "\n…（内容过长，已截断）"
}
func sortedProjectNames(projects map[string]config.ProjectConfig) []string {
	names := make([]string, 0, len(projects))
	for n := range projects {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
