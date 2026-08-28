package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/ximengyi/taskian/internal/config"
)

type Cursor struct{ cfg config.AgentConfig }

func (c *Cursor) Check() error                                         { _, err := exec.LookPath(c.cfg.Command); return err }
func (c *Cursor) Start(ctx context.Context, r Request) (Result, error) { return c.run(ctx, r, false) }
func (c *Cursor) Resume(ctx context.Context, r Request) (Result, error) {
	if r.SessionID == "" {
		return Result{}, fmt.Errorf("Cursor session_id 为空")
	}
	return c.run(ctx, r, true)
}

func (c *Cursor) run(ctx context.Context, r Request, resume bool) (Result, error) {
	args := []string{"-p", "--output-format", "stream-json", "--trust", "--workspace", r.ProjectPath}
	if c.cfg.Model != "" {
		args = append(args, "--model", c.cfg.Model)
	}
	if c.cfg.Force {
		args = append(args, "--force")
	}
	args = append(args, c.cfg.Args...)
	if resume {
		args = append(args, "--resume", r.SessionID, resumePrompt(r.Prompt))
	} else {
		args = append(args, withContract(r.Prompt))
	}
	logs, err := runCommand(ctx, c.cfg, r.ProjectPath, args)
	session, text := parseCursorOutput(logs)
	if session == "" {
		session = r.SessionID
	}
	if err != nil {
		return Result{SessionID: session, Logs: logs}, err
	}
	e, err := parseEnvelope(text)
	if err != nil {
		return Result{SessionID: session, Logs: logs}, err
	}
	return Result{Status: e.Status, Message: e.Message, Question: e.Question, SessionID: session, Logs: logs}, nil
}

func parseCursorOutput(logs string) (sessionID, text string) {
	scanner := bufio.NewScanner(strings.NewReader(logs))
	scanner.Buffer(make([]byte, 64*1024), 2<<20)
	for scanner.Scan() {
		var v map[string]any
		if json.Unmarshal(scanner.Bytes(), &v) != nil {
			continue
		}
		if id, _ := v["session_id"].(string); id != "" {
			sessionID = id
		}
		if value, _ := v["result"].(string); value != "" {
			text = value
		}
		if message, ok := v["message"].(map[string]any); ok {
			if content, _ := message["content"].(string); content != "" {
				text = content
			}
		}
		if content, _ := v["content"].(string); content != "" {
			text = content
		}
	}
	return sessionID, text
}
