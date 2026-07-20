package config

import (
	"encoding/base32"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filetrans-backend/internal/security"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Database   DatabaseConfig   `yaml:"database"`
	Auth       AuthConfig       `yaml:"auth"`
	Downloads  DownloadsConfig  `yaml:"downloads"`
	Web        WebConfig        `yaml:"web"`
	CORS       CORSConfig       `yaml:"cors"`
	Storage    StorageConfig    `yaml:"storage"`
	FilePicker FilePickerConfig `yaml:"file_picker"`
	Tokens     TokensConfig     `yaml:"tokens"`
	Audit      AuditConfig      `yaml:"audit"`
	Abuse      AbuseConfig      `yaml:"abuse"`
	Chat       ChatConfig       `yaml:"chat"`
}

type ServerConfig struct {
	Host                        string   `yaml:"host"`
	Port                        int      `yaml:"port"`
	KeepaliveIdleTimeoutSeconds int64    `yaml:"keepalive_idle_timeout_seconds"`
	TrustProxyHeaders           bool     `yaml:"trust_proxy_headers"`
	TrustedProxyCIDRs           []string `yaml:"trusted_proxy_cidrs"`
}

type DatabaseConfig struct {
	Path string `yaml:"path"`
}

type AuthConfig struct {
	TOTPSecret            string      `yaml:"totp_secret"`
	DevAllowFixedCode     bool        `yaml:"dev_allow_fixed_code"`
	SessionTTLSeconds     int64       `yaml:"session_ttl_seconds"`
	IdleTimeoutSeconds    int64       `yaml:"idle_timeout_seconds"`
	IdleGraceSeconds      int64       `yaml:"idle_grace_seconds"`
	UploadLeaseTTLSeconds int64       `yaml:"upload_lease_ttl_seconds"`
	CookieSecure          bool        `yaml:"cookie_secure"`
	Admin                 AdminConfig `yaml:"admin"`
}

type DownloadsConfig struct {
	LeaseTTLSeconds          int64 `yaml:"lease_ttl_seconds"`
	LeaseMaxTTLSeconds       int64 `yaml:"lease_max_ttl_seconds"`
	ContentHashMaxMB         int   `yaml:"content_hash_max_mb"`
	MaxConcurrentHashes      int   `yaml:"max_concurrent_hashes"`
	VerifyHashOnEveryRequest bool  `yaml:"verify_hash_on_every_request"`
}

type AdminConfig struct {
	Username       string `yaml:"username"`
	PasswordHash   string `yaml:"password_hash,omitempty"`
	PasswordSHA256 string `yaml:"password_sha256"`
}

type AbuseConfig struct {
	Login    LoginAbuseConfig    `yaml:"login"`
	Creation CreationAbuseConfig `yaml:"creation"`
	Uploads  UploadAbuseConfig   `yaml:"uploads"`
}

type LoginAbuseConfig struct {
	MaxConcurrentAdminVerifications int `yaml:"max_concurrent_admin_verifications"`
	GlobalPerMinute                 int `yaml:"global_per_minute"`
	IPMaxFailures                   int `yaml:"ip_max_failures"`
	WindowSeconds                   int `yaml:"window_seconds"`
	BlockSeconds                    int `yaml:"block_seconds"`
}

type CreationAbuseConfig struct {
	TokenGlobalPerMinute      int `yaml:"token_global_per_minute"`
	TokenPerSessionPerMinute  int `yaml:"token_per_session_per_minute"`
	LeaseGlobalPerMinute      int `yaml:"lease_global_per_minute"`
	LeasePerOwnerPerMinute    int `yaml:"lease_per_owner_per_minute"`
	PublicLeasePerIPPerMinute int `yaml:"public_lease_per_ip_per_minute"`
	MaxActiveTokens           int `yaml:"max_active_tokens"`
	MaxOutstandingLeasesTotal int `yaml:"max_outstanding_leases_total"`
	MaxOutstandingLeasesOwner int `yaml:"max_outstanding_leases_per_owner"`
}

type UploadAbuseConfig struct {
	Global      int `yaml:"global"`
	PerResource int `yaml:"per_resource"`
	PerSession  int `yaml:"per_session"`
	PerToken    int `yaml:"per_token"`
}

type WebConfig struct {
	StaticDir string `yaml:"static_dir"`
}

type CORSConfig struct {
	AllowOrigins []string `yaml:"allow_origins"`
}

