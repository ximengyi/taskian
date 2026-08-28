package transport

import (
	"context"
	"fmt"
	"os"

	"github.com/ximengyi/taskian/internal/message"
	"github.com/ximengyi/taskian/internal/outbox"
)

type Files struct{ inbox, outbox string }

func NewFiles(inbox, outboxDir string) (*Files, error) {
	if _, err := os.Stat(inbox); err != nil {
		return nil, fmt.Errorf("Wechatian 收件箱不可用: %w", err)
	}
	if err := os.MkdirAll(outboxDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建 Wechatian 发件箱: %w", err)
	}
	return &Files{inbox: inbox, outbox: outboxDir}, nil
}
func (f *Files) Name() string { return "wechatian-files" }
func (f *Files) Poll(_ context.Context) ([]message.Incoming, error) {
	return message.ScanInbox(f.inbox)
}
func (f *Files) Send(_ context.Context, _ string, text string) error {
	_, err := outbox.Write(f.outbox, "message", text)
	return err
}
func (f *Files) Close() error { return nil }
