package message

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseFileOnlyReadsReceivedQuotes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "2026-08-28.md")
	content := `---
date: 2026-08-28
---

# 微信收件箱

**00:03** · 接收

> #codex yuanze
> 修复移动端布局

**00:04** · 发送

> 这不是任务
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	items, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d messages, want 1", len(items))
	}
	if items[0].Body != "#codex yuanze\n修复移动端布局" {
		t.Fatalf("unexpected body: %q", items[0].Body)
	}
}

func TestParseReplyCommand(t *testing.T) {
	command, err := ParseCommand(Incoming{Body: "#reply   T-ABC123 选择 B\n并保留旧功能"})
	if err != nil {
		t.Fatal(err)
	}
	if command.Kind != CommandReply || command.TaskID != "T-ABC123" || command.Text != "选择 B\n并保留旧功能" {
		t.Fatalf("unexpected: %+v", command)
	}
}

func TestParseReplyCommandWithAnswerOnNextLine(t *testing.T) {
	command, err := ParseCommand(Incoming{Body: "#reply T-ABC123\n选择 B"})
	if err != nil {
		t.Fatal(err)
	}
	if command.TaskID != "T-ABC123" || command.Text != "选择 B" {
		t.Fatalf("unexpected: %+v", command)
	}
}

func TestParseTaskShortcut(t *testing.T) {
	task, err := ParseTask(Incoming{Body: "#codex yuanze\n运行测试并修复失败项"})
	if err != nil {
		t.Fatal(err)
	}
	if task.Agent != "codex" || task.Project != "yuanze" || task.Prompt != "运行测试并修复失败项" {
		t.Fatalf("unexpected task: %#v", task)
	}
}

func TestParseTaskLongForm(t *testing.T) {
	task, err := ParseTask(Incoming{Body: "#taskian codex yuanze\n检查代码"})
	if err != nil {
		t.Fatal(err)
	}
	if task.Agent != "codex" || task.Project != "yuanze" {
		t.Fatalf("unexpected task: %#v", task)
	}
}
