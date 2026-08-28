package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gofrs/flock"
	"github.com/ximengyi/taskian/internal/agent"
	"github.com/ximengyi/taskian/internal/config"
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
	cfg       *config.Config
	store     *store.Store
	transport transport.Transport
	agents    map[string]agent.Adapter
	log       *log.Logger
	queue     chan work
	runningMu sync.Mutex
	running   map[string]context.CancelFunc
	workerWG  sync.WaitGroup
	healthMu  sync.Mutex
	health    map[string]bool
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
	var channel transport.Transport
	if cfg.Channel.Type == "ilink" {
		channel, err = ilink.New(cfg.Channel, state)
	} else {
		channel, err = transport.NewFiles(cfg.InboxDir, cfg.OutboxDir)
	}
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

func newDispatcher(cfg *config.Config, state *store.Store, channel transport.Transport, adapters map[string]agent.Adapter, logger *log.Logger) *Dispatcher {
	return &Dispatcher{cfg: cfg, store: state, transport: channel, agents: adapters, log: logger, queue: make(chan work, 128), running: map[string]context.CancelFunc{}, health: map[string]bool{}}
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
	d.log.Printf("Taskian 0.4 已启动，通道=%s，并发=%d", d.transport.Name(), d.cfg.MaxConcurrentTasks)
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
			if errors.Is(err, ilink.ErrSessionExpired) {
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
	if d.transport.Name() == "wechatian-files" && d.cfg.SkipExistingOnFirstRun {
		bootstrapped, e := d.store.GetChannelState("wechatian-files.bootstrapped")
		if e != nil {
			return e
		}
		if bootstrapped == "" {
			for _, m := range messages {
				_, _ = d.store.ClaimInbound(m.Channel, m.ID, m.Sender, m.Body, m.ReceivedAt)
				_ = d.store.MarkInbound(m.Channel, m.ID, "bootstrap-skipped")
			}
			_ = d.store.SetChannelState("wechatian-files.bootstrapped", "true")
			d.log.Printf("首次运行已跳过 %d 条历史消息", len(messages))
			return nil
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
		waiting, waitErr := d.store.WaitingFor(in.Sender)
		if waitErr != nil {
			return waitErr
		}
		if len(waiting) == 1 {
			return d.handleReply(ctx, in, message.Command{Kind: message.CommandReply, TaskID: waiting[0].ID, Text: strings.TrimSpace(in.Body)}, async)
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
	default:
		return nil
	}
}

func (d *Dispatcher) handleTask(ctx context.Context, in message.Incoming, task message.Task, async bool) error {
	if task.Agent == "task" || task.Agent == "taskian" || task.Agent == "" {
		task.Agent = d.cfg.DefaultAgent
	}
	projectName, projectPath, prompt, err := d.resolveTaskTarget(in, task)
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
	record, err := d.store.CreateTask(store.Task{SourceMessageID: in.ID, Channel: in.Channel, Sender: in.Sender, Conversation: in.Conversation, Agent: task.Agent, Project: projectName, ProjectPath: projectPath, Prompt: prompt, Status: store.StatusQueued})
	if err != nil {
		return err
	}
	_ = d.store.MarkInbound(in.Channel, in.ID, "accepted")
	if err := d.send(ctx, in.Sender, fmt.Sprintf("✅ [%s] 已接收\nAgent：%s\n项目：%s", record.ID, record.Agent, record.Project), record.ID, "accepted"); err != nil {
		return err
	}
	item := work{task: record}
	if async {
		d.queue <- item
	} else {
		d.execute(ctx, item)
	}
	return nil
}

func (d *Dispatcher) resolveTaskTarget(in message.Incoming, task message.Task) (name, path, prompt string, err error) {
	target := strings.TrimSpace(task.Project)
	prompt = strings.TrimSpace(task.Prompt)
	resolve := func(value string) (string, string, bool) {
		key := strings.ToLower(strings.TrimSpace(value))
		if p, ok := d.cfg.Projects[key]; ok {
			return key, p.Path, true
		}
		if p, e := d.store.Project(key); e == nil {
			_ = d.store.TouchProject(key)
			return p.Name, p.Path, true
		}
		if filepath.IsAbs(value) {
			info, e := os.Stat(value)
			if e == nil && info.IsDir() {
				return filepath.Base(filepath.Clean(value)), filepath.Clean(value), true
			}
		}
		return "", "", false
	}
	if target != "" {
		if name, path, ok := resolve(target); ok {
			return name, path, prompt, nil
		}
	}
	current, e := d.store.ConversationProject(in.Channel, in.Conversation)
	if e != nil {
		return "", "", "", e
	}
	if current == "" {
		current = d.cfg.DefaultProject
	}
	if current != "" {
		if name, path, ok := resolve(current); ok {
			if target != "" {
				prompt = strings.TrimSpace(target + " " + prompt)
			}
			if prompt == "" {
				return "", "", "", fmt.Errorf("任务内容不能为空")
			}
			return name, path, prompt, nil
		}
	}
	if target == "" {
		return "", "", "", fmt.Errorf("尚未选择项目，请使用 #use <项目> 或在任务中指定项目名称/绝对路径")
	}
	return "", "", "", fmt.Errorf("找不到项目或目录 %q；可先发送 #project add <名称> <绝对路径>", target)
}

func (d *Dispatcher) handleUse(ctx context.Context, in message.Incoming, name string) error {
	p, err := d.store.Project(name)
	if errors.Is(err, sql.ErrNoRows) {
		if configured, ok := d.cfg.Projects[name]; ok {
			p = store.Project{Name: name, Path: configured.Path}
		} else {
			return d.send(ctx, in.Sender, "⚠️ 找不到项目 "+name, "", "project")
		}
	} else if err != nil {
		return err
	}
	if err := d.store.SetConversationProject(in.Channel, in.Conversation, p.Name); err != nil {
		return err
	}
	_ = d.store.TouchProject(p.Name)
	_ = d.store.MarkInbound(in.Channel, in.ID, "project-use")
	return d.send(ctx, in.Sender, fmt.Sprintf("✅ 当前项目：%s\n目录：%s", p.Name, p.Path), "", "project")
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
		if err := d.store.PutProject(name, path); err != nil {
			return err
		}
		_ = d.store.MarkInbound(in.Channel, in.ID, "project-add")
		return d.send(ctx, in.Sender, fmt.Sprintf("✅ 已注册项目 %s\n%s", name, path), "", "project")
	case "list":
		projects, err := d.store.Projects()
		if err != nil {
			return err
		}
		current, _ := d.store.ConversationProject(in.Channel, in.Conversation)
		lines := []string{"项目列表："}
		for _, p := range projects {
			mark := ""
			if p.Name == current {
				mark = "（当前）"
			}
			lines = append(lines, fmt.Sprintf("- %s%s：%s", p.Name, mark, p.Path))
		}
		for name, p := range d.cfg.Projects {
			lines = append(lines, fmt.Sprintf("- %s（配置）：%s", name, p.Path))
		}
		if len(lines) == 1 {
			lines = append(lines, "尚未注册项目。使用：#project add <名称> <绝对路径>")
		}
		_ = d.store.MarkInbound(in.Channel, in.ID, "project-list")
		return d.send(ctx, in.Sender, strings.Join(lines, "\n"), "", "project")
	case "show":
		if len(args) != 1 {
			return d.send(ctx, in.Sender, "格式：#project show <名称>", "", "project")
		}
		p, err := d.store.Project(args[0])
		if err != nil {
			return d.send(ctx, in.Sender, "⚠️ 找不到项目 "+args[0], "", "project")
		}
		return d.send(ctx, in.Sender, fmt.Sprintf("项目：%s\n目录：%s\n最近使用：%s", p.Name, p.Path, p.LastUsedAt.Local().Format("2006-01-02 15:04")), "", "project")
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
	case "remove":
		if len(args) != 1 {
			return d.send(ctx, in.Sender, "格式：#project remove <名称>", "", "project")
		}
		if err := d.store.RemoveProject(args[0]); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return d.send(ctx, in.Sender, "⚠️ 找不到项目 "+args[0], "", "project")
			}
			return err
		}
		return d.send(ctx, in.Sender, "✅ 已删除项目别名 "+args[0]+"；磁盘文件未改动。", "", "project")
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
	current, _ := d.store.ConversationProject(in.Channel, in.Conversation)
	if current == "" {
		current = "未设置"
	}
	agents := make([]string, 0, len(d.agents))
	for name := range d.agents {
		agents = append(agents, name)
	}
	sort.Strings(agents)
	header := fmt.Sprintf("Taskian 0.4 帮助\n默认 Agent：%s\n可用 Agent：%s\n当前项目：%s\n", d.cfg.DefaultAgent, strings.Join(agents, "、"), current)
	switch strings.ToLower(strings.TrimSpace(topic)) {
	case "project":
		return header + "\n#project add <名称> <绝对路径>\n#project list\n#project show <名称>\n#project rename <旧名> <新名>\n#project remove <名称>\n#project find <目录名> <根目录>\n#use <项目名称>"
	case "task", "agent":
		return header + "\n#task <项目> <任务>  使用默认 Agent\n#codex <项目> <任务>\n#cursor <项目> <任务>\n设置 #use 后可以省略项目。\n#reply <任务号> <回答>\n#status [任务号]\n#cancel <任务号>"
	case "examples":
		return header + "\n示例：\n#project add week-report D:\\work\\week-report\n#use week-report\n#cursor 写一下本周周报\n#task /srv/code/demo 运行测试\n#reply T-12345678 选择 B"
	default:
		return header + "\n任务：#task / #codex / #cursor\n回答：#reply <任务号> <回答>\n状态：#status / #cancel\n项目：#project / #use\n更多：help task、help project"
	}
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
	request := agent.Request{Prompt: item.task.Prompt, ProjectPath: item.task.ProjectPath, SessionID: item.task.AgentSessionID}
	if item.resume {
		request.Prompt = item.answer
	}
	var result agent.Result
	var err error
	if item.resume {
		result, err = adapter.Resume(ctx, request)
	} else {
		result, err = adapter.Start(ctx, request)
	}
	if result.SessionID != "" {
		_ = d.store.SetSession(item.task.ID, result.SessionID)
		item.task.AgentSessionID = result.SessionID
	}
	if err != nil {
		current, e := d.store.Task(item.task.ID)
		if e == nil && current.Status == store.StatusCancelled {
			return
		}
		_ = d.store.SetResult(item.task.ID, store.StatusFailed, "", err.Error())
		_ = d.send(context.Background(), item.task.Sender, truncate(fmt.Sprintf("❌ [%s] 任务失败\n%s", item.task.ID, err), d.cfg.MaxReplyChars), item.task.ID, "failed")
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
		text := fmt.Sprintf("❓ [%s] %s 等待你的回答\n%s\n\n回复：#reply %s <你的回答>", item.task.ID, item.task.Agent, result.Question, item.task.ID)
		_ = d.send(context.Background(), item.task.Sender, truncate(text, d.cfg.MaxReplyChars), item.task.ID, "question")
		return
	}
	_ = d.store.SetResult(item.task.ID, store.StatusCompleted, result.Message, "")
	_ = d.send(context.Background(), item.task.Sender, truncate(fmt.Sprintf("✅ [%s] 任务完成\n%s", item.task.ID, result.Message), d.cfg.MaxReplyChars), item.task.ID, "completed")
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
