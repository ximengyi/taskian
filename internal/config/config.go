package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const DefaultIlinkBaseURL = "https://ilinkai.weixin.qq.com"

type Config struct {
	DataDir                string                   `json:"data_dir,omitempty"`
	DatabasePath           string                   `json:"database_path,omitempty"`
	Channel                ChannelConfig            `json:"channel,omitempty"`
	PollInterval           string                   `json:"poll_interval,omitempty"`
	MaxReplyChars          int                      `json:"max_reply_chars,omitempty"`
	MaxConcurrentTasks     int                      `json:"max_concurrent_tasks,omitempty"`
	WaitingUserTimeout     string                   `json:"waiting_user_timeout,omitempty"`
	Agents                 map[string]AgentConfig   `json:"agents"`
	Projects               map[string]ProjectConfig `json:"projects"`
	VaultPath              string                   `json:"vault_path,omitempty"`
	InboxDir               string                   `json:"inbox_dir,omitempty"`
	OutboxDir              string                   `json:"outbox_dir,omitempty"`
	StatePath              string                   `json:"state_path,omitempty"`
	SkipExistingOnFirstRun bool                     `json:"skip_existing_on_first_run,omitempty"`
}

type ChannelConfig struct {
	Type            string   `json:"type,omitempty"`
	BaseURL         string   `json:"base_url,omitempty"`
	StatePath       string   `json:"state_path,omitempty"`
	AllowedSenders  []string `json:"allowed_senders,omitempty"`
	ChannelVersion  string   `json:"channel_version,omitempty"`
	LongPollTimeout string   `json:"long_poll_timeout,omitempty"`
}

