package runner

import (
	"context"
	"os"
	"strings"
	"testing"

	"taskian.local/taskian/internal/config"
	"taskian.local/taskian/internal/message"
)

func TestRunSubstitutesPromptAndOutput(t *testing.T) {
	agent := config.AgentConfig{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperProcess", "--", "{output}", "{prompt}"},
		Timeout: "5s",
		Env:     map[string]string{"TASKIAN_HELPER_PROCESS": "1"},
	}
	project := config.ProjectConfig{Path: t.TempDir(), Agent: "test"}
	result, err := Run(context.Background(), message.Task{Prompt: "hello taskian"}, agent, project)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "result: hello taskian" {
		t.Fatalf("unexpected result %q", result.Text)
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("TASKIAN_HELPER_PROCESS") != "1" {
		return
	}
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || len(os.Args) < separator+3 {
		os.Exit(2)
	}
	output, prompt := os.Args[separator+1], strings.Join(os.Args[separator+2:], " ")
	if err := os.WriteFile(output, []byte("result: "+prompt), 0o600); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}
