package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/ximengyi/taskian/internal/agent"
	"github.com/ximengyi/taskian/internal/app"
	"github.com/ximengyi/taskian/internal/config"
	"github.com/ximengyi/taskian/internal/ilink"
	"github.com/ximengyi/taskian/internal/store"
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
	if len(args) > 0 && !startsFlag(args[0]) {
		command, args = args[0], args[1:]
	}
	switch command {
	case "serve", "once", "check", "status":
		fs := flag.NewFlagSet(command, flag.ContinueOnError)
		configPath := fs.String("config", defaultConfigPath(), "配置文件路径")
		if err := fs.Parse(args); err != nil {
			return err
		}
		cfg, err := config.Load(*configPath)
		if err != nil {
			return err
		}
		if command == "status" {
			return showStatus(cfg)
		}
		dispatcher, err := app.New(cfg, log.Default())
		if err != nil {
			return err
		}
		if command == "check" {
			defer dispatcher.Close()
			if err := dispatcher.Check(); err != nil {
				return err
			}
			fmt.Printf("配置有效：通道=%s，%d 个 Agent，%d 个项目\n", cfg.Channel.Type, len(cfg.Agents), len(cfg.Projects))
			return nil
		}
		if command == "once" {
			return dispatcher.RunOnce(context.Background())
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return dispatcher.Serve(ctx)
	case "ilink":
		return runIlink(args)
	case "agents":
		return runAgents(args)
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

func runAgents(args []string) error {
	operation := "detect"
	if len(args) > 0 && !startsFlag(args[0]) {
		operation, args = args[0], args[1:]
	}
	if operation != "detect" {
		return fmt.Errorf("未知 agents 命令 %q；可用命令：detect", operation)
	}
	fs := flag.NewFlagSet("agents detect", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	if err := fs.Parse(args); err != nil {
		return err
	}
	found := agent.Detect()
	if *asJSON {
		data, err := json.MarshalIndent(found, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}
	if len(found) == 0 {
		fmt.Println("没有找到 Codex 或 Cursor Agent CLI。请先安装，并确保 codex/agent 命令可执行。")
		return nil
	}
	for _, item := range found {
		fmt.Printf("%s\t%s\t(%s)\n", item.Type, item.Path, item.Source)
	}
	return nil
}

func runIlink(args []string) error {
	operation := "status"
	if len(args) > 0 && !startsFlag(args[0]) {
		operation, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("ilink "+operation, flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.Channel.Type != "ilink" {
		return fmt.Errorf("当前配置的 channel.type 不是 ilink")
	}
	switch operation {
	case "login":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		credentials, err := ilink.Login(ctx, cfg.Channel)
		if err != nil {
			return err
		}
		state, err := store.Open(cfg.DatabasePath)
		if err == nil {
			err = state.ResetIlinkState()
			_ = state.Close()
		}
		if err != nil {
			return err
		}
		fmt.Printf("iLink 登录成功：bot_id=%s，绑定用户=%s\n", credentials.BotID, credentials.ScannedUser)
		return nil
	case "status":
		credentials, err := ilink.LoadCredentials(cfg.Channel.StatePath)
		if err != nil {
			return err
		}
		if credentials.Token == "" {
			fmt.Println("iLink：未登录")
			return nil
		}
		fmt.Printf("iLink：已登录\nbot_id：%s\n绑定用户：%s\n网关：%s\n", credentials.BotID, credentials.ScannedUser, credentials.BaseURL)
		return nil
	case "logout":
		if err := ilink.Logout(cfg.Channel.StatePath); err != nil {
			return err
		}
		state, e := store.Open(cfg.DatabasePath)
		if e == nil {
			e = state.ResetIlinkState()
			_ = state.Close()
		}
		if e != nil {
			return e
		}
		fmt.Println("iLink 登录已清除。")
		return nil
	default:
		return fmt.Errorf("未知 iLink 命令 %q；可用命令：login、status、logout", operation)
	}
}

func showStatus(cfg *config.Config) error {
	state, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer state.Close()
	counts, err := state.Counts()
	if err != nil {
		return err
	}
	fmt.Printf("Taskian %s\n通道：%s\n状态库：%s\n", version, cfg.Channel.Type, cfg.DatabasePath)
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		fmt.Println("任务：0")
	} else {
		for _, key := range keys {
			fmt.Printf("%s：%d\n", key, counts[key])
		}
	}
	return nil
}

func startsFlag(value string) bool { return len(value) > 0 && value[0] == '-' }
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
	fmt.Print(`Taskian - 微信与本地编程 Agent 的双向任务调度器

用法：
  taskian serve [-config FILE]          持续运行
  taskian once [-config FILE]           接收一轮消息后退出
  taskian check [-config FILE]          检查配置、Agent 和项目
  taskian status [-config FILE]         查看本地任务状态
  taskian agents detect [-json]         自动探测本机 Agent CLI
  taskian ilink login [-config FILE]    在终端扫码登录 iLink
  taskian ilink status [-config FILE]   查看 iLink 绑定状态
  taskian ilink logout [-config FILE]   清除 iLink 登录
  taskian example-config                输出 0.3 示例配置
  taskian version                       显示版本
`)
}
