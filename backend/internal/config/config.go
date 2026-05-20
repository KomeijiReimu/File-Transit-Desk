package config

import (
	"encoding/base32"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Database  DatabaseConfig  `yaml:"database"`
	Auth      AuthConfig      `yaml:"auth"`
	Downloads DownloadsConfig `yaml:"downloads"`
	Web       WebConfig       `yaml:"web"`
	CORS      CORSConfig      `yaml:"cors"`
	Storage   StorageConfig   `yaml:"storage"`
	Tokens    TokensConfig    `yaml:"tokens"`
	Audit     AuditConfig     `yaml:"audit"`
}

type ServerConfig struct {
	Host              string `yaml:"host"`
	Port              int    `yaml:"port"`
	TrustProxyHeaders bool   `yaml:"trust_proxy_headers"`
}

type DatabaseConfig struct {
	Path string `yaml:"path"`
}

type AuthConfig struct {
	TOTPSecret         string      `yaml:"totp_secret"`
	DevAllowFixedCode  bool        `yaml:"dev_allow_fixed_code"`
	SessionTTLSeconds  int64       `yaml:"session_ttl_seconds"`
	IdleTimeoutSeconds int64       `yaml:"idle_timeout_seconds"`
	IdleGraceSeconds   int64       `yaml:"idle_grace_seconds"`
	CookieSecure       bool        `yaml:"cookie_secure"`
	Admin              AdminConfig `yaml:"admin"`
}

type DownloadsConfig struct {
	LeaseTTLSeconds    int64 `yaml:"lease_ttl_seconds"`
	LeaseMaxTTLSeconds int64 `yaml:"lease_max_ttl_seconds"`
	ContentHashMaxMB   int   `yaml:"content_hash_max_mb"`
}

type AdminConfig struct {
	Username       string `yaml:"username"`
	PasswordSHA256 string `yaml:"password_sha256"`
}

type WebConfig struct {
	StaticDir string `yaml:"static_dir"`
}

type CORSConfig struct {
	AllowOrigins []string `yaml:"allow_origins"`
}

type StorageConfig struct {
	UploadMaxMB       int      `yaml:"upload_max_mb"`
	UploadMaxFileMB   int      `yaml:"upload_max_file_mb"`
	UploadMaxFiles    int      `yaml:"upload_max_files"`
	AllowedExtensions []string `yaml:"allowed_extensions"`
	BlockedExtensions []string `yaml:"blocked_extensions"`
	Dirs              []Dir    `yaml:"dirs"`
}

type TokensConfig struct {
	DefaultTTLSeconds int64 `yaml:"default_ttl_seconds"`
	UploadMaxMB       int   `yaml:"upload_max_mb"`
}

type AuditConfig struct {
	Retain int `yaml:"retain"`
}

type Dir struct {
	ID            string `yaml:"id" json:"id"`
	Name          string `yaml:"name" json:"name"`
	Path          string `yaml:"path" json:"path"`
	AllowDownload bool   `yaml:"allow_download" json:"allowDownload"`
	AllowUpload   bool   `yaml:"allow_upload" json:"allowUpload"`
}

func Load(path string) (*Config, error) {
	// 先加载默认值，再让 YAML 覆盖，保证新增配置项对旧配置文件保持兼容。
	c := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, err
	}
	c.normalize()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func Default() *Config {
	c := &Config{}
	c.Server.Host = "0.0.0.0"
	c.Server.Port = 8080
	c.Database.Path = "./data/filetrans.db"
	c.Auth.SessionTTLSeconds = int64((24 * time.Hour) / time.Second)
	c.Auth.IdleTimeoutSeconds = int64((3 * time.Minute) / time.Second)
	c.Auth.IdleGraceSeconds = 30
	c.Downloads.LeaseTTLSeconds = int64((2 * time.Hour) / time.Second)
	c.Downloads.LeaseMaxTTLSeconds = int64((6 * time.Hour) / time.Second)
	c.Downloads.ContentHashMaxMB = 64
	c.Storage.UploadMaxMB = 512
	c.Storage.UploadMaxFileMB = 512
	c.Storage.UploadMaxFiles = 20
	c.Tokens.DefaultTTLSeconds = 3600
	c.Tokens.UploadMaxMB = 1024
	c.Audit.Retain = 1000
	c.CORS.AllowOrigins = []string{"http://localhost:5173"}
	return c
}

