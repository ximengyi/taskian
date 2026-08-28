// Package ilink implements Taskian's headless Tencent iLink text transport.
// The protocol flow is derived from Wechatian (MIT, Copyright 2026 Laruence);
// see THIRD_PARTY_NOTICES.md.
package ilink

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	"github.com/ximengyi/taskian/internal/config"
	"github.com/ximengyi/taskian/internal/message"
	"github.com/ximengyi/taskian/internal/store"
)

const sessionExpiredCode = -14

var ErrSessionExpired = errors.New("iLink 登录已失效，请运行 taskian ilink login")

type Credentials struct {
	Token       string `json:"token"`
	BotID       string `json:"bot_id"`
	BaseURL     string `json:"base_url"`
	ScannedUser string `json:"scanned_user"`
}

type Client struct {
	cfg         config.ChannelConfig
	credentials Credentials
	store       *store.Store
	http        *http.Client
	allowed     map[string]bool
}

type qrResponse struct {
	QRCode  string `json:"qrcode"`
	Content string `json:"qrcode_img_content"`
}
type qrStatus struct {
	Status  string `json:"status"`
	Token   string `json:"bot_token"`
	BotID   string `json:"ilink_bot_id"`
	BaseURL string `json:"baseurl"`
	User    string `json:"ilink_user_id"`
}
type baseInfo struct {
	ChannelVersion string `json:"channel_version"`
}
type item struct {
	Type int `json:"type"`
	Text *struct {
		Text string `json:"text"`
	} `json:"text_item,omitempty"`
	Voice *struct {
		Text string `json:"text"`
	} `json:"voice_item,omitempty"`
}
type rawMessage struct {
	MessageID    json.Number `json:"message_id"`
	From         string      `json:"from_user_id"`
	CreateTime   int64       `json:"create_time_ms"`
	MessageType  int         `json:"message_type"`
	Items        []item      `json:"item_list"`
	ContextToken string      `json:"context_token"`
}
type updatesResponse struct {
	Ret          int          `json:"ret"`
	ErrorCode    int          `json:"errcode"`
	ErrorMessage string       `json:"errmsg"`
	Messages     []rawMessage `json:"msgs"`
	Cursor       string       `json:"get_updates_buf"`
}

func New(cfg config.ChannelConfig, state *store.Store) (*Client, error) {
	credentials, err := LoadCredentials(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	if credentials.Token == "" {
		return nil, fmt.Errorf("iLink 尚未登录，请先运行 taskian ilink login")
	}
	if credentials.BaseURL != "" {
		cfg.BaseURL = credentials.BaseURL
	}
	allowed := map[string]bool{}
	for _, id := range cfg.AllowedSenders {
		allowed[id] = true
	}
	if len(allowed) == 0 && credentials.ScannedUser != "" {
		allowed[credentials.ScannedUser] = true
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("iLink 登录状态没有绑定用户，请重新登录或配置 allowed_senders")
	}
	return &Client{cfg: cfg, credentials: credentials, store: state, http: &http.Client{}, allowed: allowed}, nil
}

func (c *Client) Name() string { return "ilink" }
func (c *Client) Close() error { return nil }

func (c *Client) Poll(ctx context.Context) ([]message.Incoming, error) {
	cursor, err := c.store.GetChannelState("ilink.cursor")
	if err != nil {
		return nil, err
	}
	body := map[string]any{"get_updates_buf": cursor, "base_info": baseInfo{ChannelVersion: c.cfg.ChannelVersion}}
	var response updatesResponse
	timeout, _ := time.ParseDuration(c.cfg.LongPollTimeout)
	if err := c.post(ctx, "ilink/bot/getupdates", body, timeout, &response); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, nil
		}
		return nil, err
	}
	if response.ErrorCode == sessionExpiredCode {
		return nil, ErrSessionExpired
	}
	if response.ErrorCode != 0 {
		return nil, fmt.Errorf("iLink getupdates errcode=%d: %s", response.ErrorCode, response.ErrorMessage)
	}
	if response.Cursor != "" {
		if err := c.store.SetChannelState("ilink.cursor", response.Cursor); err != nil {
			return nil, err
		}
	}
	result := make([]message.Incoming, 0, len(response.Messages))
	for _, raw := range response.Messages {
		if raw.MessageType == 2 || raw.From == "" || (len(c.allowed) > 0 && !c.allowed[raw.From]) {
			continue
		}
		text := extractText(raw.Items)
		if strings.TrimSpace(text) == "" {
			continue
		}
		id := raw.MessageID.String()
		if id == "" || id == "0" {
			id = fmt.Sprintf("%s-%d", raw.From, raw.CreateTime)
		}
		if raw.ContextToken != "" {
			if err := c.store.SetChannelState("ilink.context."+raw.From, raw.ContextToken); err != nil {
				return nil, err
			}
		}
		at := time.UnixMilli(raw.CreateTime)
		if raw.CreateTime == 0 {
			at = time.Now()
		}
		result = append(result, message.Incoming{ID: id, Source: "ilink", Body: text, Channel: "ilink", Sender: raw.From, Conversation: raw.From, ReceivedAt: at})
	}
	return result, nil
}

