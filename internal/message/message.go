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
	ID        string
	Source    string
	Timestamp string
	Body      string
}

type Task struct {
	Agent   string
	Project string
	Prompt  string
	Source  Incoming
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
			ID:        hex.EncodeToString(sum[:]),
			Source:    source,
			Timestamp: match[1],
			Body:      text,
		})
	}
	return result, nil
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
		if len(header) != 2 {
			return Task{}, fmt.Errorf("快捷格式应为：#<agent> <项目>")
		}
		agent, project = strings.TrimPrefix(header[0], "#"), header[1]
	}
	prompt := strings.TrimSpace(strings.Join(lines[1:], "\n"))
	if prompt == "" {
		return Task{}, fmt.Errorf("任务正文不能为空")
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
