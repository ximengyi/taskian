package app

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/ximengyi/taskian/internal/agent"
	"github.com/ximengyi/taskian/internal/config"
	"github.com/ximengyi/taskian/internal/message"
	"github.com/ximengyi/taskian/internal/store"
)

type fakeTransport struct {
	mu   sync.Mutex
	in   []message.Incoming
	sent []string
}

func (f *fakeTransport) Name() string { return "test" }
func (f *fakeTransport) Poll(context.Context) ([]message.Incoming, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.in
	f.in = nil
	return out, nil
}
func (f *fakeTransport) Send(_ context.Context, _ string, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, text)
	return nil
}
func (f *fakeTransport) Close() error { return nil }

type fakeAgent struct {
	starts, resumes int
	checkErr        error
}

func (f *fakeAgent) Check() error { return f.checkErr }
func (f *fakeAgent) Start(context.Context, agent.Request) (agent.Result, error) {
	f.starts++
	return agent.Result{Status: agent.NeedsInput, Question: "A 还是 B？", SessionID: "session-1"}, nil
}

func TestHealthCheckNotifiesOnlyInitialFailuresAndChanges(t *testing.T) {
	dir := t.TempDir()
	state, err := store.Open(filepath.Join(dir, "taskian.db"))
	if err != nil {
		t.Fatal(err)
	}
	channel := &fakeTransport{}
	adapter := &fakeAgent{checkErr: errors.New("not logged in")}
	cfg := &config.Config{
		Health:   config.HealthConfig{NotifySenders: []string{"owner"}},
		Projects: map[string]config.ProjectConfig{"p": {Path: dir}},
	}
	d := newDispatcher(cfg, state, channel, map[string]agent.Adapter{"cursor": adapter}, log.New(io.Discard, "", 0))
	defer d.Close()
	d.runHealthCheck(context.Background(), true)
	d.runHealthCheck(context.Background(), false)
	adapter.checkErr = nil
	d.runHealthCheck(context.Background(), false)
	if len(channel.sent) != 2 {
		t.Fatalf("notifications=%d want=2: %v", len(channel.sent), channel.sent)
	}
}

