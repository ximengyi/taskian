package feishu

import (
	"path/filepath"
	"testing"
	"time"
)

func TestManualCredentialsAndBindingCode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feishu.json")
	creds, err := SetupManual(path, "cli_test", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if creds.BindCode == "" || !validBinding("绑定 "+creds.BindCode, creds, time.Now()) {
		t.Fatalf("invalid binding credentials: %+v", creds)
	}
	loaded, err := LoadCredentials(path)
	if err != nil || loaded.AppID != "cli_test" || loaded.AppSecret != "secret" {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	if validBinding("绑定 "+creds.BindCode, creds, creds.BindExpires.Add(time.Second)) {
		t.Fatal("expired binding code was accepted")
	}
}
