package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"taskian.local/taskian/internal/config"
	"taskian.local/taskian/internal/message"
)

type Result struct {
	Text     string
	Logs     string
	Duration time.Duration
}

func Run(ctx context.Context, task message.Task, agent config.AgentConfig, project config.ProjectConfig) (Result, error) {
	timeout, _ := time.ParseDuration(agent.Timeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	outputFile, err := os.CreateTemp("", "taskian-result-*.md")
	if err != nil {
		return Result{}, err
	}
	outputPath := outputFile.Name()
	_ = outputFile.Close()
	defer os.Remove(outputPath)

	replacer := strings.NewReplacer(
		"{prompt}", task.Prompt,
		"{output}", outputPath,
		"{project}", project.Path,
	)
	args := make([]string, len(agent.Args))
	for i, value := range agent.Args {
		args[i] = replacer.Replace(value)
	}

	cmd := exec.CommandContext(ctx, agent.Command, args...)
	cmd.Dir = project.Path
	cmd.Env = os.Environ()
	for key, value := range agent.Env {
		cmd.Env = append(cmd.Env, key+"="+os.ExpandEnv(value))
	}
	var combined limitedBuffer
	combined.max = 128 * 1024
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	started := time.Now()
	err = cmd.Run()
	duration := time.Since(started)

	data, readErr := os.ReadFile(outputPath)
	text := strings.TrimSpace(string(data))
	if text == "" {
		text = strings.TrimSpace(combined.String())
	}
	result := Result{Text: text, Logs: combined.String(), Duration: duration}
	if ctx.Err() == context.DeadlineExceeded {
		return result, fmt.Errorf("agent 执行超过 %s", timeout)
	}
	if err != nil {
		return result, fmt.Errorf("agent 退出失败: %w", err)
	}
	if readErr != nil && !os.IsNotExist(readErr) {
		return result, fmt.Errorf("读取 agent 输出: %w", readErr)
	}
	if result.Text == "" {
		result.Text = "任务执行完成，但 agent 没有返回文字结果。"
	}
	return result, nil
}

type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := b.max - b.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return original, nil
}
