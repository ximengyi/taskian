package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"taskian.local/taskian/internal/app"
	"taskian.local/taskian/internal/config"
)

var version = "dev"

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	if err := run(os.Args[1:]); err != nil {
		log.Printf("错误: %v", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "serve"
	if len(args) > 0 && args[0][0] != '-' {
		command, args = args[0], args[1:]
	}

	switch command {
	case "serve", "once", "check":
		fs := flag.NewFlagSet(command, flag.ContinueOnError)
		configPath := fs.String("config", defaultConfigPath(), "配置文件路径")
		if err := fs.Parse(args); err != nil {
			return err
		}
		cfg, err := config.Load(*configPath)
		if err != nil {
			return err
		}
		if command == "check" {
			return check(cfg)
		}
		dispatcher, err := app.New(cfg, log.Default())
		if err != nil {
			return err
		}
		if command == "once" {
			return dispatcher.RunOnce(context.Background())
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return dispatcher.Serve(ctx)

	case "example-config":
		data, err := json.MarshalIndent(config.Example(), "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil

	case "version":
		fmt.Printf("Taskian %s\n", version)
		return nil

	case "help", "-h", "--help":
		printHelp()
		return nil
	default:
		printHelp()
		return fmt.Errorf("未知命令 %q", command)
	}
}

func check(cfg *config.Config) error {
	for name, agent := range cfg.Agents {
		if _, err := exec.LookPath(agent.Command); err != nil {
			return fmt.Errorf("找不到 agent %q 的命令 %q: %w", name, agent.Command, err)
		}
	}
	for name, project := range cfg.Projects {
		info, err := os.Stat(project.Path)
		if err != nil {
			return fmt.Errorf("项目 %q 路径不可用: %w", name, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("项目 %q 路径不是目录", name)
		}
	}
	fmt.Printf("配置有效：%d 个 agent，%d 个项目\n", len(cfg.Agents), len(cfg.Projects))
	return nil
}

func defaultConfigPath() string {
	if value := os.Getenv("TASKIAN_CONFIG"); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(home, ".taskian", "config.json")
}

func printHelp() {
	fmt.Print(`Taskian - 通过 Wechatian 调度本地 AI agent

用法：
  taskian serve [-config FILE]         持续监听（默认）
  taskian once [-config FILE]          扫描一次后退出
  taskian check [-config FILE]         检查配置和依赖
  taskian example-config               输出示例配置
  taskian version                      显示版本
`)
}
