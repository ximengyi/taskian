// Package feishu implements Taskian's headless Feishu/Lark WebSocket transport.
package feishu

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	channel "github.com/larksuite/channel-sdk-go"
	channeltypes "github.com/larksuite/channel-sdk-go/types"
	"github.com/larksuite/oapi-sdk-go/v3/scene/registration"
	"github.com/mdp/qrterminal/v3"
	"github.com/ximengyi/taskian/internal/config"
	"github.com/ximengyi/taskian/internal/message"
	"github.com/ximengyi/taskian/internal/store"
)

type Credentials struct {
	AppID       string    `json:"app_id"`
	AppSecret   string    `json:"app_secret"`
	OwnerID     string    `json:"owner_id,omitempty"`
	BindCode    string    `json:"bind_code,omitempty"`
	BindExpires time.Time `json:"bind_expires,omitempty"`
}

type Client struct {
	cfg       config.ChannelConfig
	state     *store.Store
	logger    *log.Logger
	channel   channel.Channel
	messages  chan message.Incoming
	errors    chan error
	ctx       context.Context
	cancel    context.CancelFunc
	startOnce sync.Once
	closeOnce sync.Once
	mu        sync.RWMutex
	creds     Credentials
}

func New(cfg config.ChannelConfig, state *store.Store, logger *log.Logger) (*Client, error) {
	creds, err := LoadCredentials(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	if cfg.AppID != "" {
		creds.AppID = cfg.AppID
	}
	if cfg.AppSecret != "" {
		creds.AppSecret = cfg.AppSecret
	}
	if cfg.OwnerID != "" {
		creds.OwnerID = cfg.OwnerID
	}
	if creds.AppID == "" || creds.AppSecret == "" {
		return nil, fmt.Errorf("飞书尚未配置，请运行 taskian feishu setup")
	}
	requireMention := true
	if cfg.RequireMention != nil {
		requireMention = *cfg.RequireMention
	}
	ch, err := channel.New(creds.AppID, creds.AppSecret, channel.WithPolicyConfig(channeltypes.PolicyConfig{
		RequireMention: &requireMention,
		DMMode:         "open",
	}))
	if err != nil {
		return nil, fmt.Errorf("创建飞书通道: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{cfg: cfg, state: state, logger: logger, channel: ch, messages: make(chan message.Incoming, 128), errors: make(chan error, 16), ctx: ctx, cancel: cancel, creds: creds}
	c.registerHandlers()
	return c, nil
}

func (c *Client) Name() string { return "feishu" }

func (c *Client) registerHandlers() {
	c.channel.OnReady(func() { c.logger.Printf("飞书长连接已就绪") })
	c.channel.OnReconnecting(func() { c.logger.Printf("飞书长连接正在重连") })
	c.channel.OnReconnected(func() { c.logger.Printf("飞书长连接已恢复") })
	c.channel.OnDisconnected(func() { c.logger.Printf("飞书长连接已断开") })
	c.channel.OnError(func(err error) {
		select {
		case c.errors <- err:
		default:
			c.logger.Printf("飞书通道错误: %v", err)
		}
	})
	c.channel.OnMessage(c.onMessage)
}

func (c *Client) onMessage(ctx context.Context, incoming *channel.NormalizedMessage) error {
	if incoming == nil || incoming.ChatID == "" || incoming.UserID == "" {
		return nil
	}
	c.mu.RLock()
	creds := c.creds
	c.mu.RUnlock()
	text := strings.TrimSpace(incoming.Content)
	if creds.OwnerID == "" {
		if !validBinding(text, creds, time.Now()) {
			_, _ = c.channel.Send(ctx, &channel.SendInput{ReceiveID: incoming.ChatID, MsgType: "text", Text: "⛔ Taskian 尚未绑定。请在运行 Taskian 的电脑上执行 taskian feishu setup，并发送终端显示的绑定命令。"})
			return nil
		}
		creds.OwnerID, creds.BindCode = incoming.UserID, ""
		creds.BindExpires = time.Time{}
		if err := SaveCredentials(c.cfg.StatePath, creds); err != nil {
			return err
		}
		if err := c.state.PutChannelBinding("feishu", incoming.UserID, "owner"); err != nil {
			return err
		}
		c.mu.Lock()
		c.creds = creds
		c.mu.Unlock()
		_, err := c.channel.Send(ctx, &channel.SendInput{ReceiveID: incoming.ChatID, MsgType: "text", Text: "✅ 飞书已绑定 Taskian。发送 help、当前项目 或 项目列表 即可开始。"})
		return err
	}
	if incoming.UserID != creds.OwnerID {
		_, _ = c.channel.Send(ctx, &channel.SendInput{ReceiveID: incoming.ChatID, MsgType: "text", Text: "⛔ 当前账号没有权限调度这台电脑上的 Taskian。"})
		return nil
	}
	if incoming.ChatType == "group" && !incoming.MentionedBot {
		return nil
	}
	if len(incoming.Resources) > 0 && text == "" {
		_, _ = c.channel.Send(ctx, &channel.SendInput{ReceiveID: incoming.ChatID, MsgType: "text", Text: "⚠️ Taskian 0.5 暂只支持飞书文本消息，图片和附件尚不能作为任务输入。"})
		return nil
	}
	if text == "" {
		return nil
	}
	id := incoming.MessageID
	if id == "" {
		id = incoming.EventID
	}
	if id == "" {
		id = fmt.Sprintf("%s-%d", incoming.ChatID, incoming.CreateTimeMs)
	}
	at := time.UnixMilli(incoming.CreateTimeMs)
	if incoming.CreateTimeMs == 0 {
		at = time.Now()
	}
	msg := message.Incoming{ID: id, Source: "feishu", Body: text, Channel: "feishu", Sender: incoming.ChatID, Conversation: incoming.ChatID, ReceivedAt: at}
	select {
	case c.messages <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return context.Canceled
	}
}

func validBinding(text string, creds Credentials, now time.Time) bool {
	if creds.BindCode == "" || !creds.BindExpires.After(now) {
		return false
	}
	text = strings.TrimSpace(text)
	return strings.EqualFold(text, "绑定 "+creds.BindCode) || strings.EqualFold(text, "bind "+creds.BindCode)
}

func (c *Client) start() {
	c.startOnce.Do(func() {
		go func() {
			if err := c.channel.Start(c.ctx); err != nil && !errors.Is(err, context.Canceled) {
				select {
				case c.errors <- err:
				default:
				}
			}
		}()
	})
}

func (c *Client) Poll(ctx context.Context) ([]message.Incoming, error) {
	c.start()
	select {
	case item := <-c.messages:
		return []message.Incoming{item}, nil
	case err := <-c.errors:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, context.Canceled
	}
}

func (c *Client) Send(ctx context.Context, to, text string) error {
	_, err := c.channel.Send(ctx, &channel.SendInput{ReceiveID: to, MsgType: "text", Text: strings.TrimSpace(text)})
	return err
}

func (c *Client) NotificationRecipients() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.creds.OwnerID == "" {
		return nil
	}
	return []string{c.creds.OwnerID}
}

func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.cancel()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = c.channel.Stop(ctx)
	})
	return err
}

