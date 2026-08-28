package agent

import (
	"slices"
	"testing"

	"github.com/ximengyi/taskian/internal/config"
)

func TestParseEnvelope(t *testing.T) {
	e, err := parseEnvelope(`{"status":"needs_input","message":"need choice","question":"A or B?"}`)
	if err != nil {
		t.Fatal(err)
	}
	if e.Status != NeedsInput || e.Question != "A or B?" {
		t.Fatalf("unexpected: %+v", e)
	}
	if _, err := parseEnvelope("please answer me"); err == nil {
		t.Fatal("expected strict envelope error")
	}
}

func TestCodexAndCursorSessionParsing(t *testing.T) {
	logs := "{\"type\":\"thread.started\",\"thread_id\":\"abc-123\"}\n{\"type\":\"turn.completed\"}\n"
	if got := codexSessionID(logs); got != "abc-123" {
		t.Fatalf("session=%q", got)
	}
	cursorLogs := "{\"type\":\"system\",\"session_id\":\"chat-7\"}\n{\"type\":\"result\",\"session_id\":\"chat-7\",\"result\":\"{\\\"status\\\":\\\"completed\\\",\\\"message\\\":\\\"done\\\",\\\"question\\\":\\\"\\\"}\"}\n"
	session, text := parseCursorOutput(cursorLogs)
	if session != "chat-7" {
		t.Fatalf("session=%q", session)
	}
	if _, err := parseEnvelope(text); err != nil {
		t.Fatal(err)
	}
}

func TestCodexArgsAllowNonGitDirectory(t *testing.T) {
	for _, resume := range []bool{false, true} {
		args := codexArgs(config.AgentConfig{}, Request{Prompt: "test", ProjectPath: t.TempDir(), SessionID: "thread-1"}, resume, "schema.json", "output.json")
		if !slices.Contains(args, "--skip-git-repo-check") {
			t.Fatalf("resume=%v args=%v", resume, args)
		}
	}
}
