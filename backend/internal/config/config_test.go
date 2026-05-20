package config

import "testing"

func TestConfigValidatesAdminPasswordHash(t *testing.T) {
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
