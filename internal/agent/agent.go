package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/ximengyi/taskian/internal/config"
)

const (
	Completed  = "completed"
	NeedsInput = "needs_input"
)

type Request struct{ Prompt, ProjectPath, SessionID string }
type Result struct{ Status, Message, Question, SessionID, Logs string }

type Adapter interface {
	Start(context.Context, Request) (Result, error)
	Resume(context.Context, Request) (Result, error)
	Check() error
}

func New(cfg config.AgentConfig) (Adapter, error) {
	switch cfg.Type {
	case "codex":
		return &Codex{cfg: cfg}, nil
	case "cursor":
		return &Cursor{cfg: cfg}, nil
	case "generic":
		return &Generic{cfg: cfg}, nil
	default:
		return nil, fmt.Errorf("不支持的 agent 类型 %q", cfg.Type)
	}
}

const contract = `

TASKIAN RESPONSE CONTRACT:
Work autonomously when it is safe and the request is sufficiently specified. Your final response MUST be only one JSON object, without Markdown fences or surrounding text:
{"status":"completed","message":"concise user-visible result","question":""}
If and only if you cannot continue without a decision or missing information from the user, stop safely and return:
{"status":"needs_input","message":"brief context","question":"one concrete question for the user"}
Never treat a request for higher privileges, disabled sandboxing, deployment, push, or destructive action as ordinary clarification. Refuse unsafe elevation in the completed response.`

func withContract(prompt string) string { return strings.TrimSpace(prompt) + contract }
func resumePrompt(answer string) string {
	return "The user answered the pending question:\n" + strings.TrimSpace(answer) + "\nContinue the same task." + contract
}
