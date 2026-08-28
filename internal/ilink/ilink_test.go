package ilink

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ximengyi/taskian/internal/config"
	"github.com/ximengyi/taskian/internal/store"
)

func TestPollAndSendText(t *testing.T) {
	var sent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token-1" {
			t.Errorf("missing auth")
		}
		switch r.URL.Path {
		case "/ilink/bot/getupdates":
			response := map[string]any{
				"ret": 0, "errcode": 0, "get_updates_buf": "cursor-2",
				"msgs": []any{map[string]any{
					"message_id": 101, "from_user_id": "owner", "create_time_ms": time.Now().UnixMilli(),
					"message_type": 1, "context_token": "ctx-1",
					"item_list": []any{map[string]any{"type": 1, "text_item": map[string]string{"text": "#status"}}},
				}},
			}
			_ = json.NewEncoder(w).Encode(response)
		case "/ilink/bot/sendmessage":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			data, _ := json.Marshal(body)
			sent = string(data)
			_ = json.NewEncoder(w).Encode(map[string]any{"ret": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	credentialsPath := filepath.Join(dir, "ilink.json")
	if err := SaveCredentials(credentialsPath, Credentials{Token: "token-1", BotID: "bot", BaseURL: server.URL, ScannedUser: "owner"}); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	client, err := New(config.ChannelConfig{BaseURL: server.URL, StatePath: credentialsPath, ChannelVersion: "taskian-test", LongPollTimeout: "1s"}, state)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := client.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Body != "#status" {
		t.Fatalf("messages=%+v", messages)
	}
	if err := client.Send(context.Background(), "owner", "reply text"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent, "reply text") || !strings.Contains(sent, "ctx-1") {
		t.Fatalf("sent=%s", sent)
	}
}

func TestSplitRunes(t *testing.T) {
	parts := splitRunes("你好🙂abc", 3)
	if len(parts) != 2 || parts[0] != "你好🙂" {
		t.Fatalf("parts=%v", parts)
	}
}

func TestSaveCredentialsCanReplaceExistingBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ilink.json")
	if err := SaveCredentials(path, Credentials{Token: "old", BotID: "one", ScannedUser: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := SaveCredentials(path, Credentials{Token: "new", BotID: "two", ScannedUser: "u"}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "new" || got.BotID != "two" {
		t.Fatalf("credentials=%+v", got)
	}
}

func TestLoginFlowPersistsConfirmedBinding(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			_ = json.NewEncoder(w).Encode(map[string]any{"qrcode": "key-1", "qrcode_img_content": "https://example.test/confirm/key-1"})
		case "/ilink/bot/get_qrcode_status":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "confirmed", "bot_token": "secret", "ilink_bot_id": "bot-1", "baseurl": server.URL, "ilink_user_id": "owner"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "ilink.json")
	got, err := Login(context.Background(), config.ChannelConfig{BaseURL: server.URL, StatePath: path})
	if err != nil {
		t.Fatal(err)
	}
	if got.BotID != "bot-1" || got.ScannedUser != "owner" {
		t.Fatalf("binding=%+v", got)
	}
	saved, err := LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Token != "secret" {
		t.Fatalf("saved=%+v", saved)
	}
}
