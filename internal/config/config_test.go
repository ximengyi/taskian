package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadResolvesInboxUnderVaultAndDefaultsSkipExisting(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "vault")
	project := filepath.Join(t.TempDir(), "project")
	for _, path := range []string{vault, project} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	raw := map[string]any{
		"vault_path": vault,
		"agents": map[string]any{
			"CODEX": map[string]any{
				"command": "codex",
				"args":    []string{"exec", "{prompt}"},
				"timeout": "1m",
			},
		},
		"projects": map[string]any{
			"Demo": map[string]any{"path": project, "agent": "CODEX"},
		},
	}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InboxDir != filepath.Join(vault, "Wechatian") {
		t.Fatalf("inbox=%q", cfg.InboxDir)
	}
	if !cfg.SkipExistingOnFirstRun {
		t.Fatal("skip_existing_on_first_run should default to true")
	}
	if _, ok := cfg.Agents["codex"]; !ok {
		t.Fatal("agent name was not normalized")
	}
	if got := cfg.Projects["demo"].Agent; got != "codex" {
		t.Fatalf("project agent=%q", got)
	}
}
