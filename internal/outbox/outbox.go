package outbox

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

func Write(dir, label, content string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	label = unsafeName.ReplaceAllString(label, "-")
	label = strings.Trim(label, "-")
	if label == "" {
		label = "message"
	}
	name := fmt.Sprintf("taskian-%s-%s.md", time.Now().Format("20060102-150405.000"), label)
	finalPath := filepath.Join(dir, name)
	temp, err := os.CreateTemp(dir, ".taskian-*.tmp")
	if err != nil {
		return "", err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.WriteString(strings.TrimSpace(content) + "\n"); err != nil {
		_ = temp.Close()
		return "", err
	}
	if err := temp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tempPath, finalPath); err != nil {
		return "", err
	}
	return finalPath, nil
}