func (c *Client) Send(ctx context.Context, to, text string) error {
	token, err := c.store.GetChannelState("ilink.context." + to)
	if err != nil {
		return err
	}
	if token == "" {
		return fmt.Errorf("缺少 %s 的 context_token；用户必须先给机器人发消息", to)
	}
	chunks := splitRunes(strings.TrimSpace(text), 3800)
	for i, chunk := range chunks {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		payload := map[string]any{"msg": map[string]any{
			"from_user_id": "", "to_user_id": to, "client_id": "tsk-" + randomHex(6), "message_type": 2, "message_state": 2,
			"item_list": []any{map[string]any{"type": 1, "text_item": map[string]string{"text": chunk}}}, "context_token": token,
		}, "base_info": baseInfo{ChannelVersion: c.cfg.ChannelVersion}}
		var ack struct {
			Ret          *int   `json:"ret"`
			ErrorCode    *int   `json:"errcode"`
			ErrorMessage string `json:"errmsg"`
		}
		if err := c.post(ctx, "ilink/bot/sendmessage", payload, 15*time.Second, &ack); err != nil {
			return err
		}
		if ack.Ret != nil && *ack.Ret != 0 {
			return fmt.Errorf("iLink sendmessage ret=%d: %s", *ack.Ret, ack.ErrorMessage)
		}
		if ack.ErrorCode != nil && *ack.ErrorCode != 0 {
			return fmt.Errorf("iLink sendmessage errcode=%d: %s", *ack.ErrorCode, ack.ErrorMessage)
		}
	}
	return nil
}

func (c *Client) post(parent context.Context, path string, body any, timeout time.Duration, out any) error {
	ctx, cancel := context.WithTimeout(parent, timeout+10*time.Second)
	defer cancel()
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(c.cfg.BaseURL, "/") + "/" + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	req.Header.Set("Authorization", "Bearer "+c.credentials.Token)
	req.Header.Set("X-WECHAT-UIN", randomUIN())
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s HTTP %d: %s", path, resp.StatusCode, truncateBytes(data, 300))
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("解析 %s 响应: %w", path, err)
	}
	return nil
}

func Login(ctx context.Context, cfg config.ChannelConfig) (Credentials, error) {
	client := &http.Client{}
	deadline := time.Now().Add(8 * time.Minute)
	refresh := 0
	for time.Now().Before(deadline) && refresh < 3 {
		qr, err := fetchQR(ctx, client, cfg.BaseURL)
		if err != nil {
			return Credentials{}, err
		}
		refresh++
		if err := displayQR(qr.Content, cfg.StatePath); err != nil {
			return Credentials{}, err
		}
		fmt.Println("请使用微信扫码并在手机上确认登录……")
		fetched := time.Now()
		scanned := false
		for time.Since(fetched) < 80*time.Second {
			status, err := pollQR(ctx, client, cfg.BaseURL, qr.QRCode)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return Credentials{}, err
				}
				fmt.Printf("查询扫码状态失败，正在重试：%v\n", err)
				continue
			}
			switch status.Status {
			case "scaned":
				if !scanned {
					fmt.Println("二维码已扫描，请在手机上确认。")
				}
				scanned = true
			case "confirmed":
				if status.Token == "" || status.BotID == "" {
					return Credentials{}, fmt.Errorf("iLink 确认响应缺少登录凭据")
				}
				credentials := Credentials{Token: status.Token, BotID: status.BotID, BaseURL: status.BaseURL, ScannedUser: status.User}
				if credentials.BaseURL == "" {
					credentials.BaseURL = cfg.BaseURL
				}
				if err := SaveCredentials(cfg.StatePath, credentials); err != nil {
					return Credentials{}, err
				}
				return credentials, nil
			case "expired":
				fetched = time.Time{}
			}
			select {
			case <-ctx.Done():
				return Credentials{}, ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
		fmt.Println("二维码即将过期，正在刷新……")
	}
	return Credentials{}, fmt.Errorf("扫码登录超时，请重新运行 taskian ilink login")
}

