package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/ximengyi/taskian/internal/config"
)

type Generic struct{ cfg config.AgentConfig }

func (g *Generic) Check() error { _, err := exec.LookPath(g.cfg.Command); return err }
func (g *Generic) Start(ctx context.Context, r Request) (Result, error) {
	return g.run(ctx, r, g.cfg.Args)
}
func (g *Generic) Resume(ctx context.Context, r Request) (Result, error) {
	if len(g.cfg.ResumeArgs) == 0 {
		return Result{}, fmt.Errorf("generic agent 未配置 resume_args，无法双向恢复")
	}
	return g.run(ctx, r, g.cfg.ResumeArgs)
}
func (g *Generic) run(ctx context.Context, r Request, template []string) (Result, error) {
	out, err := os.CreateTemp("", "taskian-generic-*.txt")
	if err != nil {
		return Result{}, err
	}
	path := out.Name()
	_ = out.Close()
	defer os.Remove(path)
	replacer := strings.NewReplacer("{prompt}", r.Prompt, "{project}", r.ProjectPath, "{session}", r.SessionID, "{output}", path)
	args := make([]string, len(template))
	for i, v := range template {
		args[i] = replacer.Replace(v)
	}
	logs, runErr := runCommand(ctx, g.cfg, r.ProjectPath, args, r.Output)
	data, _ := os.ReadFile(path)
	text := strings.TrimSpace(string(data))
	if text == "" {
		text = strings.TrimSpace(logs)
	}
	if runErr != nil {
		return Result{Logs: logs}, runErr
	}
	return Result{Status: Completed, Message: text, Logs: logs}, nil
}
