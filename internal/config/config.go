package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	VaultPath              string                   `json:"vault_path"`
	InboxDir               string                   `json:"inbox_dir"`
	OutboxDir              string                   `json:"outbox_dir"`
	StatePath              string                   `json:"state_path"`
	PollInterval           string                   `json:"poll_interval"`
	SkipExistingOnFirstRun bool                     `json:"skip_existing_on_first_run"`
	MaxReplyChars          int                      `json:"max_reply_chars"`
	Agents                 map[string]AgentConfig   `json:"agents"`
	Projects               map[string]ProjectConfig `json:"projects"`
}

type AgentConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Timeout string            `json:"timeout"`
	Env     map[string]string `json:"env,omitempty"`
}

type ProjectConfig struct {
	Path  string `json:"path"`
	Agent string `json:"agent"`
}

func Load(path string) (*Config, error) {
	path = expandPath(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %q: %w", path, err)
	}
	cfg := Config{SkipExistingOnFirstRun: true}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("解析配置 %q: %w", path, err)
	}
	applyDefaults(&cfg)
	cfg.VaultPath = expandPath(cfg.VaultPath)
	cfg.InboxDir = resolveUnder(cfg.VaultPath, cfg.InboxDir)
	cfg.OutboxDir = resolveUnder(cfg.VaultPath, cfg.OutboxDir)
	cfg.StatePath = expandPath(cfg.StatePath)
	projects := make(map[string]ProjectConfig, len(cfg.Projects))
	for name, project := range cfg.Projects {
		project.Path = expandPath(project.Path)
		project.Agent = strings.ToLower(project.Agent)
		projects[strings.ToLower(name)] = project
	}
	cfg.Projects = projects
	agents := make(map[string]AgentConfig, len(cfg.Agents))
	for name, agent := range cfg.Agents {
		agents[strings.ToLower(name)] = agent
	}
	cfg.Agents = agents
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) Validate() error {
	if c.VaultPath == "" || c.InboxDir == "" || c.OutboxDir == "" {
		return fmt.Errorf("vault_path、inbox_dir 和 outbox_dir 不能为空")
	}
	if _, err := time.ParseDuration(c.PollInterval); err != nil {
		return fmt.Errorf("poll_interval 无效: %w", err)
	}
	if len(c.Agents) == 0 || len(c.Projects) == 0 {
		return fmt.Errorf("至少配置一个 agent 和一个项目")
	}
	for name, agent := range c.Agents {
		if name == "" || agent.Command == "" {
			return fmt.Errorf("agent 名称和 command 不能为空")
		}
		if _, err := time.ParseDuration(agent.Timeout); err != nil {
			return fmt.Errorf("agent %q timeout 无效: %w", name, err)
		}
		joined := strings.Join(agent.Args, "\x00")
		if !strings.Contains(joined, "{prompt}") {
			return fmt.Errorf("agent %q 的 args 必须包含 {prompt}", name)
		}
	}
	for name, project := range c.Projects {
		if project.Path == "" {
			return fmt.Errorf("项目 %q 的 path 不能为空", name)
		}
		if _, ok := c.Agents[strings.ToLower(project.Agent)]; !ok {
			return fmt.Errorf("项目 %q 引用了未知 agent %q", name, project.Agent)
		}
	}
	return nil
}

func (c *Config) PollDuration() time.Duration {
	duration, _ := time.ParseDuration(c.PollInterval)
	return duration
}

func Example() Config {
	return Config{
		VaultPath:              `C:\Users\you\Documents\Obsidian Vault`,
		InboxDir:               "Wechatian",
		OutboxDir:              "Wechatian/outbox",
		StatePath:              "~/.taskian/state.json",
		PollInterval:           "10s",
		SkipExistingOnFirstRun: true,
		MaxReplyChars:          6000,
		Agents: map[string]AgentConfig{
			"codex": {
				Command: "codex",
				Args:    []string{"exec", "--sandbox", "workspace-write", "--output-last-message", "{output}", "{prompt}"},
				Timeout: "45m",
			},
		},
		Projects: map[string]ProjectConfig{
			"my-project": {Path: `C:\work\my-project`, Agent: "codex"},
		},
	}
}

func applyDefaults(c *Config) {
	if c.InboxDir == "" {
		c.InboxDir = "Wechatian"
	}
	if c.OutboxDir == "" {
		c.OutboxDir = "Wechatian/outbox"
	}
	if c.StatePath == "" {
		c.StatePath = "~/.taskian/state.json"
	}
	if c.PollInterval == "" {
		c.PollInterval = "10s"
	}
	if c.MaxReplyChars <= 0 {
		c.MaxReplyChars = 6000
	}
	for name, agent := range c.Agents {
		if agent.Timeout == "" {
			agent.Timeout = "45m"
			c.Agents[name] = agent
		}
	}
	for name, project := range c.Projects {
		if project.Agent == "" {
			project.Agent = "codex"
			c.Projects[name] = project
		}
	}
}

func expandPath(path string) string {
	path = os.ExpandEnv(path)
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimLeft(path[1:], `/\`))
		}
	}
	if absolute, err := filepath.Abs(path); err == nil {
		return filepath.Clean(absolute)
	}
	return filepath.Clean(path)
}

func resolveUnder(root, path string) string {
	path = os.ExpandEnv(path)
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimLeft(path[1:], `/\`))
		}
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(root, path))
}