func (c *Config) normalize() {
	// 归一化只做安全的默认值补齐和大小写/空白处理，不在这里放宽 validate 的硬约束。
	c.Auth.TOTPSecret = normalizeTOTPSecret(c.Auth.TOTPSecret)
	c.Auth.Admin.Username = strings.TrimSpace(c.Auth.Admin.Username)
	c.Auth.Admin.PasswordSHA256 = strings.TrimSpace(c.Auth.Admin.PasswordSHA256)
	if c.Server.Host == "" {
		c.Server.Host = "0.0.0.0"
	}
	if c.Server.Port <= 0 {
		c.Server.Port = 8080
	}
	if c.Database.Path == "" {
		c.Database.Path = "./data/filetrans.db"
	}
	if c.Auth.SessionTTLSeconds <= 0 {
		c.Auth.SessionTTLSeconds = int64((24 * time.Hour) / time.Second)
	}
	if c.Auth.IdleTimeoutSeconds <= 0 {
		c.Auth.IdleTimeoutSeconds = int64((3 * time.Minute) / time.Second)
	}
	if c.Auth.IdleGraceSeconds < 0 {
		c.Auth.IdleGraceSeconds = 0
	}
	if c.Downloads.LeaseTTLSeconds <= 0 {
		c.Downloads.LeaseTTLSeconds = int64((2 * time.Hour) / time.Second)
	}
	if c.Downloads.LeaseMaxTTLSeconds <= 0 {
		c.Downloads.LeaseMaxTTLSeconds = int64((6 * time.Hour) / time.Second)
	}
	if c.Downloads.LeaseTTLSeconds > c.Downloads.LeaseMaxTTLSeconds {
		c.Downloads.LeaseTTLSeconds = c.Downloads.LeaseMaxTTLSeconds
	}
	if c.Storage.UploadMaxMB <= 0 {
		c.Storage.UploadMaxMB = 512
	}
	if c.Storage.UploadMaxFileMB <= 0 {
		c.Storage.UploadMaxFileMB = c.Storage.UploadMaxMB
	}
	if c.Storage.UploadMaxFiles <= 0 {
		c.Storage.UploadMaxFiles = 20
	}
	c.Storage.AllowedExtensions = normalizeExtensions(c.Storage.AllowedExtensions)
	c.Storage.BlockedExtensions = normalizeExtensions(c.Storage.BlockedExtensions)
	if c.Tokens.DefaultTTLSeconds <= 0 {
		c.Tokens.DefaultTTLSeconds = 3600
	}
	if c.Audit.Retain <= 0 {
		c.Audit.Retain = 1000
	}
}

