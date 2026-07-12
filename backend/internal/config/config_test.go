package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filetrans-backend/internal/security"
)

func TestPrepareAtomicCommitAndAbort(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatalf("write old config: %v", err)
	}
	prepared, normalized, err := PrepareAtomic(path, validTestConfig())
	if err != nil {
		t.Fatalf("prepare config: %v", err)
	}
	if normalized == nil || prepared.tempPath == "" {
		t.Fatalf("expected normalized config and prepared temp")
	}
	if current, err := os.ReadFile(path); err != nil || string(current) != "old" {
		t.Fatalf("prepare must not publish config, content=%q err=%v", current, err)
	}
	info, err := os.Stat(prepared.tempPath)
	if err != nil || info.IsDir() {
		t.Fatalf("expected synced config temp file, info=%v err=%v", info, err)
	}
	tempPath := prepared.tempPath
	prepared.Abort()
	prepared.Abort()
	if _, err := os.Stat(tempPath); prepared.tempPath != "" || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected idempotent abort to remove temp, path=%q err=%v", prepared.tempPath, err)
	}

	prepared, _, err = PrepareAtomic(path, validTestConfig())
	if err != nil {
		t.Fatalf("prepare commit config: %v", err)
	}
	published, err := prepared.Commit()
	if err != nil || !published {
		t.Fatalf("commit config: published=%v err=%v", published, err)
	}
	prepared.Abort()
	loaded, err := Load(path)
	if err != nil || loaded.Storage.Dirs[0].ID != "default" {
		t.Fatalf("expected committed config to load, config=%+v err=%v", loaded, err)
	}
	backup, err := os.ReadFile(path + ".bak")
	if err != nil || string(backup) != "old" {
		t.Fatalf("expected old config backup, content=%q err=%v", backup, err)
	}
}

func TestPreparedSaveCommitPublishedSemantics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	prepared, _, err := PrepareAtomic(path, validTestConfig())
	if err != nil {
		t.Fatalf("prepare rename failure: %v", err)
	}
	renameErr := errors.New("rename failed")
	prepared.rename = func(string, string) error { return renameErr }
	published, err := prepared.Commit()
	if published || !errors.Is(err, renameErr) {
		t.Fatalf("expected unpublished rename failure, published=%v err=%v", published, err)
	}
	tempPath := prepared.tempPath
	prepared.Abort()
	if _, err := os.Stat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected failed rename temp removed, err=%v", err)
	}

	prepared, _, err = PrepareAtomic(path, validTestConfig())
	if err != nil {
		t.Fatalf("prepare sync failure: %v", err)
	}
	syncErr := errors.New("sync failed")
	prepared.syncParent = func(string) error { return syncErr }
	published, err = prepared.Commit()
	if !published || !errors.Is(err, syncErr) {
		t.Fatalf("expected published sync failure, published=%v err=%v", published, err)
	}
	if _, loadErr := Load(path); loadErr != nil {
		t.Fatalf("expected renamed config to be visible after sync failure: %v", loadErr)
	}
	prepared.Abort()
}

func TestPrepareAtomicFailsWhenBackupCannotBePublished(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatalf("write old config: %v", err)
	}
	if err := os.Mkdir(path+".bak", 0700); err != nil {
		t.Fatalf("block backup path: %v", err)
	}
	prepared, normalized, err := PrepareAtomic(path, validTestConfig())
	if err == nil || prepared != nil || normalized != nil {
		t.Fatalf("expected backup publication check to fail, prepared=%v normalized=%v err=%v", prepared, normalized, err)
	}
	if current, readErr := os.ReadFile(path); readErr != nil || string(current) != "old" {
		t.Fatalf("expected main config unchanged, content=%q err=%v", current, readErr)
	}
}

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

	phc, err := security.Hash([]byte("secret"))
	if err != nil {
		t.Fatalf("hash PHC: %v", err)
	}
	c.Auth.Admin.PasswordHash = phc
	c.Auth.Admin.PasswordSHA256 = "not-a-hash"
	if err := c.validate(); err == nil {
		t.Fatalf("expected invalid rollback SHA rejected even when PHC is present")
	}
	c.Auth.Admin.PasswordSHA256 = "2bb80d537b1da3e38bd30361aa855686bde0eacd7162fef6a25fe97bf527a25b"
	if err := c.validate(); err != nil {
		t.Fatalf("expected valid PHC and rollback SHA: %v", err)
	}
	c.Auth.Admin.PasswordHash = "malformed"
	c.Auth.Admin.PasswordSHA256 = "2bb80d537b1da3e38bd30361aa855686bde0ba34388b29d94bb536a73f23c8db"
	if err := c.validate(); err == nil {
		t.Fatalf("expected malformed PHC not to fall back to SHA")
	}
	c.Auth.Admin.PasswordHash = ""
	c.Auth.Admin.PasswordSHA256 = ""
	if err := c.validate(); err == nil {
		t.Fatalf("expected missing admin password hashes to fail")
	}
}