func fetchQR(ctx context.Context, client *http.Client, base string) (qrResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	endpoint := strings.TrimRight(base, "/") + "/ilink/bot/get_bot_qrcode?bot_type=3"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return qrResponse{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return qrResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return qrResponse{}, fmt.Errorf("get_bot_qrcode HTTP %d", resp.StatusCode)
	}
	var out qrResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return out, err
	}
	if out.QRCode == "" || out.Content == "" {
		return out, fmt.Errorf("get_bot_qrcode 响应缺少二维码")
	}
	return out, nil
}

func pollQR(parent context.Context, client *http.Client, base, key string) (qrStatus, error) {
	ctx, cancel := context.WithTimeout(parent, 40*time.Second)
	defer cancel()
	endpoint := strings.TrimRight(base, "/") + "/ilink/bot/get_qrcode_status?qrcode=" + url.QueryEscape(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return qrStatus{}, err
	}
	req.Header.Set("iLink-App-ClientVersion", "1")
	resp, err := client.Do(req)
	if err != nil {
		return qrStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return qrStatus{}, fmt.Errorf("get_qrcode_status HTTP %d", resp.StatusCode)
	}
	var out qrStatus
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	return out, err
}

func displayQR(content, statePath string) error {
	code, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return fmt.Errorf("生成二维码: %w", err)
	}
	bitmap := code.Bitmap()
	fmt.Println()
	for y := 0; y < len(bitmap); y += 2 {
		var line strings.Builder
		for x := range bitmap[y] {
			top := bitmap[y][x]
			bottom := false
			if y+1 < len(bitmap) {
				bottom = bitmap[y+1][x]
			}
			switch {
			case top && bottom:
				line.WriteRune('█')
			case top:
				line.WriteRune('▀')
			case bottom:
				line.WriteRune('▄')
			default:
				line.WriteRune(' ')
			}
		}
		fmt.Println(line.String())
	}
	dir := filepath.Dir(statePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	pngPath := filepath.Join(dir, "ilink-login-qr.png")
	if err := qrcode.WriteFile(content, qrcode.Medium, 512, pngPath); err != nil {
		return err
	}
	_ = os.Chmod(pngPath, 0o600)
	fmt.Printf("\n如果终端二维码无法识别，请打开：%s\n", pngPath)
	return nil
}

func LoadCredentials(path string) (Credentials, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Credentials{}, nil
	}
	if err != nil {
		return Credentials{}, fmt.Errorf("读取 iLink 登录状态: %w", err)
	}
	var out Credentials
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("解析 iLink 登录状态: %w", err)
	}
	return out, nil
}
func SaveCredentials(path string, c Credentials) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ilink-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	backup := path + ".bak"
	_ = os.Remove(backup)
	hadOld := false
	if _, err := os.Stat(path); err == nil {
		if err := os.Rename(path, backup); err != nil {
			return err
		}
		hadOld = true
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		if hadOld {
			_ = os.Rename(backup, path)
		}
		return err
	}
	if hadOld {
		_ = os.Remove(backup)
	}
	return os.Chmod(path, 0o600)
}
func Logout(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func extractText(items []item) string {
	for _, it := range items {
		if it.Type == 1 && it.Text != nil && strings.TrimSpace(it.Text.Text) != "" {
			return strings.TrimSpace(it.Text.Text)
		}
		if it.Type == 3 && it.Voice != nil && strings.TrimSpace(it.Voice.Text) != "" {
			return strings.TrimSpace(it.Voice.Text)
		}
	}
	return ""
}
func splitRunes(value string, max int) []string {
	r := []rune(value)
	if len(r) == 0 {
		return nil
	}
	var out []string
	for i := 0; i < len(r); i += max {
		end := i + max
		if end > len(r) {
			end = len(r)
		}
		out = append(out, string(r[i:end]))
	}
	return out
}
func randomHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)[:n]
}
func randomUIN() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1<<32))
	return base64.StdEncoding.EncodeToString([]byte(strconv.FormatInt(n.Int64(), 10)))
}
func truncateBytes(data []byte, max int) string {
	if len(data) > max {
		data = data[:max]
	}
	return string(data)
}
