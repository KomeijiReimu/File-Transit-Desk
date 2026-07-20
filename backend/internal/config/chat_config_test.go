package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestChatDefaultsExplicitValuesAndBounds(t *testing.T) {
	want := ChatConfig{
		WithdrawWindowSeconds:    300,
		MaxMessageChars:          2000,
		MaxMessageBytes:          8192,
		RetentionDays:            90,
		MaxMessages:              50000,
		SessionMessagesPerMinute: 20,
		IPMessagesPerMinute:      60,
		GlobalMessagesPerMinute:  300,
		CleanupBatch:             500,
	}
	if got := Default().Chat; got != want {
		t.Fatalf("chat defaults=%+v want=%+v", got, want)
	}
	exampleBytes, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	var example struct {
		Chat ChatConfig `yaml:"chat"`
	}
	if err := yaml.Unmarshal(exampleBytes, &example); err != nil {
		t.Fatalf("parse example config: %v", err)
	}
	if example.Chat != want {
		t.Fatalf("chat example=%+v want=%+v", example.Chat, want)
	}

	cfg := validTestConfig()
	cfg.Chat = ChatConfig{
		WithdrawWindowSeconds:    601,
		MaxMessageChars:          1234,
		MaxMessageBytes:          4321,
		RetentionDays:            45,
		MaxMessages:              12345,
		SessionMessagesPerMinute: 7,
		IPMessagesPerMinute:      17,
		GlobalMessagesPerMinute:  27,
		CleanupBatch:             321,
	}
	normalized, err := cfg.NormalizedClone()
	if err != nil {
		t.Fatalf("normalize explicit chat config: %v", err)
	}
	if normalized.Chat != cfg.Chat {
		t.Fatalf("explicit chat config was replaced: got=%+v want=%+v", normalized.Chat, cfg.Chat)
	}

	for name, mutate := range map[string]func(*Config){
		"withdraw zero":  func(c *Config) { c.Chat.WithdrawWindowSeconds = 0 },
		"chars zero":     func(c *Config) { c.Chat.MaxMessageChars = 0 },
		"bytes zero":     func(c *Config) { c.Chat.MaxMessageBytes = 0 },
		"retention zero": func(c *Config) { c.Chat.RetentionDays = 0 },
		"messages zero":  func(c *Config) { c.Chat.MaxMessages = 0 },
		"session rate":   func(c *Config) { c.Chat.SessionMessagesPerMinute = 100001 },
		"ip rate":        func(c *Config) { c.Chat.IPMessagesPerMinute = 0 },
		"global rate":    func(c *Config) { c.Chat.GlobalMessagesPerMinute = 0 },
		"cleanup batch":  func(c *Config) { c.Chat.CleanupBatch = 10001 },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := validTestConfig()
			mutate(candidate)
			if _, err := candidate.NormalizedClone(); err == nil {
				t.Fatalf("invalid chat config accepted: %+v", candidate.Chat)
			}
		})
	}
}

func TestChatConfigLoadDefaultsOldFilesAndPreservesExplicitValues(t *testing.T) {
	base := `auth:
  dev_allow_fixed_code: true
  admin:
    username: admin
    password_sha256: 2bb80d537b1da3e38bd30361aa855686bde0ba34388b29d94bb536a73f23c8db
`
	oldPath := filepath.Join(t.TempDir(), "old.yaml")
	if err := os.WriteFile(oldPath, []byte(base), 0600); err != nil {
		t.Fatalf("write old config: %v", err)
	}
	old, err := Load(oldPath)
	if err != nil {
		t.Fatalf("load old config without chat section: %v", err)
	}
	if old.Chat != Default().Chat {
		t.Fatalf("old config did not receive chat defaults: %+v", old.Chat)
	}

	explicitPath := filepath.Join(t.TempDir(), "explicit.yaml")
	explicit := base + `chat:
  withdraw_window_seconds: 600
  max_message_chars: 3000
  max_message_bytes: 12000
  retention_days: 30
  max_messages: 23456
  session_messages_per_minute: 9
  ip_messages_per_minute: 19
  global_messages_per_minute: 29
  cleanup_batch: 250
`
	if err := os.WriteFile(explicitPath, []byte(explicit), 0600); err != nil {
		t.Fatalf("write explicit chat config: %v", err)
	}
	loaded, err := Load(explicitPath)
	if err != nil {
		t.Fatalf("load explicit chat config: %v", err)
	}
	if loaded.Chat.WithdrawWindowSeconds != 600 || loaded.Chat.MaxMessageChars != 3000 || loaded.Chat.MaxMessageBytes != 12000 || loaded.Chat.RetentionDays != 30 || loaded.Chat.MaxMessages != 23456 || loaded.Chat.SessionMessagesPerMinute != 9 || loaded.Chat.IPMessagesPerMinute != 19 || loaded.Chat.GlobalMessagesPerMinute != 29 || loaded.Chat.CleanupBatch != 250 {
		t.Fatalf("explicit chat config was not preserved: %+v", loaded.Chat)
	}
}