type AgentConfig struct {
	Type       string            `json:"type,omitempty"`
	Command    string            `json:"command"`
	Args       []string          `json:"args,omitempty"`
	ResumeArgs []string          `json:"resume_args,omitempty"`
	Timeout    string            `json:"timeout,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Sandbox    string            `json:"sandbox,omitempty"`
	Model      string            `json:"model,omitempty"`
	Force      bool              `json:"force,omitempty"`
}

type ProjectConfig struct {
	Path          string   `json:"path"`
	Agent         string   `json:"agent,omitempty"`
	AllowedAgents []string `json:"allowed_agents,omitempty"`
}

func Load(path string) (*Config, error) {
	path = ExpandPath(path)
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
	resolvePaths(&cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) Validate() error {
	if c.Channel.Type != "ilink" && c.Channel.Type != "wechatian-files" {
		return fmt.Errorf("channel.type 必须是 ilink 或 wechatian-files")
	}
	if c.Channel.Type == "wechatian-files" && (c.VaultPath == "" || c.InboxDir == "" || c.OutboxDir == "") {
		return fmt.Errorf("wechatian-files 通道需要 vault_path、inbox_dir 和 outbox_dir")
	}
	if c.Channel.Type == "ilink" && c.Channel.StatePath == "" {
		return fmt.Errorf("ilink 通道需要 state_path")
	}
	if _, err := time.ParseDuration(c.PollInterval); err != nil {
		return fmt.Errorf("poll_interval 无效: %w", err)
	}
	if _, err := time.ParseDuration(c.WaitingUserTimeout); err != nil {
		return fmt.Errorf("waiting_user_timeout 无效: %w", err)
	}
	if _, err := time.ParseDuration(c.Channel.LongPollTimeout); err != nil {
		return fmt.Errorf("channel.long_poll_timeout 无效: %w", err)
	}
	if len(c.Agents) == 0 || len(c.Projects) == 0 {
		return fmt.Errorf("至少配置一个 agent 和一个项目")
	}
	for name, agent := range c.Agents {
		if name == "" || agent.Command == "" {
			return fmt.Errorf("agent 名称和 command 不能为空")
		}
		switch agent.Type {
		case "codex", "cursor", "generic":
		default:
			return fmt.Errorf("agent %q 的 type %q 不受支持", name, agent.Type)
		}
		if _, err := time.ParseDuration(agent.Timeout); err != nil {
			return fmt.Errorf("agent %q timeout 无效: %w", name, err)
		}
		if agent.Type == "generic" && !containsPlaceholder(agent.Args) {
			return fmt.Errorf("generic agent %q 的 args 必须包含 {prompt}", name)
		}
	}
	for name, project := range c.Projects {
		if project.Path == "" {
			return fmt.Errorf("项目 %q 的 path 不能为空", name)
		}
		if len(project.AllowedAgents) == 0 {
			return fmt.Errorf("项目 %q 至少允许一个 agent", name)
		}
		for _, agentName := range project.AllowedAgents {
			if _, ok := c.Agents[agentName]; !ok {
				return fmt.Errorf("项目 %q 引用了未知 agent %q", name, agentName)
			}
		}
	}
	return nil
}

func (c *Config) PollDuration() time.Duration {
	duration, _ := time.ParseDuration(c.PollInterval)
	return duration
}
func (c *Config) WaitingDuration() time.Duration {
	duration, _ := time.ParseDuration(c.WaitingUserTimeout)
	return duration
}
func (c *Config) LongPollDuration() time.Duration {
	duration, _ := time.ParseDuration(c.Channel.LongPollTimeout)
	return duration
}

func (p ProjectConfig) Allows(agent string) bool {
	for _, allowed := range p.AllowedAgents {
		if strings.EqualFold(allowed, agent) {
			return true
		}
	}
	return false
}

func Example() Config {
	return Config{
		DataDir: "~/.taskian", DatabasePath: "~/.taskian/taskian.db",
		Channel:      ChannelConfig{Type: "ilink", BaseURL: DefaultIlinkBaseURL, StatePath: "~/.taskian/ilink.json", ChannelVersion: "taskian/0.2", LongPollTimeout: "35s"},
		PollInterval: "10s", MaxReplyChars: 6000, MaxConcurrentTasks: 2, WaitingUserTimeout: "72h",
		Agents: map[string]AgentConfig{
			"codex":  {Type: "codex", Command: "codex", Timeout: "45m", Sandbox: "workspace-write"},
			"cursor": {Type: "cursor", Command: "agent", Timeout: "45m"},
		},
		Projects: map[string]ProjectConfig{"my-project": {Path: "/srv/code/my-project", AllowedAgents: []string{"codex", "cursor"}}},
	}
}

func applyDefaults(c *Config) {
	if c.DataDir == "" {
		c.DataDir = "~/.taskian"
	}
	if c.DatabasePath == "" {
		c.DatabasePath = filepath.Join(c.DataDir, "taskian.db")
	}
	if c.Channel.Type == "" {
		if c.VaultPath != "" {
			c.Channel.Type = "wechatian-files"
		} else {
			c.Channel.Type = "ilink"
		}
	}
	if c.Channel.BaseURL == "" {
		c.Channel.BaseURL = DefaultIlinkBaseURL
	}
	if c.Channel.StatePath == "" {
		c.Channel.StatePath = filepath.Join(c.DataDir, "ilink.json")
	}
	if c.Channel.ChannelVersion == "" {
		c.Channel.ChannelVersion = "taskian/0.2"
	}
	if c.Channel.LongPollTimeout == "" {
		c.Channel.LongPollTimeout = "35s"
	}
	if c.InboxDir == "" {
		c.InboxDir = "Wechatian"
	}
	if c.OutboxDir == "" {
		c.OutboxDir = "Wechatian/outbox"
	}
	if c.StatePath == "" {
		c.StatePath = filepath.Join(c.DataDir, "state.json")
	}
	if c.PollInterval == "" {
		c.PollInterval = "10s"
	}
	if c.WaitingUserTimeout == "" {
		c.WaitingUserTimeout = "72h"
	}
	if c.MaxReplyChars <= 0 {
		c.MaxReplyChars = 6000
	}
	if c.MaxConcurrentTasks <= 0 {
		c.MaxConcurrentTasks = 2
	}
	for name, agent := range c.Agents {
		if agent.Type == "" {
			if name == "codex" && !containsPlaceholder(agent.Args) {
				agent.Type = "codex"
			} else if name == "cursor" && !containsPlaceholder(agent.Args) {
				agent.Type = "cursor"
			} else {
				agent.Type = "generic"
			}
		}
		if agent.Timeout == "" {
			agent.Timeout = "45m"
		}
		if agent.Type == "codex" && agent.Sandbox == "" {
			agent.Sandbox = "workspace-write"
		}
		c.Agents[name] = agent
	}
	for name, project := range c.Projects {
		if len(project.AllowedAgents) == 0 {
			if project.Agent == "" {
				project.Agent = "codex"
			}
			project.AllowedAgents = []string{project.Agent}
		}
		c.Projects[name] = project
	}
}

func resolvePaths(c *Config) {
	c.DataDir = ExpandPath(c.DataDir)
	c.DatabasePath = ExpandPath(c.DatabasePath)
	c.Channel.StatePath = ExpandPath(c.Channel.StatePath)
	c.VaultPath = ExpandPath(c.VaultPath)
	if c.Channel.Type == "wechatian-files" {
		c.InboxDir = resolveUnder(c.VaultPath, c.InboxDir)
		c.OutboxDir = resolveUnder(c.VaultPath, c.OutboxDir)
	}
	c.StatePath = ExpandPath(c.StatePath)
	projects := make(map[string]ProjectConfig, len(c.Projects))
	for name, project := range c.Projects {
		project.Path = ExpandPath(project.Path)
		project.Agent = strings.ToLower(project.Agent)
		for i := range project.AllowedAgents {
			project.AllowedAgents[i] = strings.ToLower(project.AllowedAgents[i])
		}
		sort.Strings(project.AllowedAgents)
		projects[strings.ToLower(name)] = project
	}
	c.Projects = projects
	agents := make(map[string]AgentConfig, len(c.Agents))
	for name, agent := range c.Agents {
		agents[strings.ToLower(name)] = agent
	}
	c.Agents = agents
}

func containsPlaceholder(args []string) bool {
	joined := strings.Join(args, "\x00")
	return strings.Contains(joined, "{prompt}") || strings.Contains(joined, "{output}")
}

func ExpandPath(path string) string {
	if path == "" {
		return ""
	}
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
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(root, path))
}
