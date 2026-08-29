package transport

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ximengyi/taskian/internal/message"
)

const routeSeparator = "\x00"

type multiResult struct {
	messages []message.Incoming
	err      error
}

// Multi combines independent transports while preserving the origin channel
// inside the sender routing key used by the existing dispatcher.
type Multi struct {
	children map[string]Transport
	results  chan multiResult
	ctx      context.Context
	cancel   context.CancelFunc
	once     sync.Once
	close    sync.Once
}

func NewMulti(children ...Transport) (*Multi, error) {
	if len(children) == 0 {
		return nil, fmt.Errorf("至少需要一个消息通道")
	}
	items := make(map[string]Transport, len(children))
	for _, child := range children {
		if child == nil || child.Name() == "" {
			return nil, fmt.Errorf("消息通道无效")
		}
		if _, exists := items[child.Name()]; exists {
			return nil, fmt.Errorf("重复消息通道 %s", child.Name())
		}
		items[child.Name()] = child
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Multi{children: items, results: make(chan multiResult, 128), ctx: ctx, cancel: cancel}, nil
}

func (m *Multi) Name() string {
	if len(m.children) == 1 {
		for name := range m.children {
			return name
		}
	}
	return "multi"
}

func (m *Multi) start() {
	m.once.Do(func() {
		for name, child := range m.children {
			go m.pollChild(name, child)
		}
	})
}

func (m *Multi) pollChild(name string, child Transport) {
	backoff := time.Second
	for m.ctx.Err() == nil {
		messages, err := child.Poll(m.ctx)
		if err != nil {
			select {
			case m.results <- multiResult{err: fmt.Errorf("通道 %s: %w", name, err)}:
			case <-m.ctx.Done():
				return
			}
			select {
			case <-time.After(backoff):
			case <-m.ctx.Done():
				return
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		for index := range messages {
			messages[index].Channel = name
			messages[index].Sender = route(name, messages[index].Sender)
			messages[index].Conversation = route(name, messages[index].Conversation)
		}
		if len(messages) > 0 {
			select {
			case m.results <- multiResult{messages: messages}:
			case <-m.ctx.Done():
				return
			}
		} else if name == "wechatian-files" {
			select {
			case <-time.After(time.Second):
			case <-m.ctx.Done():
				return
			}
		}
	}
}

func (m *Multi) Poll(ctx context.Context) ([]message.Incoming, error) {
	m.start()
	select {
	case result := <-m.results:
		return result.messages, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.ctx.Done():
		return nil, context.Canceled
	}
}

func (m *Multi) Send(ctx context.Context, to, text string) error {
	name, target, ok := unroute(to)
	if !ok {
		if len(m.children) == 1 {
			for _, child := range m.children {
				return child.Send(ctx, to, text)
			}
		}
		return fmt.Errorf("消息目标缺少通道路由")
	}
	child, ok := m.children[name]
	if !ok {
		return fmt.Errorf("找不到消息通道 %s", name)
	}
	return child.Send(ctx, target, text)
}

func (m *Multi) NotificationRecipients() []string {
	var result []string
	for name, child := range m.children {
		if source, ok := child.(interface{ NotificationRecipients() []string }); ok {
			for _, recipient := range source.NotificationRecipients() {
				result = append(result, route(name, recipient))
			}
		}
	}
	return result
}

func (m *Multi) Close() error {
	var first error
	m.close.Do(func() {
		m.cancel()
		for _, child := range m.children {
			if err := child.Close(); err != nil && first == nil {
				first = err
			}
		}
	})
	return first
}

func route(channel, target string) string { return channel + routeSeparator + target }
func unroute(value string) (string, string, bool) {
	parts := strings.SplitN(value, routeSeparator, 2)
	return parts[0], func() string {
		if len(parts) == 2 {
			return parts[1]
		}
		return ""
	}(), len(parts) == 2
}
