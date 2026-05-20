package config

import "testing"

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
