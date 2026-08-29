package transport

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ximengyi/taskian/internal/message"
)

type multiFake struct {
	name string
	in   chan message.Incoming
	mu   sync.Mutex
	to   string
	text string
}

func (f *multiFake) Name() string { return f.name }
func (f *multiFake) Poll(ctx context.Context) ([]message.Incoming, error) {
	select {
	case item := <-f.in:
		return []message.Incoming{item}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (f *multiFake) Send(_ context.Context, to, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.to, f.text = to, text
	return nil
}
func (f *multiFake) Close() error { return nil }

func TestMultiRoutesRepliesToOriginChannel(t *testing.T) {
	wechat := &multiFake{name: "ilink", in: make(chan message.Incoming, 1)}
	feishu := &multiFake{name: "feishu", in: make(chan message.Incoming, 1)}
	multi, err := NewMulti(wechat, feishu)
	if err != nil {
		t.Fatal(err)
	}
	defer multi.Close()
	feishu.in <- message.Incoming{ID: "m1", Sender: "chat-1", Conversation: "chat-1", Body: "项目列表"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	items, err := multi.Poll(ctx)
	if err != nil || len(items) != 1 || items[0].Channel != "feishu" {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	if err := multi.Send(ctx, items[0].Sender, "reply"); err != nil {
		t.Fatal(err)
	}
	feishu.mu.Lock()
	defer feishu.mu.Unlock()
	if feishu.to != "chat-1" || feishu.text != "reply" {
		t.Fatalf("to=%q text=%q", feishu.to, feishu.text)
	}
	if wechat.to != "" {
		t.Fatalf("reply leaked to ilink: %q", wechat.to)
	}
}