func SetupAutomatic(ctx context.Context, path string, showQR func(string)) (Credentials, error) {
	result, err := registration.RegisterApp(ctx, &registration.Options{
		Source:     "taskian",
		CreateOnly: true,
		AppPreset:  &registration.AppPreset{Name: "Taskian", Desc: "通过飞书调度本机 Codex、Cursor 等编程 Agent"},
		Addons: &registration.AppAddons{
			Scopes: registration.AppAddonsScopes{Tenant: []string{"im:message:send_as_bot"}},
			Events: registration.AppAddonsEvents{Items: registration.AppAddonsEventItems{Tenant: []string{"im.message.receive_v1"}}},
		},
		OnQRCode: func(info *registration.QRCodeInfo) {
			fmt.Printf("\n请在飞书中打开或扫描下面的授权地址（%d 秒内有效）：\n%s\n\n", info.ExpireIn, info.URL)
			qrterminal.GenerateHalfBlock(info.URL, qrterminal.M, os.Stdout)
			if showQR != nil {
				showQR(info.URL)
			}
		},
	})
	if err != nil {
		return Credentials{}, err
	}
	creds := Credentials{AppID: result.ClientID, AppSecret: result.ClientSecret}
	if result.UserInfo != nil {
		creds.OwnerID = result.UserInfo.OpenID
	}
	if creds.OwnerID == "" {
		creds.BindCode = randomCode()
		creds.BindExpires = time.Now().Add(10 * time.Minute)
	}
	return creds, SaveCredentials(path, creds)
}

func SetupManual(path, appID, appSecret string) (Credentials, error) {
	if strings.TrimSpace(appID) == "" || strings.TrimSpace(appSecret) == "" {
		return Credentials{}, fmt.Errorf("App ID 和 App Secret 不能为空")
	}
	creds := Credentials{AppID: strings.TrimSpace(appID), AppSecret: strings.TrimSpace(appSecret), BindCode: randomCode(), BindExpires: time.Now().Add(10 * time.Minute)}
	return creds, SaveCredentials(path, creds)
}

func LoadCredentials(path string) (Credentials, error) {
	var creds Credentials
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return creds, nil
	}
	if err != nil {
		return creds, err
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return creds, fmt.Errorf("解析飞书凭据: %w", err)
	}
	return creds, nil
}

func SaveCredentials(path string, creds Credentials) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".feishu-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if runtimeErr := os.Rename(name, path); runtimeErr != nil {
		_ = os.Remove(path)
		if err := os.Rename(name, path); err != nil {
			return err
		}
	}
	return os.Chmod(path, 0o600)
}

func Logout(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func randomCode() string {
	value := make([]byte, 3)
	_, _ = rand.Read(value)
	return strings.ToUpper(hex.EncodeToString(value))
}
