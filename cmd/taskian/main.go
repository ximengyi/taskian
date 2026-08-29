package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/ximengyi/taskian/internal/agent"
	"github.com/ximengyi/taskian/internal/app"
	"github.com/ximengyi/taskian/internal/config"
	"github.com/ximengyi/taskian/internal/feishu"
	"github.com/ximengyi/taskian/internal/ilink"
	"github.com/ximengyi/taskian/internal/service"
	"github.com/ximengyi/taskian/internal/store"
)

var version = "dev"

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	if err := run(os.Args[1:]); err != nil {
		log.Printf("错误: %v", err)
		if len(os.Args) == 1 && isatty.IsTerminal(os.Stdin.Fd()) {
			fmt.Print("\n按回车退出……")
			_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "launch"
	if len(args) > 0 && !startsFlag(args[0]) {
		command, args = args[0], args[1:]
	}
	switch command {
	case "serve", "once", "check", "status":
		fs := flag.NewFlagSet(command, flag.ContinueOnError)
		configPath := fs.String("config", defaultConfigPath(), "配置文件路径")
		logPath := fs.String("log", "", "同时写入日志文件")
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
		closeLog, err := enableFileLog(*logPath)
		if err != nil {
			return err
		}
		defer closeLog()
		dispatcher, err := app.New(cfg, log.Default())
		if err != nil {
			return err
		}
		if command == "check" {
			defer dispatcher.Close()
			if err := dispatcher.Check(); err != nil {
				return err
			}
			fmt.Printf("配置有效：通道=%s，%d 个 Agent，%d 个项目\n", channelNames(cfg), len(cfg.Agents), len(cfg.Projects))
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
	case "feishu":
		return runFeishu(args)
	case "agents":
		return runAgents(args)
	case "launch", "init":
		return runLauncher(args)
	case "service":
		return runService(args)
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

func runLauncher(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := os.Stat(*configPath); os.IsNotExist(err) {
		if err := config.WritePersonal(*configPath); err != nil {
			return err
		}
		fmt.Printf("已创建个人模式配置：%s\n", *configPath)
	}
	fmt.Printf("Taskian %s 首次启动\n\n正在探测本机 Agent：\n", version)
	found := agent.Detect()
	if len(found) == 0 {
		fmt.Println("- 暂未找到 Codex/Cursor；可以稍后安装，Taskian 会继续检查。")
	}
	for _, item := range found {
		fmt.Printf("- %s：%s\n", item.Type, item.Path)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	ilinkChannel, hasIlink := cfg.ChannelOf("ilink")
	credentials := ilink.Credentials{}
	if hasIlink {
		credentials, err = ilink.LoadCredentials(ilinkChannel.StatePath)
	}
	if err != nil {
		return err
	}
	if hasIlink && credentials.Token == "" {
		fmt.Println("\n请使用微信扫描下面的二维码绑定 Taskian：")
		if err := runIlink([]string{"login", "-config", *configPath}); err != nil {
			return err
		}
	}
	reader := bufio.NewReader(os.Stdin)
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		fmt.Println("\n当前系统暂不支持自动后台服务，将在当前窗口运行。")
		return serve(cfg)
	}
	fmt.Print("\n是否让 Taskian 在后台自动启动？[Y/n] ")
	answer, _ := reader.ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer == "" || answer == "y" || answer == "yes" {
		manager := service.Manager{ConfigPath: config.ExpandPath(*configPath), Version: version}
		if err := manager.Run("install"); err != nil {
			return fmt.Errorf("安装后台服务: %w", err)
		}
		if err := manager.Run("start"); err != nil {
			return fmt.Errorf("启动后台服务: %w", err)
		}
		if err := manager.WaitRunning(15 * time.Second); err != nil {
			return err
		}
		fmt.Println("\n✅ Taskian 已在后台运行，现在可以关闭窗口。")
		if enabled, checkErr := service.LingerEnabled(); checkErr == nil && !enabled {
			fmt.Printf("⚠️ 当前用户退出登录后 systemd user service 可能停止。若需持续运行，请执行：\nsudo loginctl enable-linger %s\n", service.CurrentUsername())
		}
		return nil
	}
	fmt.Println("\nTaskian 将在当前窗口运行；关闭窗口会停止服务。")
	return serve(cfg)
}

func runService(args []string) error {
	operation := "status"
	if len(args) > 0 && !startsFlag(args[0]) {
		operation, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("service "+operation, flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manager := service.Manager{ConfigPath: config.ExpandPath(*configPath), Version: version}
	if err := manager.Run(operation); err != nil {
		return err
	}
	if operation == "install" {
		fmt.Println("后台服务已安装。使用 taskian service start 启动。")
	}
	if operation == "start" {
		if err := manager.WaitRunning(15 * time.Second); err != nil {
			return err
		}
		fmt.Println("Taskian 后台服务已启动。")
	}
	if operation == "stop" {
		fmt.Println("Taskian 后台服务已停止。")
	}
	if operation == "uninstall" {
		fmt.Println("后台服务已移除；配置、登录和任务数据均已保留。")
	}
	return nil
}

func serve(cfg *config.Config) error {
	dispatcher, err := app.New(cfg, log.Default())
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return dispatcher.Serve(ctx)
}

func enableFileLog(path string) (func(), error) {
	if path == "" {
		return func() {}, nil
	}
	path = config.ExpandPath(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err == nil && info.Size() > 5<<20 {
		_ = os.Remove(path + ".1")
		_ = os.Rename(path, path+".1")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	log.SetOutput(io.MultiWriter(os.Stderr, file))
	return func() { _ = file.Close() }, nil
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
	channelCfg, ok := cfg.ChannelOf("ilink")
	if !ok {
		return fmt.Errorf("当前配置没有启用 ilink 通道")
	}
	switch operation {
	case "login":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		credentials, err := ilink.Login(ctx, channelCfg)
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
		credentials, err := ilink.LoadCredentials(channelCfg.StatePath)
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
		if err := ilink.Logout(channelCfg.StatePath); err != nil {
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

func runFeishu(args []string) error {
	operation := "status"
	if len(args) > 0 && !startsFlag(args[0]) {
		operation, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("feishu "+operation, flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	appID := fs.String("app-id", "", "飞书应用 App ID（手工配置）")
	appSecret := fs.String("app-secret", "", "飞书应用 App Secret（手工配置）")
	manual := fs.Bool("manual", false, "使用已有飞书企业自建应用")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	channelCfg, exists := cfg.ChannelOf("feishu")
	if !exists {
		channelCfg = config.ChannelConfig{Type: "feishu", StatePath: filepath.Join(cfg.DataDir, "feishu.json")}
	}
	switch operation {
	case "setup":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		var credentials feishu.Credentials
		if *manual || *appID != "" || *appSecret != "" {
			credentials, err = feishu.SetupManual(channelCfg.StatePath, firstNonEmpty(*appID, os.Getenv("FEISHU_APP_ID")), firstNonEmpty(*appSecret, os.Getenv("FEISHU_APP_SECRET")))
		} else {
			credentials, err = feishu.SetupAutomatic(ctx, channelCfg.StatePath, nil)
		}
		if err != nil {
			return fmt.Errorf("配置飞书: %w", err)
		}
		if err := config.EnsureChannel(*configPath, config.ChannelConfig{Type: "feishu", StatePath: channelCfg.StatePath}); err != nil {
			return fmt.Errorf("启用飞书通道: %w", err)
		}
		fmt.Println("\n✅ 飞书通道已写入 Taskian 配置。")
		if credentials.OwnerID != "" {
			fmt.Printf("已绑定飞书用户：%s\n", credentials.OwnerID)
		} else {
			fmt.Printf("启动或重启 Taskian 后，请在飞书中向机器人发送：绑定 %s\n绑定码有效期至：%s\n", credentials.BindCode, credentials.BindExpires.Local().Format("2006-01-02 15:04:05"))
		}
		fmt.Println("请确保应用已发布，并启用了“接收消息”事件和长连接模式。")
		return nil
	case "status":
		credentials, loadErr := feishu.LoadCredentials(channelCfg.StatePath)
		if loadErr != nil {
			return loadErr
		}
		if credentials.AppID == "" {
			credentials.AppID = channelCfg.AppID
		}
		if credentials.OwnerID == "" {
			credentials.OwnerID = channelCfg.OwnerID
		}
		if credentials.AppID == "" {
			fmt.Println("飞书：未配置。运行 taskian feishu setup 开始配置。")
			return nil
		}
		binding := "等待绑定"
		if credentials.OwnerID != "" {
			binding = credentials.OwnerID
		}
		fmt.Printf("飞书：已配置\nApp ID：%s\n绑定用户：%s\n配置文件：%s\n", credentials.AppID, binding, channelCfg.StatePath)
		return nil
	case "logout":
		if err := feishu.Logout(channelCfg.StatePath); err != nil {
			return err
		}
		state, stateErr := store.Open(cfg.DatabasePath)
		if stateErr == nil {
			stateErr = state.DeleteChannelBindings("feishu")
			_ = state.Close()
		}
		if stateErr != nil {
			return stateErr
		}
		fmt.Println("飞书凭据和本机绑定已清除；通道仍保留在配置中。")
		return nil
	default:
		return fmt.Errorf("未知 feishu 命令 %q；可用命令：setup、status、logout", operation)
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
	fmt.Printf("Taskian %s\n通道：%s\n状态库：%s\n", version, channelNames(cfg), cfg.DatabasePath)
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
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
func channelNames(cfg *config.Config) string {
	names := make([]string, 0, len(cfg.Channels))
	for _, channel := range cfg.Channels {
		names = append(names, channel.Type)
	}
	return strings.Join(names, ", ")
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
	fmt.Print(`Taskian - 微信/飞书与本地编程 Agent 的双向任务调度器

用法：
  taskian serve [-config FILE]          持续运行
  taskian init [-config FILE]           初始化、扫码并选择后台运行
  taskian once [-config FILE]           接收一轮消息后退出
  taskian check [-config FILE]          检查配置、Agent 和项目
  taskian status [-config FILE]         查看本地任务状态
  taskian agents detect [-json]         自动探测本机 Agent CLI
  taskian service <操作>                管理后台服务
  taskian ilink login [-config FILE]    在终端扫码登录 iLink
  taskian ilink status [-config FILE]   查看 iLink 绑定状态
  taskian ilink logout [-config FILE]   清除 iLink 登录
  taskian feishu setup [-config FILE]   配置并绑定飞书长连接
  taskian feishu status [-config FILE]  查看飞书配置和绑定状态
  taskian feishu logout [-config FILE]  清除飞书凭据与绑定
  taskian example-config                输出 0.5 示例配置
  taskian version                       显示版本
`)
}