type StorageConfig struct {
	UploadMaxMB                         int      `yaml:"upload_max_mb"`
	UploadMaxFileMB                     int      `yaml:"upload_max_file_mb"`
	UploadMaxFiles                      int      `yaml:"upload_max_files"`
	UploadTempRetentionSeconds          int64    `yaml:"upload_temp_retention_seconds"`
	UploadTempCleanupIntervalSeconds    int64    `yaml:"upload_temp_cleanup_interval_seconds"`
	UploadTempCleanupMaxEntries         int      `yaml:"upload_temp_cleanup_max_entries"`
	UploadTempCleanupMaxDurationSeconds int      `yaml:"upload_temp_cleanup_max_duration_seconds"`
	DirectoryListScanLimit              int      `yaml:"directory_list_scan_limit"`
	DirectoryListMaxPageSize            int      `yaml:"directory_list_max_page_size"`
	MinFreeMB                           int      `yaml:"min_free_mb"`
	MinFreePercent                      int      `yaml:"min_free_percent"`
	AllowedExtensions                   []string `yaml:"allowed_extensions"`
	BlockedExtensions                   []string `yaml:"blocked_extensions"`
	Dirs                                []Dir    `yaml:"dirs"`
	Shares                              []Dir    `yaml:"shares,omitempty"`
}

type FilePickerConfig struct {
	Roots          []FilePickerRoot `yaml:"roots"`
	MaxScanEntries int              `yaml:"max_scan_entries"`
	MaxPageSize    int              `yaml:"max_page_size"`
	DenyNames      []string         `yaml:"deny_names"`
	DenyPatterns   []string         `yaml:"deny_patterns"`
}

type FilePickerRoot struct {
	ID               string `yaml:"id" json:"id"`
	Name             string `yaml:"name" json:"name"`
	Path             string `yaml:"path" json:"path"`
	AllowSelectFiles bool   `yaml:"allow_select_files" json:"allowSelectFiles"`
	AllowSelectDirs  bool   `yaml:"allow_select_dirs" json:"allowSelectDirs"`
	ShowHidden       bool   `yaml:"show_hidden" json:"showHidden"`
	FollowSymlinks   bool   `yaml:"follow_symlinks" json:"followSymlinks"`
}

type TokensConfig struct {
	DefaultTTLSeconds int64 `yaml:"default_ttl_seconds"`
	MaxTTLSeconds     int64 `yaml:"max_ttl_seconds"`
	UploadMaxMB       int   `yaml:"upload_max_mb"`
}

type AuditConfig struct {
	Retain                      int `yaml:"retain"`
	UnauthorizedSampleSeconds   int `yaml:"unauthorized_sample_seconds"`
	UnauthorizedGlobalPerMinute int `yaml:"unauthorized_global_per_minute"`
	PruneEveryWrites            int `yaml:"prune_every_writes"`
}

type ChatConfig struct {
	WithdrawWindowSeconds    int `yaml:"withdraw_window_seconds"`
	MaxMessageChars          int `yaml:"max_message_chars"`
	MaxMessageBytes          int `yaml:"max_message_bytes"`
	RetentionDays            int `yaml:"retention_days"`
	MaxMessages              int `yaml:"max_messages"`
	SessionMessagesPerMinute int `yaml:"session_messages_per_minute"`
	IPMessagesPerMinute      int `yaml:"ip_messages_per_minute"`
	GlobalMessagesPerMinute  int `yaml:"global_messages_per_minute"`
	CleanupBatch             int `yaml:"cleanup_batch"`
}

type Dir struct {
	ID            string `yaml:"id" json:"id"`
	Name          string `yaml:"name" json:"name"`
	Type          string `yaml:"type,omitempty" json:"type"`
	Path          string `yaml:"path" json:"path"`
	AllowDownload bool   `yaml:"allow_download" json:"allowDownload"`
	AllowUpload   bool   `yaml:"allow_upload" json:"allowUpload"`
}

type PreparedSave struct {
	path       string
	dir        string
	tempPath   string
	published  bool
	rename     func(string, string) error
	syncParent func(string) error
}

var ErrInvalidConfig = errors.New("invalid config content")

const (
	ResourceDirectory = "directory"
	ResourceFile      = "file"
	maxUploadLimitMB  = 102400
)

func Load(path string) (*Config, error) {
	// 先加载默认值，再让 YAML 覆盖，保证新增配置项对旧配置文件保持兼容。
	c := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败\n配置文件：%s\n原因：%w\n处理建议：请确认配置文件存在，并且当前用户对该文件有读取权限", displayPath(path), err)
	}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("配置文件格式错误\n配置文件：%s\n原因：%w\n处理建议：请检查提示行附近的 YAML 缩进、冒号和列表层级；例如 file_picker 应与 storage 同级，不能缩进到 storage.shares 下面", displayPath(path), err)
	}
	c.normalize()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("配置内容校验失败\n配置文件：%s\n原因：%w\n处理建议：请按上面的原因修改配置；如果是首次运行，可对照 backend/config.example.yaml", displayPath(path), err)
	}
	return c, nil
}

func displayPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

func SaveAtomic(path string, c *Config) error {
	prepared, _, err := PrepareAtomic(path, c)
	if err != nil {
		return err
	}
	defer prepared.Abort()
	_, err = prepared.Commit()
	return err
}

func PrepareAtomic(path string, c *Config) (*PreparedSave, *Config, error) {
	next, err := c.NormalizedClone()
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	b, err := yaml.Marshal(next)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, nil, err
	}
	if old, err := os.ReadFile(path); err == nil {
		// 写回前保留一份备份，便于管理员误操作后手工恢复敏感配置和目录列表。
		if err := writeFileAtomic(path+".bak", old, 0600); err != nil {
			return nil, nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.yaml.tmp")
	if err != nil {
		return nil, nil, err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := tmp.Write(b); err != nil {
		cleanup()
		return nil, nil, err
	}
	if err := tmp.Chmod(0600); err != nil {
		cleanup()
		return nil, nil, err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return nil, nil, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return nil, nil, err
	}
	return &PreparedSave{path: path, dir: dir, tempPath: tmpName, rename: os.Rename, syncParent: syncDir}, next, nil
}

func (p *PreparedSave) Commit() (bool, error) {
	if p == nil {
		return false, fmt.Errorf("prepared config is nil")
	}
	if p.published {
		return true, nil
	}
	if p.tempPath == "" {
		return false, fmt.Errorf("prepared config is not available")
	}
	if err := p.rename(p.tempPath, p.path); err != nil {
		return false, err
	}
	p.published = true
	if err := p.syncParent(p.dir); err != nil {
		return true, err
	}
	return true, nil
}

func (p *PreparedSave) Abort() {
	if p == nil || p.published || p.tempPath == "" {
		return
	}
	_ = os.Remove(p.tempPath)
	p.tempPath = ""
}

func (c *Config) NormalizedClone() (*Config, error) {
	next := c.Clone()
	next.normalize()
	if err := next.validate(); err != nil {
		return nil, err
	}
	return next, nil
}

func writeFileAtomic(path string, content []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.yaml.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func (c *Config) Clone() *Config {
	var out Config
	b, err := yaml.Marshal(c)
	if err != nil || yaml.Unmarshal(b, &out) != nil {
		return Default()
	}
	return &out
}

func Default() *Config {
	c := &Config{}
	c.Server.Host = "0.0.0.0"
	c.Server.Port = 17878
	c.Server.KeepaliveIdleTimeoutSeconds = 120
	c.Database.Path = "./data/filetrans.db"
	c.Auth.SessionTTLSeconds = int64((24 * time.Hour) / time.Second)
	c.Auth.IdleTimeoutSeconds = int64((2 * time.Hour) / time.Second)
	c.Auth.IdleGraceSeconds = 30
	c.Auth.UploadLeaseTTLSeconds = int64((30 * time.Minute) / time.Second)
	c.Downloads.LeaseTTLSeconds = int64((2 * time.Hour) / time.Second)
	c.Downloads.LeaseMaxTTLSeconds = int64((6 * time.Hour) / time.Second)
	c.Downloads.ContentHashMaxMB = 64
	c.Downloads.MaxConcurrentHashes = 2
	c.Storage.UploadMaxMB = 5120
	c.Storage.UploadMaxFileMB = 5120
	c.Storage.UploadMaxFiles = 20
	c.Storage.UploadTempRetentionSeconds = 86400
	c.Storage.UploadTempCleanupIntervalSeconds = 3600
	c.Storage.UploadTempCleanupMaxEntries = 50000
	c.Storage.UploadTempCleanupMaxDurationSeconds = 5
	c.Storage.DirectoryListScanLimit = 5000
	c.Storage.DirectoryListMaxPageSize = 200
	c.FilePicker.MaxScanEntries = 5000
	c.FilePicker.MaxPageSize = 200
	c.Tokens.DefaultTTLSeconds = 3600
	c.Tokens.MaxTTLSeconds = int64((24 * time.Hour) / time.Second)
	c.Tokens.UploadMaxMB = 5120
	c.Audit.Retain = 1000
	c.Audit.UnauthorizedSampleSeconds = 60
	c.Audit.UnauthorizedGlobalPerMinute = 120
	c.Audit.PruneEveryWrites = 100
	c.Abuse.Login.MaxConcurrentAdminVerifications = 2
	c.Abuse.Login.GlobalPerMinute = 120
	c.Abuse.Login.IPMaxFailures = 10
	c.Abuse.Login.WindowSeconds = 180
	c.Abuse.Login.BlockSeconds = 90
	c.Abuse.Creation.TokenGlobalPerMinute = 120
	c.Abuse.Creation.TokenPerSessionPerMinute = 30
	c.Abuse.Creation.LeaseGlobalPerMinute = 600
	c.Abuse.Creation.LeasePerOwnerPerMinute = 60
	c.Abuse.Creation.PublicLeasePerIPPerMinute = 120
	c.Abuse.Creation.MaxActiveTokens = 1000
	c.Abuse.Creation.MaxOutstandingLeasesTotal = 5000
	c.Abuse.Creation.MaxOutstandingLeasesOwner = 64
	c.Abuse.Uploads = UploadAbuseConfig{Global: 16, PerResource: 8, PerSession: 4, PerToken: 2}
	c.Chat = ChatConfig{
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
	c.Storage.MinFreeMB = 1024
	c.Storage.MinFreePercent = 5
	return c
}

func (c *Config) normalize() {
	// 归一化只做安全的默认值补齐和大小写/空白处理，不在这里放宽 validate 的硬约束。
	c.Auth.TOTPSecret = normalizeTOTPSecret(c.Auth.TOTPSecret)
	c.Auth.Admin.Username = strings.TrimSpace(c.Auth.Admin.Username)
	c.Auth.Admin.PasswordSHA256 = strings.TrimSpace(c.Auth.Admin.PasswordSHA256)
	c.Auth.Admin.PasswordHash = strings.TrimSpace(c.Auth.Admin.PasswordHash)
	if c.Server.Host == "" {
		c.Server.Host = "0.0.0.0"
	}
	if c.Server.Port <= 0 {
		c.Server.Port = 17878
	}
	if c.Server.KeepaliveIdleTimeoutSeconds <= 0 {
		c.Server.KeepaliveIdleTimeoutSeconds = 120
	}
	if c.Database.Path == "" {
		c.Database.Path = "./data/filetrans.db"
	}
	if c.Auth.SessionTTLSeconds <= 0 {
		c.Auth.SessionTTLSeconds = int64((24 * time.Hour) / time.Second)
	}
	if c.Auth.IdleTimeoutSeconds <= 0 {
		c.Auth.IdleTimeoutSeconds = int64((2 * time.Hour) / time.Second)
	}
	if c.Auth.IdleGraceSeconds < 0 {
		c.Auth.IdleGraceSeconds = 0
	}
	if c.Auth.UploadLeaseTTLSeconds <= 0 {
		c.Auth.UploadLeaseTTLSeconds = int64((30 * time.Minute) / time.Second)
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
		c.Storage.UploadMaxMB = 5120
	}
	if c.Storage.UploadMaxFileMB <= 0 {
		c.Storage.UploadMaxFileMB = c.Storage.UploadMaxMB
	}
	if c.Storage.UploadMaxFiles <= 0 {
		c.Storage.UploadMaxFiles = 20
	}
	if c.Storage.UploadTempRetentionSeconds <= 0 {
		c.Storage.UploadTempRetentionSeconds = 86400
	}
	if c.Storage.UploadTempCleanupIntervalSeconds <= 0 {
		c.Storage.UploadTempCleanupIntervalSeconds = 3600
	}
	if c.Storage.DirectoryListScanLimit <= 0 {
		c.Storage.DirectoryListScanLimit = 5000
	}
	if c.Storage.DirectoryListMaxPageSize <= 0 {
		c.Storage.DirectoryListMaxPageSize = 200
	}
	c.Storage.AllowedExtensions = normalizeExtensions(c.Storage.AllowedExtensions)
	c.Storage.BlockedExtensions = normalizeExtensions(c.Storage.BlockedExtensions)
	c.Storage.Dirs = normalizeResources(c.Storage.Dirs, ResourceDirectory)
	c.Storage.Shares = normalizeResources(c.Storage.Shares, "")
	c.FilePicker.Roots = normalizeFilePickerRoots(c.FilePicker.Roots)
	if c.FilePicker.MaxPageSize <= 0 {
		c.FilePicker.MaxPageSize = 200
	}
	if c.FilePicker.MaxScanEntries <= 0 {
		c.FilePicker.MaxScanEntries = 5000
	}
	c.FilePicker.DenyNames = normalizeNames(c.FilePicker.DenyNames)
	c.FilePicker.DenyPatterns = normalizeNames(c.FilePicker.DenyPatterns)
	if c.Tokens.DefaultTTLSeconds <= 0 {
		c.Tokens.DefaultTTLSeconds = 3600
	}
	if c.Tokens.MaxTTLSeconds <= 0 {
		c.Tokens.MaxTTLSeconds = int64((24 * time.Hour) / time.Second)
	}
	if c.Tokens.DefaultTTLSeconds > c.Tokens.MaxTTLSeconds {
		c.Tokens.DefaultTTLSeconds = c.Tokens.MaxTTLSeconds
	}
	if c.Audit.Retain <= 0 {
		c.Audit.Retain = 1000
	}
	// Chat defaults are seeded before YAML decoding. Do not re-default zero or
	// negative values here: explicit invalid user input must reach validation
	// instead of being silently replaced.
}

func (c *Config) validate() error {
	if c.Chat.WithdrawWindowSeconds < 1 || c.Chat.WithdrawWindowSeconds > 86400 {
		return fmt.Errorf("chat.withdraw_window_seconds must be between 1 and 86400")
	}
	if c.Chat.MaxMessageChars < 1 || c.Chat.MaxMessageChars > 10000 {
		return fmt.Errorf("chat.max_message_chars must be between 1 and 10000")
	}
	if c.Chat.MaxMessageBytes < 1 || c.Chat.MaxMessageBytes > 65536 {
		return fmt.Errorf("chat.max_message_bytes must be between 1 and 65536")
	}
	if c.Chat.RetentionDays < 1 || c.Chat.RetentionDays > 3650 {
		return fmt.Errorf("chat.retention_days must be between 1 and 3650")
	}
	if c.Chat.MaxMessages < 1 || c.Chat.MaxMessages > 1000000 {
		return fmt.Errorf("chat.max_messages must be between 1 and 1000000")
	}
	for name, limit := range map[string]int{
		"chat.session_messages_per_minute": c.Chat.SessionMessagesPerMinute,
		"chat.ip_messages_per_minute":      c.Chat.IPMessagesPerMinute,
		"chat.global_messages_per_minute":  c.Chat.GlobalMessagesPerMinute,
	} {
		if limit < 1 || limit > 100000 {
			return fmt.Errorf("%s must be between 1 and 100000", name)
		}
	}
	if c.Chat.CleanupBatch < 1 || c.Chat.CleanupBatch > 10000 {
		return fmt.Errorf("chat.cleanup_batch must be between 1 and 10000")
	}
	if c.Audit.UnauthorizedSampleSeconds < 0 || c.Audit.UnauthorizedSampleSeconds > 86400 {
		return fmt.Errorf("audit.unauthorized_sample_seconds must be between 0 and 86400")
	}
	if c.Audit.UnauthorizedGlobalPerMinute < 0 || c.Audit.UnauthorizedGlobalPerMinute > 100000 {
		return fmt.Errorf("audit.unauthorized_global_per_minute must be between 0 and 100000")
	}
	if c.Audit.PruneEveryWrites < 0 || c.Audit.PruneEveryWrites > 100000 {
		return fmt.Errorf("audit.prune_every_writes must be between 0 and 100000")
	}
	// 启动阶段集中拒绝危险配置，避免服务运行后才在请求路径上暴露问题。
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port must be between 1 and 65535")
	}
	if c.Server.KeepaliveIdleTimeoutSeconds < 1 || c.Server.KeepaliveIdleTimeoutSeconds > 86400 {
		return fmt.Errorf("server.keepalive_idle_timeout_seconds must be between 1 and 86400")
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
	if c.Auth.UploadLeaseTTLSeconds < 60 || c.Auth.UploadLeaseTTLSeconds > c.Auth.SessionTTLSeconds {
		return fmt.Errorf("auth.upload_lease_ttl_seconds must be between 60 and auth.session_ttl_seconds")
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
	if c.Downloads.MaxConcurrentHashes < 1 || c.Downloads.MaxConcurrentHashes > 16 {
		return fmt.Errorf("downloads.max_concurrent_hashes must be between 1 and 16")
	}
	if c.Storage.UploadMaxMB < 1 || c.Storage.UploadMaxMB > maxUploadLimitMB {
		return fmt.Errorf("storage.upload_max_mb must be between 1 and %d", maxUploadLimitMB)
	}
	if c.Storage.UploadMaxFileMB < 1 || c.Storage.UploadMaxFileMB > c.Storage.UploadMaxMB {
		return fmt.Errorf("storage.upload_max_file_mb must be between 1 and storage.upload_max_mb")
	}
	if c.Storage.UploadMaxFiles < 1 || c.Storage.UploadMaxFiles > 1000 {
		return fmt.Errorf("storage.upload_max_files must be between 1 and 1000")
	}
	if c.Storage.UploadTempCleanupMaxEntries < 100 || c.Storage.UploadTempCleanupMaxEntries > 1000000 {
		return fmt.Errorf("storage.upload_temp_cleanup_max_entries must be between 100 and 1000000")
	}
	if c.Storage.UploadTempCleanupMaxDurationSeconds < 1 || c.Storage.UploadTempCleanupMaxDurationSeconds > 60 {
		return fmt.Errorf("storage.upload_temp_cleanup_max_duration_seconds must be between 1 and 60")
	}
	if c.Storage.DirectoryListScanLimit < 100 || c.Storage.DirectoryListScanLimit > 100000 {
		return fmt.Errorf("storage.directory_list_scan_limit must be between 100 and 100000")
	}
	if c.Storage.DirectoryListMaxPageSize < 1 || c.Storage.DirectoryListMaxPageSize > 1000 {
		return fmt.Errorf("storage.directory_list_max_page_size must be between 1 and 1000")
	}
	if c.Storage.DirectoryListMaxPageSize > c.Storage.DirectoryListScanLimit {
		return fmt.Errorf("storage.directory_list_max_page_size must not exceed directory_list_scan_limit")
	}
	if c.Tokens.UploadMaxMB < 0 || c.Tokens.UploadMaxMB > maxUploadLimitMB {
		return fmt.Errorf("tokens.upload_max_mb must be between 0 and %d", maxUploadLimitMB)
	}
	if overlap := intersectExtensions(c.Storage.AllowedExtensions, c.Storage.BlockedExtensions); overlap != "" {
		return fmt.Errorf("extension %q cannot appear in both storage.allowed_extensions and storage.blocked_extensions", overlap)
	}
	if err := validateExtensionList("storage.allowed_extensions", c.Storage.AllowedExtensions); err != nil {
		return err
	}
	if err := validateExtensionList("storage.blocked_extensions", c.Storage.BlockedExtensions); err != nil {
		return err
	}
	if c.FilePicker.MaxScanEntries < 100 || c.FilePicker.MaxScanEntries > 100000 {
		return fmt.Errorf("file_picker.max_scan_entries must be between 100 and 100000")
	}
	if c.FilePicker.MaxPageSize < 1 || c.FilePicker.MaxPageSize > 200 {
		return fmt.Errorf("file_picker.max_page_size must be between 1 and 200")
	}
	if c.FilePicker.MaxPageSize > c.FilePicker.MaxScanEntries {
		return fmt.Errorf("file_picker.max_page_size must not exceed max_scan_entries")
	}
	if c.Server.TrustProxyHeaders && len(c.Server.TrustedProxyCIDRs) == 0 {
		return fmt.Errorf("server.trusted_proxy_cidrs must not be empty when trust_proxy_headers is true")
	}
	if !c.Server.TrustProxyHeaders && len(c.Server.TrustedProxyCIDRs) != 0 {
		return fmt.Errorf("server.trusted_proxy_cidrs must be empty when trust_proxy_headers is false")
	}
	if len(c.Server.TrustedProxyCIDRs) > 64 {
		return fmt.Errorf("server.trusted_proxy_cidrs must contain at most 64 entries")
	}
	for _, value := range c.Server.TrustedProxyCIDRs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("server.trusted_proxy_cidrs contains an invalid CIDR")
		}
		if unsafeTrustedProxyPrefix(prefix) {
			return fmt.Errorf("server.trusted_proxy_cidrs must not trust all IPv4 or IPv6 addresses")
		}
	}
	seenPickerRoots := map[string]struct{}{}
	for _, root := range c.FilePicker.Roots {
		if !validResourceID(root.ID) {
			return fmt.Errorf("file_picker root id %q may only contain letters, numbers, underscore and hyphen", root.ID)
		}
		if _, ok := seenPickerRoots[root.ID]; ok {
			return fmt.Errorf("file_picker contains duplicate root id %q", root.ID)
		}
		if strings.TrimSpace(root.Path) == "" {
			return fmt.Errorf("file_picker root %s path must not be empty", root.ID)
		}
		if !root.AllowSelectFiles && !root.AllowSelectDirs {
			return fmt.Errorf("file_picker root %s must allow selecting files or directories", root.ID)
		}
		seenPickerRoots[root.ID] = struct{}{}
	}
	if c.Tokens.DefaultTTLSeconds < 60 {
		return fmt.Errorf("tokens.default_ttl_seconds must be at least 60")
	}
	if c.Tokens.MaxTTLSeconds < c.Tokens.DefaultTTLSeconds {
		return fmt.Errorf("tokens.max_ttl_seconds must be greater than or equal to tokens.default_ttl_seconds")
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
	if len(c.Auth.Admin.Username) > 128 {
		return fmt.Errorf("auth.admin.username must not exceed 128 bytes")
	}
	adminPHC := strings.TrimSpace(c.Auth.Admin.PasswordHash)
	legacyHash := strings.TrimSpace(c.Auth.Admin.PasswordSHA256)
	if adminPHC == "" && legacyHash == "" {
		return fmt.Errorf("auth.admin.password_hash or deprecated password_sha256 must be set")
	}
	if strings.Contains(adminPHC, "REPLACE_WITH_ADMIN_PASSWORD_HASH") || strings.Contains(legacyHash, "REPLACE_WITH_ADMIN_PASSWORD_SHA256") || strings.Contains(c.Auth.Admin.Username, "REPLACE_WITH_ADMIN_USERNAME") {
		return fmt.Errorf("auth.admin still contains the example placeholder")
	}
	if adminPHC != "" {
		if err := security.Validate(adminPHC); err != nil {
			return fmt.Errorf("auth.admin.password_hash must be a valid bounded Argon2id PHC string")
		}
	}
	if legacyHash != "" && (len(legacyHash) != 64 || strings.Trim(legacyHash, "0123456789abcdefABCDEF") != "") {
		return fmt.Errorf("auth.admin.password_sha256 must be a sha256 hex string")
	}
	if c.Abuse.Login.MaxConcurrentAdminVerifications < 0 || c.Abuse.Login.MaxConcurrentAdminVerifications > 8 {
		return fmt.Errorf("abuse.login.max_concurrent_admin_verifications must be between 0 and 8")
	}
	if c.Abuse.Login.GlobalPerMinute < 0 || c.Abuse.Login.GlobalPerMinute > 100000 || c.Abuse.Login.IPMaxFailures < 0 || c.Abuse.Login.IPMaxFailures > 1000 || c.Abuse.Login.WindowSeconds < 0 || c.Abuse.Login.WindowSeconds > 86400 || c.Abuse.Login.BlockSeconds < 0 || c.Abuse.Login.BlockSeconds > 86400 {
		return fmt.Errorf("abuse.login rate limits are outside the supported range")
	}
	creationLimits := []int{c.Abuse.Creation.TokenGlobalPerMinute, c.Abuse.Creation.TokenPerSessionPerMinute, c.Abuse.Creation.LeaseGlobalPerMinute, c.Abuse.Creation.LeasePerOwnerPerMinute, c.Abuse.Creation.PublicLeasePerIPPerMinute}
	for _, limit := range creationLimits {
		if limit < 0 || limit > 100000 {
			return fmt.Errorf("abuse.creation rate limits must be between 0 and 100000")
		}
	}
	if c.Abuse.Creation.MaxActiveTokens < 0 || c.Abuse.Creation.MaxActiveTokens > 1000000 || c.Abuse.Creation.MaxOutstandingLeasesTotal < 0 || c.Abuse.Creation.MaxOutstandingLeasesTotal > 1000000 || c.Abuse.Creation.MaxOutstandingLeasesOwner < 0 || c.Abuse.Creation.MaxOutstandingLeasesOwner > 100000 {
		return fmt.Errorf("abuse.creation outstanding limits are outside the supported range")
	}
	for _, limit := range []int{c.Abuse.Uploads.Global, c.Abuse.Uploads.PerResource, c.Abuse.Uploads.PerSession, c.Abuse.Uploads.PerToken} {
		if limit < 0 || limit > 10000 {
			return fmt.Errorf("abuse.uploads limits must be between 0 and 10000")
		}
	}
	if c.Storage.MinFreeMB < 0 || c.Storage.MinFreeMB > 1000000000 || c.Storage.MinFreePercent < 0 || c.Storage.MinFreePercent > 100 {
		return fmt.Errorf("storage free space reserve is outside the supported range")
	}
	for _, origin := range c.CORS.AllowOrigins {
		if strings.TrimSpace(origin) == "*" {
			return fmt.Errorf("cors.allow_origins must not contain * when credential cookies are enabled")
		}
	}
	seenDirs := map[string]struct{}{}
	for _, dir := range c.Resources() {
		if strings.TrimSpace(dir.ID) == "" {
			return fmt.Errorf("storage resources contains an empty id")
		}
		if !validResourceID(dir.ID) {
			return fmt.Errorf("storage resource id %q may only contain letters, numbers, underscore and hyphen", dir.ID)
		}
		if strings.TrimSpace(dir.Path) == "" {
			return fmt.Errorf("storage resource %s path must not be empty", dir.ID)
		}
		if _, ok := seenDirs[dir.ID]; ok {
			return fmt.Errorf("storage resources contains duplicate id %q", dir.ID)
		}
		if dir.Type != ResourceDirectory && dir.Type != ResourceFile {
			return fmt.Errorf("storage resource %s type must be directory or file", dir.ID)
		}
		if dir.Type == ResourceFile && dir.AllowUpload {
			return fmt.Errorf("storage file resource %s cannot allow upload", dir.ID)
		}
		seenDirs[dir.ID] = struct{}{}
	}
	return nil
}

func unsafeTrustedProxyPrefix(prefix netip.Prefix) bool {
	prefix = prefix.Masked()
	if prefix.Addr().Is4() && prefix.Bits() == 0 {
		return true
	}
	if prefix.Addr().Is6() && !prefix.Addr().Is4In6() && prefix.Bits() == 0 {
		return true
	}
	// An IPv6 prefix at or above the IPv4-mapped /96 boundary can otherwise
	// amount to trusting every IPv4 peer after address unmapping.
	mappedFirst := netip.MustParseAddr("::ffff:0.0.0.0")
	mappedLast := netip.MustParseAddr("::ffff:255.255.255.255")
	return prefix.Addr().Is6() && prefix.Contains(mappedFirst) && prefix.Contains(mappedLast)
}

func normalizeResources(values []Dir, defaultType string) []Dir {
	out := make([]Dir, 0, len(values))
	for _, dir := range values {
		dir.ID = strings.TrimSpace(dir.ID)
		dir.Name = strings.TrimSpace(dir.Name)
		dir.Path = strings.TrimSpace(dir.Path)
		dir.Type = normalizeResourceType(dir.Type, defaultType)
		if dir.Name == "" {
			dir.Name = dir.ID
		}
		if dir.Type == ResourceFile {
			dir.AllowUpload = false
		}
		out = append(out, dir)
	}
	return out
}

func normalizeFilePickerRoots(values []FilePickerRoot) []FilePickerRoot {
	out := make([]FilePickerRoot, 0, len(values))
	for _, root := range values {
		root.ID = strings.TrimSpace(root.ID)
		root.Name = strings.TrimSpace(root.Name)
		root.Path = strings.TrimSpace(root.Path)
		if root.Name == "" {
			root.Name = root.ID
		}
		out = append(out, root)
	}
	return out
}

func normalizeNames(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		name := strings.TrimSpace(strings.ToLower(value))
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

func normalizeResourceType(value, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "dir" || value == "folder" || value == "" && fallback == ResourceDirectory {
		return ResourceDirectory
	}
	if value == "" && fallback != "" {
		return fallback
	}
	if value == ResourceFile {
		return ResourceFile
	}
	return value
}

func validResourceID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
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
		if !strings.HasPrefix(ext, ".") {
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

func validateExtensionList(field string, values []string) error {
	for _, ext := range values {
		if !validUploadExtension(ext) {
			return fmt.Errorf("%s contains invalid extension %q", field, ext)
		}
	}
	return nil
}

func validUploadExtension(ext string) bool {
	if len(ext) < 2 || len(ext) > 33 || ext[0] != '.' {
		return false
	}
	for _, r := range ext[1:] {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '+' {
			continue
		}
		return false
	}
	return true
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
	for _, d := range c.Resources() {
		if d.ID == id {
			return d, true
		}
	}
	return Dir{}, false
}

func (c *Config) Resources() []Dir {
	resources := make([]Dir, 0, len(c.Storage.Dirs)+len(c.Storage.Shares))
	for _, dir := range c.Storage.Dirs {
		if dir.Type == "" {
			dir.Type = ResourceDirectory
		}
		resources = append(resources, dir)
	}
	for _, share := range c.Storage.Shares {
		if share.Type == "" {
			share.Type = ResourceFile
		}
		resources = append(resources, share)
	}
	return resources
}

func (c *Config) SetResources(resources []Dir) {
	dirs := make([]Dir, 0, len(resources))
	shares := make([]Dir, 0, len(resources))
	for _, resource := range normalizeResources(resources, ResourceDirectory) {
		if resource.Type == ResourceFile {
			shares = append(shares, resource)
			continue
		}
		dirs = append(dirs, resource)
	}
	c.Storage.Dirs = dirs
	c.Storage.Shares = shares
}