func (c *Config) validate() error {
	// 启动阶段集中拒绝危险配置，避免服务运行后才在请求路径上暴露问题。
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port must be between 1 and 65535")
	}
	if c.Auth.IdleTimeoutSeconds < 30 {
		return fmt.Errorf("auth.idle_timeout_seconds must be at least 30")
	}
	if c.Auth.IdleTimeoutSeconds > c.Auth.SessionTTLSeconds {
		return fmt.Errorf("auth.idle_timeout_seconds must not exceed auth.session_ttl_seconds")
	}
	if c.Auth.IdleGraceSeconds > c.Auth.IdleTimeoutSeconds {
		return fmt.Errorf("auth.idle_grace_seconds must not exceed auth.idle_timeout_seconds")
	}
	if c.Downloads.LeaseTTLSeconds < 60 {
		return fmt.Errorf("downloads.lease_ttl_seconds must be at least 60")
	}
	if c.Downloads.LeaseMaxTTLSeconds < c.Downloads.LeaseTTLSeconds {
		return fmt.Errorf("downloads.lease_max_ttl_seconds must be greater than or equal to downloads.lease_ttl_seconds")
	}
	if c.Downloads.ContentHashMaxMB < 0 || c.Downloads.ContentHashMaxMB > 102400 {
		return fmt.Errorf("downloads.content_hash_max_mb must be between 0 and 102400")
	}
	if c.Storage.UploadMaxMB < 1 || c.Storage.UploadMaxMB > 10240 {
		return fmt.Errorf("storage.upload_max_mb must be between 1 and 10240")
	}
	if c.Storage.UploadMaxFileMB < 1 || c.Storage.UploadMaxFileMB > c.Storage.UploadMaxMB {
		return fmt.Errorf("storage.upload_max_file_mb must be between 1 and storage.upload_max_mb")
	}
	if c.Storage.UploadMaxFiles < 1 || c.Storage.UploadMaxFiles > 1000 {
		return fmt.Errorf("storage.upload_max_files must be between 1 and 1000")
	}
	if c.Tokens.UploadMaxMB < 0 || c.Tokens.UploadMaxMB > 102400 {
		return fmt.Errorf("tokens.upload_max_mb must be between 0 and 102400")
	}
	if overlap := intersectExtensions(c.Storage.AllowedExtensions, c.Storage.BlockedExtensions); overlap != "" {
		return fmt.Errorf("extension %q cannot appear in both storage.allowed_extensions and storage.blocked_extensions", overlap)
	}
	if c.Tokens.DefaultTTLSeconds < 60 {
		return fmt.Errorf("tokens.default_ttl_seconds must be at least 60")
	}
	secret := c.Auth.TOTPSecret
	if secret == "" && !c.Auth.DevAllowFixedCode {
		return fmt.Errorf("auth.totp_secret must be set unless auth.dev_allow_fixed_code is true")
	}
	if strings.Contains(secret, "REPLACE_WITH_YOUR_BASE32_SECRET") {
		return fmt.Errorf("auth.totp_secret still contains the example placeholder")
	}
	if secret != "" {
		if err := validateBase32Secret(secret); err != nil {
			return fmt.Errorf("auth.totp_secret must be a valid Base32 secret: %w", err)
		}
	}
	if strings.TrimSpace(c.Auth.Admin.Username) == "" {
		return fmt.Errorf("auth.admin.username must be set")
	}
	adminHash := strings.TrimSpace(c.Auth.Admin.PasswordSHA256)
	if adminHash == "" {
		return fmt.Errorf("auth.admin.password_sha256 must be set")
	}
	if strings.Contains(adminHash, "REPLACE_WITH_ADMIN_PASSWORD_SHA256") || strings.Contains(c.Auth.Admin.Username, "REPLACE_WITH_ADMIN_USERNAME") {
		return fmt.Errorf("auth.admin still contains the example placeholder")
	}
	if len(adminHash) != 64 || strings.Trim(adminHash, "0123456789abcdefABCDEF") != "" {
		return fmt.Errorf("auth.admin.password_sha256 must be a sha256 hex string")
	}
	for _, origin := range c.CORS.AllowOrigins {
		if strings.TrimSpace(origin) == "*" {
			return fmt.Errorf("cors.allow_origins must not contain * when credential cookies are enabled")
		}
	}
	seenDirs := map[string]struct{}{}
	for _, dir := range c.Storage.Dirs {
		if strings.TrimSpace(dir.ID) == "" {
			return fmt.Errorf("storage.dirs contains an empty id")
		}
		if strings.TrimSpace(dir.Path) == "" {
			return fmt.Errorf("storage.dirs[%s].path must not be empty", dir.ID)
		}
		if _, ok := seenDirs[dir.ID]; ok {
			return fmt.Errorf("storage.dirs contains duplicate id %q", dir.ID)
		}
		seenDirs[dir.ID] = struct{}{}
	}
	return nil
}

func normalizeTOTPSecret(secret string) string {
	// 用户常会从验证器复制带空格或小写的 Base32 Secret，这里统一成校验库可接受的格式。
	secret = strings.TrimSpace(secret)
	secret = strings.ReplaceAll(secret, " ", "")
	return strings.ToUpper(secret)
}

func validateBase32Secret(secret string) error {
	if decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret); err == nil {
		return validateSecretLength(decoded)
	}
	padding := strings.Repeat("=", (8-len(secret)%8)%8)
	decoded, err := base32.StdEncoding.DecodeString(secret + padding)
	if err != nil {
		return err
	}
	return validateSecretLength(decoded)
}

func validateSecretLength(decoded []byte) error {
	if len(decoded) < 10 {
		return fmt.Errorf("decoded secret must be at least 10 bytes")
	}
	return nil
}

func normalizeExtensions(values []string) []string {
	// 扩展名统一转小写并补前导点，避免 .JPG / jpg 这类写法绕过上传策略。
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		ext := strings.TrimSpace(strings.ToLower(value))
		if ext == "" {
			continue
		}
		if ext != "*" && !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		if _, ok := seen[ext]; ok {
			continue
		}
		seen[ext] = struct{}{}
		out = append(out, ext)
	}
	return out
}

func intersectExtensions(left, right []string) string {
	seen := map[string]struct{}{}
	for _, ext := range left {
		seen[ext] = struct{}{}
	}
	for _, ext := range right {
		if _, ok := seen[ext]; ok {
			return ext
		}
	}
	return ""
}

func (c *Config) Dir(id string) (Dir, bool) {
	// 目录 ID 是所有文件、令牌和审计操作的权限边界，禁止用前端传入的真实路径查找。
	for _, d := range c.Storage.Dirs {
		if d.ID == id {
			return d, true
		}
	}
	return Dir{}, false
}