func TestConfigPreservesAdminPHCAndAbuseLimit(t *testing.T) {
	c := validTestConfig()
	phc, err := security.Hash([]byte("save-secret"))
	if err != nil {
		t.Fatalf("hash PHC: %v", err)
	}
	c.Auth.Admin.PasswordHash = phc
	c.Auth.Admin.PasswordSHA256 = "2bb80d537b1da3e38bd30361aa855686bde0eacd7162fef6a25fe97bf527a25b"
	c.Abuse.Login.MaxConcurrentAdminVerifications = 0
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := SaveAtomic(path, c); err != nil {
		t.Fatalf("save config: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if loaded.Auth.Admin.PasswordHash != phc || loaded.Auth.Admin.PasswordSHA256 != c.Auth.Admin.PasswordSHA256 || loaded.Abuse.Login.MaxConcurrentAdminVerifications != 0 {
		t.Fatalf("expected PHC, legacy rollback hash, and explicit zero limit preserved: %+v", loaded.Auth.Admin)
	}
	if Default().Abuse.Login.MaxConcurrentAdminVerifications != 2 {
		t.Fatalf("expected default admin verification concurrency to be 2")
	}
	loaded.Abuse.Login.MaxConcurrentAdminVerifications = 9
	if err := loaded.validate(); err == nil {
		t.Fatalf("expected admin verification concurrency above 8 rejected")
	}
}

func TestConfigValidatesTrustedProxyCIDRs(t *testing.T) {
	c := validTestConfig()
	c.Server.TrustProxyHeaders = true
	if err := c.validate(); err == nil {
		t.Fatalf("expected trusted proxy mode without CIDRs rejected")
	}
	c.Server.TrustedProxyCIDRs = []string{"172.28.0.0/24", "2001:db8::/32"}
	if err := c.validate(); err != nil {
		t.Fatalf("expected IPv4 and IPv6 trusted proxy CIDRs: %v", err)
	}
	c.Server.TrustProxyHeaders = false
	if err := c.validate(); err == nil {
		t.Fatalf("expected CIDRs rejected when proxy trust is disabled")
	}
	c.Server.TrustProxyHeaders = true
	c.Server.TrustedProxyCIDRs = []string{"not-a-cidr"}
	if err := c.validate(); err == nil {
		t.Fatalf("expected malformed CIDR rejected")
	}
	c.Server.TrustedProxyCIDRs = make([]string, 65)
	for i := range c.Server.TrustedProxyCIDRs {
		c.Server.TrustedProxyCIDRs[i] = "127.0.0.1/32"
	}
	if err := c.validate(); err == nil {
		t.Fatalf("expected more than 64 CIDRs rejected")
	}
}

func TestConfigAbuseDefaultsAndBounds(t *testing.T) {
	c := Default()
	if c.Abuse.Login.GlobalPerMinute != 120 || c.Abuse.Login.IPMaxFailures != 10 || c.Abuse.Login.WindowSeconds != 180 || c.Abuse.Login.BlockSeconds != 90 {
		t.Fatalf("unexpected login abuse defaults: %+v", c.Abuse.Login)
	}
	want := CreationAbuseConfig{TokenGlobalPerMinute: 120, TokenPerSessionPerMinute: 30, LeaseGlobalPerMinute: 600, LeasePerOwnerPerMinute: 60, PublicLeasePerIPPerMinute: 120, MaxActiveTokens: 1000, MaxOutstandingLeasesTotal: 5000, MaxOutstandingLeasesOwner: 64}
	if c.Abuse.Creation != want {
		t.Fatalf("unexpected creation abuse defaults: got=%+v want=%+v", c.Abuse.Creation, want)
	}
	if c.Abuse.Uploads != (UploadAbuseConfig{Global: 16, PerResource: 8, PerSession: 4, PerToken: 2}) {
		t.Fatalf("unexpected upload concurrency defaults: %+v", c.Abuse.Uploads)
	}
	if c.Storage.MinFreeMB != 1024 || c.Storage.MinFreePercent != 5 {
		t.Fatalf("unexpected storage reserve defaults: %+v", c.Storage)
	}
	c = validTestConfig()
	c.Abuse.Login.IPMaxFailures = 1001
	if err := c.validate(); err == nil {
		t.Fatalf("expected unreasonable login limit rejected")
	}
	c = validTestConfig()
	c.Abuse.Creation.MaxOutstandingLeasesOwner = -1
	if err := c.validate(); err == nil {
		t.Fatalf("expected negative creation limit rejected")
	}
	c = validTestConfig()
	c.Abuse.Login.GlobalPerMinute = 0
	c.Abuse.Creation = CreationAbuseConfig{}
	c.Abuse.Uploads = UploadAbuseConfig{}
	c.Storage.MinFreeMB = 0
	c.Storage.MinFreePercent = 0
	if err := c.validate(); err != nil {
		t.Fatalf("expected explicit zero limits to disable controls: %v", err)
	}
	c = validTestConfig()
	c.Abuse.Uploads.Global = 10001
	if err := c.validate(); err == nil {
		t.Fatalf("expected unreasonable upload concurrency rejected")
	}
	c = validTestConfig()
	c.Storage.MinFreePercent = 101
	if err := c.validate(); err == nil {
		t.Fatalf("expected invalid disk reserve percentage rejected")
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

func TestConfigAllowsLargeLANUploadLimit(t *testing.T) {
	// 局域网传输可能需要 10GB 以上的大文件上传；只保留极高防误配上限，不把 50GB 这类常见私有传输场景挡掉。
	c := validTestConfig()
	c.Storage.UploadMaxMB = 51200
	c.Storage.UploadMaxFileMB = 51200
	c.Tokens.UploadMaxMB = 51200
	if err := c.validate(); err != nil {
		t.Fatalf("expected 50GB upload limit to pass validation: %v", err)
	}

	c.Storage.UploadMaxMB = maxUploadLimitMB + 1
	c.Storage.UploadMaxFileMB = c.Storage.UploadMaxMB
	if err := c.validate(); err == nil {
		t.Fatalf("expected upload limit above maxUploadLimitMB to fail")
	}
}

func TestConfigValidatesUploadLeaseTTL(t *testing.T) {
	c := validTestConfig()
	if c.Auth.UploadLeaseTTLSeconds != 1800 {
		t.Fatalf("expected default upload lease ttl 1800, got %d", c.Auth.UploadLeaseTTLSeconds)
	}
	c.Auth.UploadLeaseTTLSeconds = c.Auth.SessionTTLSeconds + 1
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "auth.upload_lease_ttl_seconds") {
		t.Fatalf("expected upload lease ttl validation error, got %v", err)
	}
}

func TestLoadPreservesLargeUploadLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `auth:
  dev_allow_fixed_code: true
  admin:
    username: admin
    password_sha256: 2bb80d537b1da3e38bd30361aa855686bde0ba34388b29d94bb536a73f23c8db
storage:
  upload_max_mb: 51200
  upload_max_file_mb: 51200
  dirs:
    - id: default
      name: Default
      path: /tmp
      allow_download: true
      allow_upload: true
`
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if c.Storage.UploadMaxMB != 51200 || c.Storage.UploadMaxFileMB != 51200 {
		t.Fatalf("expected 51200MB limits to be preserved, got request=%d file=%d", c.Storage.UploadMaxMB, c.Storage.UploadMaxFileMB)
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

func TestConfigAllowsFullyDisabledLegacyResource(t *testing.T) {
	c := validTestConfig()
	c.Storage.Dirs = []Dir{{ID: "legacy", Name: "Legacy", Type: ResourceDirectory, Path: "/legacy", AllowDownload: false, AllowUpload: false}}
	normalized, err := c.NormalizedClone()
	if err != nil {
		t.Fatalf("expected disabled legacy resource to remain representable: %v", err)
	}
	if normalized.Storage.Dirs[0].AllowDownload || normalized.Storage.Dirs[0].AllowUpload {
		t.Fatalf("expected disabled permissions preserved: %+v", normalized.Storage.Dirs[0])
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

func TestDefaultKeepaliveIdleTimeoutAndValidation(t *testing.T) {
	cfg := Default()
	if cfg.Server.KeepaliveIdleTimeoutSeconds != 120 {
		t.Fatalf("unexpected keepalive idle timeout default: %d", cfg.Server.KeepaliveIdleTimeoutSeconds)
	}
	cfg = validTestConfig()
	cfg.Server.KeepaliveIdleTimeoutSeconds = 86401
	if err := cfg.validate(); err == nil {
		t.Fatalf("expected excessive keepalive idle timeout rejected")
	}
	cfg = validTestConfig()
	clone := cfg.Clone()
	if clone.Server.KeepaliveIdleTimeoutSeconds != cfg.Server.KeepaliveIdleTimeoutSeconds {
		t.Fatalf("clone did not preserve keepalive idle timeout")
	}
	cfg.Server.KeepaliveIdleTimeoutSeconds = 333
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := SaveAtomic(path, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if loaded.Server.KeepaliveIdleTimeoutSeconds != 333 {
		t.Fatalf("save/load did not preserve keepalive idle timeout: %d", loaded.Server.KeepaliveIdleTimeoutSeconds)
	}
}

func TestDirectoryListingLimitsDefaultsValidationAndClone(t *testing.T) {
	cfg := Default()
	if cfg.Storage.DirectoryListScanLimit != 5000 || cfg.Storage.DirectoryListMaxPageSize != 200 || cfg.FilePicker.MaxScanEntries != 5000 || cfg.FilePicker.MaxPageSize != 200 {
		t.Fatalf("unexpected listing defaults: storage=%+v picker=%+v", cfg.Storage, cfg.FilePicker)
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Storage.DirectoryListScanLimit = 99 },
		func(c *Config) { c.Storage.DirectoryListScanLimit = 100001 },
		func(c *Config) { c.Storage.DirectoryListMaxPageSize = 1001 },
		func(c *Config) { c.FilePicker.MaxScanEntries = 99 },
		func(c *Config) { c.FilePicker.MaxScanEntries = 100001 },
		func(c *Config) { c.FilePicker.MaxPageSize = 201 },
		func(c *Config) { c.Storage.DirectoryListScanLimit, c.Storage.DirectoryListMaxPageSize = 100, 101 },
		func(c *Config) { c.FilePicker.MaxScanEntries, c.FilePicker.MaxPageSize = 100, 101 },
	} {
		candidate := validTestConfig()
		mutate(candidate)
		if err := candidate.validate(); err == nil {
			t.Fatalf("expected invalid listing configuration rejected: storage=%+v picker=%+v", candidate.Storage, candidate.FilePicker)
		}
	}
	cfg = validTestConfig()
	cfg.Storage.DirectoryListScanLimit = 1234
	cfg.Storage.DirectoryListMaxPageSize = 123
	cfg.FilePicker.MaxScanEntries = 2345
	cfg.FilePicker.MaxPageSize = 124
	clone := cfg.Clone()
	if clone.Storage.DirectoryListScanLimit != 1234 || clone.Storage.DirectoryListMaxPageSize != 123 || clone.FilePicker.MaxScanEntries != 2345 || clone.FilePicker.MaxPageSize != 124 {
		t.Fatalf("clone did not preserve listing limits")
	}
}

func TestDownloadHashVerificationDefaultsValidationAndClone(t *testing.T) {
	cfg := Default()
	if cfg.Downloads.MaxConcurrentHashes != 2 || cfg.Downloads.VerifyHashOnEveryRequest {
		t.Fatalf("unexpected download hash defaults: %+v", cfg.Downloads)
	}
	for _, value := range []int{0, 17} {
		candidate := validTestConfig()
		candidate.Downloads.MaxConcurrentHashes = value
		if value == 0 {
			// Explicit validation is tested before normalize; loaded omitted values are filled by defaults.
			if err := candidate.validate(); err == nil {
				t.Fatalf("expected zero max concurrent hashes rejected")
			}
		} else if err := candidate.validate(); err == nil {
			t.Fatalf("expected excessive max concurrent hashes rejected")
		}
	}
	cfg = validTestConfig()
	cfg.Downloads.MaxConcurrentHashes = 7
	cfg.Downloads.VerifyHashOnEveryRequest = true
	clone := cfg.Clone()
	if clone.Downloads.MaxConcurrentHashes != 7 || !clone.Downloads.VerifyHashOnEveryRequest {
		t.Fatalf("clone did not preserve download hash settings")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := SaveAtomic(path, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if loaded.Downloads.MaxConcurrentHashes != 7 || !loaded.Downloads.VerifyHashOnEveryRequest {
		t.Fatalf("save/load did not preserve download hash settings: %+v", loaded.Downloads)
	}
}

func TestAuditPolicyDefaultsValidationAndClone(t *testing.T) {
	cfg := Default()
	if cfg.Audit.UnauthorizedSampleSeconds != 60 || cfg.Audit.UnauthorizedGlobalPerMinute != 120 || cfg.Audit.PruneEveryWrites != 100 {
		t.Fatalf("unexpected audit policy defaults: %+v", cfg.Audit)
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Audit.UnauthorizedSampleSeconds = -1 },
		func(c *Config) { c.Audit.UnauthorizedGlobalPerMinute = -1 },
		func(c *Config) { c.Audit.PruneEveryWrites = -1 },
		func(c *Config) { c.Audit.UnauthorizedSampleSeconds = 86401 },
	} {
		candidate := validTestConfig()
		mutate(candidate)
		if err := candidate.validate(); err == nil {
			t.Fatalf("expected invalid audit policy rejected: %+v", candidate.Audit)
		}
	}
	cfg = validTestConfig()
	cfg.Audit.UnauthorizedSampleSeconds = 0
	cfg.Audit.UnauthorizedGlobalPerMinute = 0
	cfg.Audit.PruneEveryWrites = 0
	clone := cfg.Clone()
	if clone.Audit.UnauthorizedSampleSeconds != 0 || clone.Audit.UnauthorizedGlobalPerMinute != 0 || clone.Audit.PruneEveryWrites != 0 {
		t.Fatalf("clone did not preserve disabled audit policy: %+v", clone.Audit)
	}
}

func TestUploadTempCleanupBoundsDefaultsValidationAndClone(t *testing.T) {
	cfg := Default()
	if cfg.Storage.UploadTempCleanupMaxEntries != 50000 || cfg.Storage.UploadTempCleanupMaxDurationSeconds != 5 {
		t.Fatalf("unexpected upload cleanup defaults: %+v", cfg.Storage)
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Storage.UploadTempCleanupMaxEntries = 99 },
		func(c *Config) { c.Storage.UploadTempCleanupMaxEntries = 1000001 },
		func(c *Config) { c.Storage.UploadTempCleanupMaxDurationSeconds = 0 },
		func(c *Config) { c.Storage.UploadTempCleanupMaxDurationSeconds = 61 },
	} {
		candidate := validTestConfig()
		mutate(candidate)
		if err := candidate.validate(); err == nil {
			t.Fatalf("expected invalid upload cleanup bounds rejected: %+v", candidate.Storage)
		}
	}
	cfg = validTestConfig()
	cfg.Storage.UploadTempCleanupMaxEntries = 1234
	cfg.Storage.UploadTempCleanupMaxDurationSeconds = 9
	clone := cfg.Clone()
	if clone.Storage.UploadTempCleanupMaxEntries != 1234 || clone.Storage.UploadTempCleanupMaxDurationSeconds != 9 {
		t.Fatalf("clone did not preserve cleanup bounds: %+v", clone.Storage)
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
