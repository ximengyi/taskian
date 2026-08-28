package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/ximengyi/taskian/internal/config"
)

type Codex struct{ cfg config.AgentConfig }

func (c *Codex) Check() error                                         { _, err := exec.LookPath(c.cfg.Command); return err }
func (c *Codex) Start(ctx context.Context, r Request) (Result, error) { return c.run(ctx, r, false) }
func (c *Codex) Resume(ctx context.Context, r Request) (Result, error) {
	if r.SessionID == "" {
		return Result{}, fmt.Errorf("Codex thread_id 为空")
	}
	return c.run(ctx, r, true)
}

func (c *Codex) run(ctx context.Context, r Request, resume bool) (Result, error) {
	dir, err := os.MkdirTemp("", "taskian-codex-*")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)
	schema := dir + string(os.PathSeparator) + "schema.json"
	output := dir + string(os.PathSeparator) + "output.json"
	if err := os.WriteFile(schema, []byte(resultSchema), 0o600); err != nil {
		return Result{}, err
	}
	var args []string
	if resume {
		args = []string{"exec", "resume", "--json", "--output-schema", schema, "-o", output}
		if c.cfg.Model != "" {
			args = append(args, "-m", c.cfg.Model)
		}
		args = append(args, c.cfg.Args...)
		args = append(args, r.SessionID, resumePrompt(r.Prompt))
	} else {
		args = []string{"exec", "--json", "--output-schema", schema, "-o", output, "-C", r.ProjectPath}
		if c.cfg.Sandbox != "" {
			args = append(args, "-s", c.cfg.Sandbox)
		}
		if c.cfg.Model != "" {
			args = append(args, "-m", c.cfg.Model)
		}
		args = append(args, c.cfg.Args...)
		args = append(args, withContract(r.Prompt))
	}
	logs, runErr := runCommand(ctx, c.cfg, r.ProjectPath, args)
	sessionID := codexSessionID(logs)
	data, readErr := os.ReadFile(output)
	if runErr != nil {
		return Result{SessionID: sessionID, Logs: logs}, runErr
	}
	if readErr != nil {
		return Result{SessionID: sessionID, Logs: logs}, fmt.Errorf("读取 Codex 最终输出: %w", readErr)
	}
	e, err := parseEnvelope(string(data))
	if err != nil {
		return Result{SessionID: sessionID, Logs: logs}, err
	}
	if sessionID == "" {
		sessionID = r.SessionID
	}
	return Result{Status: e.Status, Message: e.Message, Question: e.Question, SessionID: sessionID, Logs: logs}, nil
}

func codexSessionID(logs string) string {
	scanner := bufio.NewScanner(strings.NewReader(logs))
	for scanner.Scan() {
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Type == "thread.started" && event.ThreadID != "" {
			return event.ThreadID
		}
	}
	return ""
}

const resultSchema = `{"type":"object","properties":{"status":{"type":"string","enum":["completed","needs_input"]},"message":{"type":"string"},"question":{"type":"string"}},"required":["status","message","question"],"additionalProperties":false}`
