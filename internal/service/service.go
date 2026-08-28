package service

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/gofrs/flock"
	"github.com/ximengyi/taskian/internal/config"
	"github.com/ximengyi/taskian/internal/store"
)

type Manager struct{ ConfigPath, Version string }

func CurrentUsername() string {
	current, err := user.Current()
	if err == nil && current.Username != "" {
		return current.Username
	}
	return os.Getenv("USER")
}

func LingerEnabled() (bool, error) {
	if runtime.GOOS != "linux" {
		return true, nil
	}
	current, err := user.Current()
	if err != nil {
		return false, err
	}
	output, err := exec.Command("loginctl", "show-user", current.Username, "-p", "Linger", "--value").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("检查 systemd linger: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return strings.EqualFold(strings.TrimSpace(string(output)), "yes"), nil
}

func (m Manager) WaitRunning(timeout time.Duration) error {
	cfg, err := config.Load(m.ConfigPath)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		lock := flock.New(cfg.DatabasePath + ".lock")
		locked, checkErr := lock.TryLock()
		if checkErr == nil && !locked {
			state, openErr := store.Open(cfg.DatabasePath)
			if openErr == nil {
				value, readErr := state.GetChannelState("service.heartbeat")
				_ = state.Close()
				stamp, parseErr := time.Parse(time.RFC3339Nano, value)
				if readErr == nil && parseErr == nil && time.Since(stamp) < 30*time.Second {
					return nil
				}
			}
		}
		if locked {
			_ = lock.Unlock()
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("后台实例未在 %s 内获得单实例锁", timeout)
}

func (m Manager) WaitStopped(timeout time.Duration) error {
	cfg, err := config.Load(m.ConfigPath)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(cfg.DatabasePath); os.IsNotExist(statErr) {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		lock := flock.New(cfg.DatabasePath + ".lock")
		locked, checkErr := lock.TryLock()
		if checkErr == nil && locked {
			_ = lock.Unlock()
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("后台实例未在 %s 内停止", timeout)
}

func (m Manager) Run(operation string) error {
	switch operation {
	case "install":
		return m.install()
	case "start":
		return m.command("start")
	case "stop":
		return m.command("stop")
	case "restart":
		_ = m.command("stop")
		return m.command("start")
	case "status":
		return m.command("status")
	case "logs":
		return m.logs()
	case "uninstall":
		return m.uninstall()
	default:
		return fmt.Errorf("未知 service 命令 %q", operation)
	}
}

func (m Manager) install() error {
	_ = m.command("stop")
	_ = m.WaitStopped(5 * time.Second)
	stable, err := stableExecutable()
	if err != nil {
		return err
	}
	if err := copySelf(stable); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		logPath := filepath.Join(filepath.Dir(m.ConfigPath), "logs", "taskian.log")
		if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
			return err
		}
		arguments := fmt.Sprintf("serve -config \"%s\" -log \"%s\"", m.ConfigPath, logPath)
		script := fmt.Sprintf("$a=New-ScheduledTaskAction -Execute '%s' -Argument '%s'; $t=New-ScheduledTaskTrigger -AtLogOn; $s=New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -MultipleInstances IgnoreNew; Register-ScheduledTask -TaskName 'Taskian' -Action $a -Trigger $t -Settings $s -Force | Out-Null", psQuote(stable), psQuote(arguments))
		return run("powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", encodePowerShell(script))
	}
	if runtime.GOOS == "linux" {
		home, _ := os.UserHomeDir()
		unitDir := filepath.Join(home, ".config", "systemd", "user")
		unit := filepath.Join(unitDir, "taskian.service")
		if err := os.MkdirAll(unitDir, 0o700); err != nil {
			return err
		}
		content := fmt.Sprintf("[Unit]\nDescription=Taskian AI task dispatcher\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nType=simple\nExecStart=\"%s\" serve -config \"%s\"\nRestart=on-failure\nRestartSec=5\n\n[Install]\nWantedBy=default.target\n", stable, m.ConfigPath)
		if err := os.WriteFile(unit, []byte(content), 0o600); err != nil {
			return err
		}
		if err := run("systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		return run("systemctl", "--user", "enable", "taskian.service")
	}
	return fmt.Errorf("当前系统暂不支持后台服务管理")
}

func (m Manager) command(operation string) error {
	if runtime.GOOS == "windows" {
		switch operation {
		case "start":
			return run("schtasks.exe", "/Run", "/TN", "Taskian")
		case "stop":
			return run("schtasks.exe", "/End", "/TN", "Taskian")
		case "status":
			output, err := exec.Command("schtasks.exe", "/Query", "/TN", "Taskian", "/V", "/FO", "LIST").CombinedOutput()
			if err != nil {
				fmt.Println("Taskian 后台服务未安装或当前不可用。")
				return nil
			}
			fmt.Print(string(output))
			return nil
		}
	}
	if runtime.GOOS == "linux" {
		if operation == "status" {
			output, err := exec.Command("systemctl", "--user", "status", "taskian.service", "--no-pager").CombinedOutput()
			if len(output) > 0 {
				fmt.Print(string(output))
			}
			if err != nil && len(output) == 0 {
				return err
			}
			return nil
		}
		return run("systemctl", "--user", operation, "taskian.service")
	}
	return fmt.Errorf("当前系统暂不支持后台服务管理")
}

func (m Manager) uninstall() error {
	_ = m.command("stop")
	_ = m.WaitStopped(5 * time.Second)
	if runtime.GOOS == "windows" {
		return run("schtasks.exe", "/Delete", "/TN", "Taskian", "/F")
	}
	if runtime.GOOS == "linux" {
		home, _ := os.UserHomeDir()
		_ = run("systemctl", "--user", "disable", "taskian.service")
		if err := os.Remove(filepath.Join(home, ".config", "systemd", "user", "taskian.service")); err != nil && !os.IsNotExist(err) {
			return err
		}
		return run("systemctl", "--user", "daemon-reload")
	}
	return fmt.Errorf("当前系统暂不支持后台服务管理")
}

func (m Manager) logs() error {
	if runtime.GOOS == "linux" {
		return runInteractive("journalctl", "--user", "-u", "taskian.service", "-n", "100", "-f")
	}
	if runtime.GOOS == "windows" {
		path := filepath.Join(filepath.Dir(m.ConfigPath), "logs", "taskian.log")
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Print(string(data))
		return nil
	}
	return fmt.Errorf("当前系统暂不支持后台服务管理")
}

func stableExecutable() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			base = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(base, "Taskian", "taskian.exe"), nil
	}
	return filepath.Join(home, ".local", "bin", "taskian"), nil
}

func copySelf(destination string) error {
	source, err := os.Executable()
	if err != nil {
		return err
	}
	source, _ = filepath.Abs(source)
	destination, _ = filepath.Abs(destination)
	if strings.EqualFold(source, destination) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	temporary := destination + ".new"
	out, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(temporary)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(temporary)
		return closeErr
	}
	_ = os.Remove(destination)
	return os.Rename(temporary, destination)
}

func run(name string, args ...string) error {
	output, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(output)))
	}
	return nil
}
func runInteractive(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	return cmd.Run()
}

func psQuote(value string) string { return strings.ReplaceAll(value, "'", "''") }
func encodePowerShell(script string) string {
	units := utf16.Encode([]rune(script))
	data := make([]byte, len(units)*2)
	for i, unit := range units {
		binary.LittleEndian.PutUint16(data[i*2:], unit)
	}
	return base64.StdEncoding.EncodeToString(data)
}
