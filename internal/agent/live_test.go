package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ximengyi/taskian/internal/config"
)

// This test is intentionally opt-in because it uses the locally authenticated
// Agent account. It validates the real CLI session-id and resume contracts
// before a release: TASKIAN_LIVE_AGENT_TEST=codex|cursor go test ./internal/agent -run Live.
func TestLiveNativeResume(t *testing.T) {
	kind := os.Getenv("TASKIAN_LIVE_AGENT_TEST")
	if kind != "codex" && kind != "cursor" {
		t.Skip("live Agent test disabled")
	}
	command := kind
	if kind == "cursor" {
		command = "agent"
	}
	cfg := config.AgentConfig{Type: kind, Command: command, Timeout: "10m"}
	if kind == "codex" {
		cfg.Sandbox = "read-only"
	}
	adapter, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	project, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	first, err := adapter.Start(ctx, Request{ProjectPath: project, Prompt: "Do not use tools and do not modify files. The task cannot continue until I choose either blue or green. Ask me to choose one."})
	if err != nil {
		t.Fatalf("%v\n%s", err, first.Logs)
	}
	if first.Status != NeedsInput || first.SessionID == "" || first.Question == "" {
		t.Fatalf("unexpected first result: %+v", first)
	}
	second, err := adapter.Resume(ctx, Request{ProjectPath: project, SessionID: first.SessionID, Prompt: "blue"})
	if err != nil {
		t.Fatalf("%v\n%s", err, second.Logs)
	}
	if second.Status != Completed || second.SessionID == "" {
		t.Fatalf("unexpected resumed result: %+v", second)
	}
}