func TestPersonalProjectRegistrationUseAndDefaultRouting(t *testing.T) {
	dir := t.TempDir()
	state, err := store.Open(filepath.Join(dir, "taskian.db"))
	if err != nil {
		t.Fatal(err)
	}
	channel := &fakeTransport{}
	adapter := &fakeAgent{}
	cfg := &config.Config{Mode: "personal", DefaultAgent: "codex", MaxReplyChars: 6000, WaitingUserTimeout: "72h", Projects: map[string]config.ProjectConfig{}}
	d := newDispatcher(cfg, state, channel, map[string]agent.Adapter{"codex": adapter}, log.New(io.Discard, "", 0))
	defer d.Close()
	channel.in = []message.Incoming{{ID: "p1", Channel: "test", Sender: "owner", Conversation: "owner", Body: "#project add demo " + dir, ReceivedAt: time.Now()}}
	if err := d.poll(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	channel.in = []message.Incoming{{ID: "p2", Channel: "test", Sender: "owner", Conversation: "owner", Body: "#use demo", ReceivedAt: time.Now()}}
	if err := d.poll(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	channel.in = []message.Incoming{{ID: "p3", Channel: "test", Sender: "owner", Conversation: "owner", Body: "#task\n写周报", ReceivedAt: time.Now()}}
	if err := d.poll(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if adapter.starts != 1 {
		t.Fatalf("starts=%d", adapter.starts)
	}
	waiting, err := state.WaitingFor("owner")
	if err != nil || len(waiting) != 1 {
		t.Fatalf("waiting=%v err=%v", waiting, err)
	}
	if waiting[0].ProjectPath != dir || waiting[0].Agent != "codex" {
		t.Fatalf("task=%+v", waiting[0])
	}
}

func TestPersonalPlainTextUsesDefaultAgentAndGlobalHome(t *testing.T) {
	dir := t.TempDir()
	state, err := store.Open(filepath.Join(dir, "taskian.db"))
	if err != nil {
		t.Fatal(err)
	}
	channel := &fakeTransport{}
	adapter := &fakeAgent{}
	cfg := &config.Config{Mode: "personal", DefaultAgent: "codex", MaxReplyChars: 6000, WaitingUserTimeout: "72h", Projects: map[string]config.ProjectConfig{}}
	d := newDispatcher(cfg, state, channel, map[string]agent.Adapter{"codex": adapter}, log.New(io.Discard, "", 0))
	defer d.Close()
	channel.in = []message.Incoming{{ID: "plain-task", Channel: "test", Sender: "owner", Conversation: "owner", Body: "写一下周报", ReceivedAt: time.Now()}}
	if err := d.poll(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	waiting, err := state.WaitingFor("owner")
	if err != nil || len(waiting) != 1 {
		t.Fatalf("waiting=%v err=%v", waiting, err)
	}
	if waiting[0].Project != "global" || waiting[0].Agent != "codex" || waiting[0].Prompt != "写一下周报" {
		t.Fatalf("task=%+v", waiting[0])
	}
}

func TestShutdownRequiresMatchingConfirmation(t *testing.T) {
	dir := t.TempDir()
	state, err := store.Open(filepath.Join(dir, "taskian.db"))
	if err != nil {
		t.Fatal(err)
	}
	channel := &fakeTransport{}
	cfg := &config.Config{Mode: "personal", DefaultAgent: "codex", MaxReplyChars: 6000, WaitingUserTimeout: "72h", Projects: map[string]config.ProjectConfig{}}
	d := newDispatcher(cfg, state, channel, map[string]agent.Adapter{}, log.New(io.Discard, "", 0))
	defer d.Close()
	action := ""
	d.systemRunner = func(value string) error { action = value; return nil }
	channel.in = []message.Incoming{{ID: "shutdown-1", Channel: "test", Sender: "owner", Conversation: "owner", Body: "帮我关一下机", ReceivedAt: time.Now()}}
	if err := d.poll(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if action != "" {
		t.Fatal("shutdown ran without confirmation")
	}
	match := regexp.MustCompile(`#confirm ([0-9A-F]{6})`).FindStringSubmatch(channel.sent[len(channel.sent)-1])
	if len(match) != 2 {
		t.Fatalf("confirmation message=%q", channel.sent)
	}
	channel.in = []message.Incoming{{ID: "shutdown-2", Channel: "test", Sender: "owner", Conversation: "owner", Body: "#confirm " + match[1], ReceivedAt: time.Now()}}
	if err := d.poll(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if action != "shutdown" {
		t.Fatalf("action=%q", action)
	}
}

func TestSystemActionDetectionDoesNotCaptureOrdinaryTasks(t *testing.T) {
	if got := detectSystemAction("帮我关一下机"); got != "shutdown" {
		t.Fatalf("action=%q", got)
	}
	if got := detectSystemAction("请帮我重启吧！"); got != "reboot" {
		t.Fatalf("action=%q", got)
	}
	for _, text := range []string{"修复 shutdown 命令的兼容问题", "怎么关机", "不要关机", "关闭机器上的测试服务"} {
		if got := detectSystemAction(text); got != "" {
			t.Fatalf("text=%q action=%q", text, got)
		}
	}
}

func (f *fakeAgent) Resume(_ context.Context, r agent.Request) (agent.Result, error) {
	f.resumes++
	if r.SessionID != "session-1" {
		return agent.Result{}, io.ErrUnexpectedEOF
	}
	return agent.Result{Status: agent.Completed, Message: "已按回答完成", SessionID: r.SessionID}, nil
}

func TestBidirectionalTaskResume(t *testing.T) {
	dir := t.TempDir()
	state, err := store.Open(filepath.Join(dir, "taskian.db"))
	if err != nil {
		t.Fatal(err)
	}
	channel := &fakeTransport{}
	adapter := &fakeAgent{}
	cfg := &config.Config{DatabasePath: filepath.Join(dir, "taskian.db"), MaxReplyChars: 6000, WaitingUserTimeout: "72h", Projects: map[string]config.ProjectConfig{"p": {Path: dir, AllowedAgents: []string{"codex"}}}}
	d := newDispatcher(cfg, state, channel, map[string]agent.Adapter{"codex": adapter}, log.New(io.Discard, "", 0))
	defer d.Close()
	channel.in = []message.Incoming{{ID: "m1", Channel: "test", Sender: "owner", Conversation: "owner", Body: "#codex p\nplease work", ReceivedAt: time.Now()}}
	if err := d.poll(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	waiting, err := state.WaitingFor("owner")
	if err != nil || len(waiting) != 1 {
		t.Fatalf("waiting=%v err=%v", waiting, err)
	}
	// With exactly one waiting task, a normal message is routed as its answer.
	channel.in = []message.Incoming{{ID: "m2", Channel: "test", Sender: "owner", Conversation: "owner", Body: "B", ReceivedAt: time.Now()}}
	if err := d.poll(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	done, err := state.Task(waiting[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != store.StatusCompleted || adapter.starts != 1 || adapter.resumes != 1 {
		t.Fatalf("task=%+v starts=%d resumes=%d", done, adapter.starts, adapter.resumes)
	}
}

func TestPlainTextIsNotRoutedWhenMultipleTasksWait(t *testing.T) {
	dir := t.TempDir()
	state, err := store.Open(filepath.Join(dir, "taskian.db"))
	if err != nil {
		t.Fatal(err)
	}
	channel := &fakeTransport{}
	cfg := &config.Config{MaxReplyChars: 6000, WaitingUserTimeout: "72h"}
	d := newDispatcher(cfg, state, channel, map[string]agent.Adapter{}, log.New(io.Discard, "", 0))
	defer d.Close()
	for _, id := range []string{"a", "b"} {
		task, err := state.CreateTask(store.Task{SourceMessageID: id, Channel: "test", Sender: "owner", Conversation: "owner", Agent: "codex", Project: "p", ProjectPath: dir, Prompt: id})
		if err != nil {
			t.Fatal(err)
		}
		if err := state.SetWaiting(task.ID, "session-"+id, "question"); err != nil {
			t.Fatal(err)
		}
	}
	channel.in = []message.Incoming{{ID: "plain", Channel: "test", Sender: "owner", Conversation: "owner", Body: "choose B", ReceivedAt: time.Now()}}
	if err := d.poll(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	waiting, err := state.WaitingFor("owner")
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting) != 2 {
		t.Fatalf("waiting=%d", len(waiting))
	}
}
