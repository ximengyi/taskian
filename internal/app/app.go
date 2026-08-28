package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	adapters := make(map[string]agent.Adapter, len(cfg.Agents))
	for name, agentCfg := range cfg.Agents {
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
	return &Dispatcher{cfg: cfg, store: state, transport: channel, agents: adapters, log: logger, queue: make(chan work, 128), running: map[string]context.CancelFunc{}}
}

func (d *Dispatcher) Close() error { _ = d.transport.Close(); return d.store.Close() }

func (d *Dispatcher) Serve(ctx context.Context) error {
	defer d.Close()
	workerCtx, stopWorkers := context.WithCancel(ctx)
	defer d.workerWG.Wait()
	defer stopWorkers()
	if err := d.store.RecoverInterrupted(); err != nil {
		return err
	}
	d.log.Printf("Taskian 0.2 已启动，通道=%s，并发=%d", d.transport.Name(), d.cfg.MaxConcurrentTasks)
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
		return d.send(ctx, in.Sender, helpText, "", "help")
	default:
		return nil
	}
}

func (d *Dispatcher) handleTask(ctx context.Context, in message.Incoming, task message.Task, async bool) error {
	project, ok := d.cfg.Projects[task.Project]
	if !ok {
		_ = d.store.MarkInbound(in.Channel, in.ID, "rejected")
		return d.send(ctx, in.Sender, fmt.Sprintf("⛔ 未授权的项目：%s\n允许的项目：%s", task.Project, strings.Join(sortedProjectNames(d.cfg.Projects), "、")), "", "rejected")
	}
	if !project.Allows(task.Agent) {
		_ = d.store.MarkInbound(in.Channel, in.ID, "rejected")
		return d.send(ctx, in.Sender, fmt.Sprintf("⛔ 项目 %s 不允许使用 %s。", task.Project, task.Agent), "", "rejected")
	}
	if _, ok := d.agents[task.Agent]; !ok {
		_ = d.store.MarkInbound(in.Channel, in.ID, "rejected")
		return d.send(ctx, in.Sender, "⛔ 未配置 Agent："+task.Agent, "", "rejected")
	}
	record, err := d.store.CreateTask(store.Task{SourceMessageID: in.ID, Channel: in.Channel, Sender: in.Sender, Conversation: in.Conversation, Agent: task.Agent, Project: task.Project, ProjectPath: project.Path, Prompt: task.Prompt, Status: store.StatusQueued})
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

const helpText = `Taskian 命令：
#codex <项目>       创建 Codex 任务
#taskian <Agent> <项目>  创建指定 Agent 任务
#reply <任务号> <回答>   回答 Agent 问题
#status [任务号]         查看状态
#cancel <任务号>         取消任务
#help                    显示帮助`
