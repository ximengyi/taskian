package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ximengyi/taskian/internal/config"
)

type envelope struct {
	Status   string `json:"status"`
	Message  string `json:"message"`
	Question string `json:"question"`
}

func runCommand(parent context.Context, cfg config.AgentConfig, dir string, args []string, live io.Writer) (string, error) {
	timeout, _ := time.ParseDuration(cfg.Timeout)
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.Command, args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+os.ExpandEnv(v))
	}
	var output limitedBuffer
	output.max = 2 << 20
	writer := io.Writer(&output)
	if live != nil {
		writer = io.MultiWriter(&output, live)
	}
	cmd.Stdout = writer
	cmd.Stderr = writer
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return output.String(), fmt.Errorf("agent 执行超过 %s", timeout)
	}
	if err != nil {
		return output.String(), fmt.Errorf("agent 退出失败: %w", err)
	}
	return output.String(), nil
}

func checkCommand(cfg config.AgentConfig, args ...string) error {
	path, err := exec.LookPath(cfg.Command)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+os.ExpandEnv(v))
	}
	var output limitedBuffer
	output.max = 16 << 10
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("登录状态检查超时")
		}
		message := strings.TrimSpace(output.String())
		if message != "" {
			return fmt.Errorf("登录状态检查失败: %s", message)
		}
		return fmt.Errorf("登录状态检查失败: %w", err)
	}
	return nil
}

func parseEnvelope(text string) (envelope, error) {
	text = strings.TrimSpace(text)
	candidates := []string{text}
	if start := strings.LastIndex(text, "{"); start >= 0 {
		candidates = append(candidates, text[start:])
	}
	if start := strings.LastIndex(text, "```json"); start >= 0 {
		value := strings.TrimSpace(strings.TrimSuffix(text[start+7:], "```"))
		candidates = append(candidates, value)
	}
	for _, candidate := range candidates {
		var e envelope
		if json.Unmarshal([]byte(candidate), &e) == nil && (e.Status == Completed || e.Status == NeedsInput) {
			if e.Status == NeedsInput && strings.TrimSpace(e.Question) == "" {
				continue
			}
			return e, nil
		}
	}
	return envelope{}, fmt.Errorf("agent 未返回有效的 Taskian 结果信封")
}

type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remain := b.max - b.Len()
	if remain > 0 {
		if len(p) > remain {
			p = p[:remain]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}
