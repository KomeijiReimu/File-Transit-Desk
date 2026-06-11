package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigValidatesAdminPasswordHash(t *testing.T) {
	// 管理员密码只允许 SHA-256 十六进制摘要，避免误把明文密码写进配置。
	c := Default()
	c.Auth.DevAllowFixedCode = true
	c.Auth.TOTPSecret = ""
	c.Auth.Admin.Username = "admin"
	c.Auth.Admin.PasswordSHA256 = "not-a-hash"
	if err := c.validate(); err == nil {
		t.Fatalf("expected invalid admin hash to fail validation")
	}

	c.Auth.Admin.PasswordSHA256 = "2bb80d537b1da3e38bd30361aa855686bde0ba34388b29d94bb536a73f23c8db"
	if err := c.validate(); err != nil {
		t.Fatalf("expected valid admin config: %v", err)
	}
}

func TestConfigValidatesDownloadContentHashLimit(t *testing.T) {
	// content_hash_max_mb=0 表示所有文件都计算哈希，负数才是非法配置。
	c := Default()
	c.Auth.DevAllowFixedCode = true
	c.Auth.TOTPSecret = ""
	c.Auth.Admin.Username = "admin"
	c.Auth.Admin.PasswordSHA256 = "2bb80d537b1da3e38bd30361aa855686bde0ba34388b29d94bb536a73f23c8db"
	c.Downloads.ContentHashMaxMB = -1
	if err := c.validate(); err == nil {
		t.Fatalf("expected negative content hash limit to fail validation")
	}
	c.Downloads.ContentHashMaxMB = 0
	if err := c.validate(); err != nil {
		t.Fatalf("expected zero content hash limit to mean no limit: %v", err)
	}
}

func TestDefaultUploadBlacklistIsEmpty(t *testing.T) {
	// 默认不再预置阻断扩展名，是否限制危险类型交由管理员按场景配置。
	c := Default()
	if len(c.Storage.BlockedExtensions) != 0 {
		t.Fatalf("expected default blocked extensions to be empty, got %v", c.Storage.BlockedExtensions)
	}
	if len(c.FilePicker.DenyNames) != 0 || len(c.FilePicker.DenyPatterns) != 0 {
		t.Fatalf("expected default file picker filters to be empty, got names=%v patterns=%v", c.FilePicker.DenyNames, c.FilePicker.DenyPatterns)
	}
}

func TestLoadReportsFriendlyYAMLError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	bad := []byte("storage:\n  shares: []\n    roots: []\n")
	if err := os.WriteFile(path, bad, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatalf("expected malformed yaml to fail")
	}
	message := err.Error()
	for _, want := range []string{"配置文件格式错误", "config.yaml", "file_picker", "storage.shares"} {
		if !strings.Contains(message, want) {
			t.Fatalf("expected friendly error to contain %q, got %q", want, message)
		}
	}
}

func TestConfigValidatesUploadExtensionFormat(t *testing.T) {
	c := validTestConfig()
	c.Storage.AllowedExtensions = []string{"*"}
	if _, err := c.NormalizedClone(); err == nil {
		t.Fatalf("expected wildcard extension to fail validation")
	}
	c.Storage.AllowedExtensions = []string{".tar.gz"}
	if _, err := c.NormalizedClone(); err == nil {
		t.Fatalf("expected compound extension to fail validation because filepath.Ext only checks the last suffix")
	}
}

func TestConfigValidatesFilePickerRoots(t *testing.T) {
	c := validTestConfig()
	c.FilePicker.Roots = []FilePickerRoot{{ID: "bad root", Name: "坏根", Path: "/tmp", AllowSelectFiles: true}}
	if _, err := c.NormalizedClone(); err == nil {
		t.Fatalf("expected invalid file picker root id to fail validation")
	}
	c.FilePicker.Roots = []FilePickerRoot{{ID: "pick", Name: "可选根", Path: "/tmp"}}
	if _, err := c.NormalizedClone(); err == nil {
		t.Fatalf("expected root without selectable type to fail validation")
	}
}

func TestConfigNormalizesTokenTTLUpperBound(t *testing.T) {
	// 令牌默认有效期不能超过最长有效期，避免旧配置或手工 YAML 写出长期公开链接。
	c := validTestConfig()
	c.Tokens.DefaultTTLSeconds = 172800
	c.Tokens.MaxTTLSeconds = 86400
	next, err := c.NormalizedClone()
	if err != nil {
		t.Fatalf("expected token ttl normalization to pass: %v", err)
	}
	if next.Tokens.DefaultTTLSeconds != next.Tokens.MaxTTLSeconds {
		t.Fatalf("expected default ttl to be clamped to max ttl, got default=%d max=%d", next.Tokens.DefaultTTLSeconds, next.Tokens.MaxTTLSeconds)
	}
}

func validTestConfig() *Config {
	c := Default()
	c.Auth.DevAllowFixedCode = true
	c.Auth.TOTPSecret = ""
	c.Auth.Admin.Username = "admin"
	c.Auth.Admin.PasswordSHA256 = "2bb80d537b1da3e38bd30361aa855686bde0ba34388b29d94bb536a73f23c8db"
	c.Storage.Dirs = []Dir{{ID: "default", Name: "Default", Path: "/tmp", AllowDownload: true, AllowUpload: true}}
	return c
}
