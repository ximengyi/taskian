package message

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type Incoming struct {
	ID           string
	Source       string
	Timestamp    string
	Body         string
	Channel      string
	Sender       string
	Conversation string
	ReceivedAt   time.Time
}

type Task struct {
	Agent   string
	Project string
	Prompt  string
	Source  Incoming
}

type CommandKind string

const (
	CommandTask    CommandKind = "task"
	CommandReply   CommandKind = "reply"
	CommandStatus  CommandKind = "status"
	CommandCancel  CommandKind = "cancel"
	CommandHelp    CommandKind = "help"
	CommandProject CommandKind = "project"
	CommandUse     CommandKind = "use"
	CommandConfirm CommandKind = "confirm"
)

type Command struct {
	Kind   CommandKind
	Task   Task
	TaskID string
	Text   string
	Action string
	Args   []string
}

var (
	receivedHeader = regexp.MustCompile(`^\*\*(\d{2}:\d{2})\*\*\s*·\s*(接收|收到|received)\s*$`)
	anyHeader      = regexp.MustCompile(`^\*\*(\d{2}:\d{2})\*\*\s*·\s*.+$`)
	ErrNotCommand  = errors.New("不是 Taskian 命令")
)

func ScanInbox(dir string) ([]Incoming, error) {
	files, err := filepath.Glob(filepath.Join(dir, "????-??-??.md"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var messages []Incoming
	for _, path := range files {
		items, err := ParseFile(path)
		if err != nil {
			return nil, err
		}
		messages = append(messages, items...)
	}
	return messages, nil
}

func ParseFile(path string) ([]Incoming, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取收件箱 %q: %w", path, err)
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	var result []Incoming
	for i := 0; i < len(lines); i++ {
		match := receivedHeader.FindStringSubmatch(strings.TrimSpace(lines[i]))
		if match == nil {
			continue
		}
		var body []string
		for i = i + 1; i < len(lines); i++ {
			line := lines[i]
			if anyHeader.MatchString(strings.TrimSpace(line)) {
				i--
				break
			}
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, ">") {
				body = append(body, strings.TrimSpace(strings.TrimPrefix(trimmed, ">")))
			} else if len(body) > 0 && trimmed != "" {
				break
			}
		}
		text := strings.TrimSpace(strings.Join(body, "\n"))
		if text == "" {
			continue
		}
		source := filepath.Base(path)
		sum := sha256.Sum256([]byte(source + "\x00" + match[1] + "\x00" + text))
		result = append(result, Incoming{
			ID:           hex.EncodeToString(sum[:]),
			Source:       source,
			Timestamp:    match[1],
			Body:         text,
			Channel:      "wechatian-files",
			Sender:       "wechatian-owner",
			Conversation: "wechatian-owner",
			ReceivedAt:   ReceivedAt(Incoming{Source: source, Timestamp: match[1]}),
		})
	}
	return result, nil
}

func ParseCommand(in Incoming) (Command, error) {
	text := strings.TrimSpace(strings.ReplaceAll(in.Body, "\r\n", "\n"))
	if text == "" {
		return Command{}, ErrNotCommand
	}
	lowerText := strings.ToLower(text)
	if lowerText == "help" || strings.HasPrefix(lowerText, "help ") || text == "帮助" {
		topic := ""
		if strings.HasPrefix(lowerText, "help ") {
			topic = strings.TrimSpace(text[len("help "):])
		}
		return Command{Kind: CommandHelp, Text: topic}, nil
	}
	switch lowerText {
	case "当前项目", "查询当前项目", "项目状态", "project current":
		return Command{Kind: CommandProject, Action: "current"}, nil
	case "项目列表", "所有项目", "列出项目", "projects", "project list":
		return Command{Kind: CommandProject, Action: "list"}, nil
	}
	for _, prefix := range []string{"切换项目 ", "使用项目 ", "项目 "} {
		if strings.HasPrefix(text, prefix) && strings.TrimSpace(strings.TrimPrefix(text, prefix)) != "" {
			return Command{Kind: CommandUse, Text: strings.TrimSpace(strings.TrimPrefix(text, prefix))}, nil
		}
	}
	if strings.HasPrefix(text, "修改项目路径 ") {
		rest := strings.TrimSpace(strings.TrimPrefix(text, "修改项目路径 "))
		parts := strings.Fields(rest)
		if len(parts) < 2 {
			return Command{}, fmt.Errorf("修改项目路径格式应为：修改项目路径 <ID或名称> <绝对路径>")
		}
		return Command{Kind: CommandProject, Action: "path", Args: []string{parts[0], strings.TrimSpace(strings.TrimPrefix(rest, parts[0]))}}, nil
	}
	lines := strings.Split(text, "\n")
	header := strings.Fields(strings.TrimSpace(lines[0]))
	if len(header) == 0 || !strings.HasPrefix(header[0], "#") {
		return Command{}, ErrNotCommand
	}
	switch strings.ToLower(header[0]) {
	case "#reply":
		rest := strings.TrimSpace(strings.TrimPrefix(text, header[0]))
		if rest == "" {
			return Command{}, fmt.Errorf("#reply 格式应为：#reply <任务号> <回答>")
		}
		var taskID, answer string
		parts := strings.Fields(rest)
		first := parts[0]
		if strings.HasPrefix(strings.ToUpper(first), "T-") {
			taskID = strings.ToUpper(first)
			answer = strings.TrimSpace(strings.TrimPrefix(rest, first))
		} else {
			answer = rest
		}
		if answer == "" {
			return Command{}, fmt.Errorf("回答内容不能为空")
		}
		return Command{Kind: CommandReply, TaskID: taskID, Text: answer}, nil
	case "#status":
		if len(header) > 2 {
			return Command{}, fmt.Errorf("#status 格式应为：#status [任务号]")
		}
		id := ""
		if len(header) == 2 {
			id = strings.ToUpper(header[1])
		}
		return Command{Kind: CommandStatus, TaskID: id}, nil
	case "#cancel":
		if len(header) != 2 {
			return Command{}, fmt.Errorf("#cancel 格式应为：#cancel <任务号>")
		}
		return Command{Kind: CommandCancel, TaskID: strings.ToUpper(header[1])}, nil
	case "#help":
		return Command{Kind: CommandHelp, Text: strings.TrimSpace(strings.TrimPrefix(text, header[0]))}, nil
	case "#project":
		if len(header) < 2 {
			return Command{}, fmt.Errorf("#project 需要子命令：add/list/current/show/rename/path/remove/find")
		}
		return Command{Kind: CommandProject, Action: strings.ToLower(header[1]), Args: header[2:]}, nil
	case "#use":
		if len(header) != 2 {
			return Command{}, fmt.Errorf("#use 格式应为：#use <项目ID或名称>")
		}
		return Command{Kind: CommandUse, Text: strings.ToLower(header[1])}, nil
	case "#confirm":
		if len(header) != 2 {
			return Command{}, fmt.Errorf("#confirm 格式应为：#confirm <确认码|cancel>")
		}
		return Command{Kind: CommandConfirm, Text: header[1]}, nil
	}
	task, err := ParseTask(in)
	if err != nil {
		return Command{}, err
	}
	return Command{Kind: CommandTask, Task: task}, nil
}

func ParseTask(in Incoming) (Task, error) {
	lines := strings.Split(strings.ReplaceAll(in.Body, "\r\n", "\n"), "\n")
	if len(lines) == 0 {
		return Task{}, ErrNotCommand
	}
	header := strings.Fields(strings.TrimSpace(lines[0]))
	if len(header) == 0 || !strings.HasPrefix(header[0], "#") {
		return Task{}, ErrNotCommand
	}
	var agent, project string
	if strings.EqualFold(header[0], "#taskian") {
		if len(header) != 3 {
			return Task{}, fmt.Errorf("#taskian 格式应为：#taskian <agent> <项目>")
		}
		agent, project = header[1], header[2]
	} else {
		agent = strings.TrimPrefix(header[0], "#")
		if len(header) >= 2 {
			project = header[1]
		}
	}
	prompt := strings.TrimSpace(strings.Join(lines[1:], "\n"))
	if len(header) > 2 && !strings.EqualFold(header[0], "#taskian") {
		prompt = strings.TrimSpace(strings.Join(header[2:], " ") + "\n" + prompt)
	}
	if prompt == "" {
		// The dispatcher may reinterpret the second token as prompt when a
		// conversation already has a current project.
		if project == "" {
			return Task{}, fmt.Errorf("任务正文不能为空")
		}
	}
	return Task{
		Agent:   strings.ToLower(agent),
		Project: strings.ToLower(project),
		Prompt:  prompt,
		Source:  in,
	}, nil
}

func ReceivedAt(in Incoming) time.Time {
	value := strings.TrimSuffix(in.Source, filepath.Ext(in.Source)) + " " + in.Timestamp
	parsed, _ := time.ParseInLocation("2006-01-02 15:04", value, time.Local)
	return parsed
}
