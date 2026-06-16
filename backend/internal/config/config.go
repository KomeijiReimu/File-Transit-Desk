package config

import (
	"encoding/base32"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	UploadMaxMB                      int      `yaml:"upload_max_mb"`
	UploadMaxFileMB                  int      `yaml:"upload_max_file_mb"`
	UploadMaxFiles                   int      `yaml:"upload_max_files"`
	UploadTempRetentionSeconds       int64    `yaml:"upload_temp_retention_seconds"`
	UploadTempCleanupIntervalSeconds int64    `yaml:"upload_temp_cleanup_interval_seconds"`
	AllowedExtensions                []string `yaml:"allowed_extensions"`
	BlockedExtensions                []string `yaml:"blocked_extensions"`
	Dirs                             []Dir    `yaml:"dirs"`
	Shares                           []Dir    `yaml:"shares,omitempty"`
}

type FilePickerConfig struct {
	Roots        []FilePickerRoot `yaml:"roots"`
	MaxPageSize  int              `yaml:"max_page_size"`
	DenyNames    []string         `yaml:"deny_names"`
	DenyPatterns []string         `yaml:"deny_patterns"`
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
	Retain int `yaml:"retain"`
}

type Dir struct {
	ID            string `yaml:"id" json:"id"`
	Name          string `yaml:"name" json:"name"`
	Type          string `yaml:"type,omitempty" json:"type"`
	Path          string `yaml:"path" json:"path"`
	AllowDownload bool   `yaml:"allow_download" json:"allowDownload"`
	AllowUpload   bool   `yaml:"allow_upload" json:"allowUpload"`
}

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
	next, err := c.NormalizedClone()
	if err != nil {
		return err
	}
	b, err := yaml.Marshal(next)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil {
		// 写回前保留一份备份，便于管理员误操作后手工恢复敏感配置和目录列表。
		if err := writeFileAtomic(path+".bak", old, 0600); err != nil {
			return err
		}
	}
	return writeFileAtomic(path, b, 0600)
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
	_ = syncDir(dir)
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
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
	c.Database.Path = "./data/filetrans.db"
	c.Auth.SessionTTLSeconds = int64((24 * time.Hour) / time.Second)
	c.Auth.IdleTimeoutSeconds = int64((30 * time.Minute) / time.Second)
	c.Auth.IdleGraceSeconds = 30
	c.Auth.UploadLeaseTTLSeconds = int64((30 * time.Minute) / time.Second)
	c.Downloads.LeaseTTLSeconds = int64((2 * time.Hour) / time.Second)
	c.Downloads.LeaseMaxTTLSeconds = int64((6 * time.Hour) / time.Second)
	c.Downloads.ContentHashMaxMB = 64
	c.Storage.UploadMaxMB = 5120
	c.Storage.UploadMaxFileMB = 5120
	c.Storage.UploadMaxFiles = 20
	c.Storage.UploadTempRetentionSeconds = 86400
	c.Storage.UploadTempCleanupIntervalSeconds = 3600
	c.FilePicker.MaxPageSize = 200
	c.Tokens.DefaultTTLSeconds = 3600
	c.Tokens.MaxTTLSeconds = int64((24 * time.Hour) / time.Second)
	c.Tokens.UploadMaxMB = 5120
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
		c.Server.Port = 17878
	}
	if c.Database.Path == "" {
		c.Database.Path = "./data/filetrans.db"
	}
	if c.Auth.SessionTTLSeconds <= 0 {
		c.Auth.SessionTTLSeconds = int64((24 * time.Hour) / time.Second)
	}
	if c.Auth.IdleTimeoutSeconds <= 0 {
		c.Auth.IdleTimeoutSeconds = int64((30 * time.Minute) / time.Second)
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
	c.Storage.AllowedExtensions = normalizeExtensions(c.Storage.AllowedExtensions)
	c.Storage.BlockedExtensions = normalizeExtensions(c.Storage.BlockedExtensions)
	c.Storage.Dirs = normalizeResources(c.Storage.Dirs, ResourceDirectory)
	c.Storage.Shares = normalizeResources(c.Storage.Shares, "")
	c.FilePicker.Roots = normalizeFilePickerRoots(c.FilePicker.Roots)
	if c.FilePicker.MaxPageSize <= 0 {
		c.FilePicker.MaxPageSize = 200
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
	if c.Storage.UploadMaxMB < 1 || c.Storage.UploadMaxMB > maxUploadLimitMB {
		return fmt.Errorf("storage.upload_max_mb must be between 1 and %d", maxUploadLimitMB)
	}
	if c.Storage.UploadMaxFileMB < 1 || c.Storage.UploadMaxFileMB > c.Storage.UploadMaxMB {
		return fmt.Errorf("storage.upload_max_file_mb must be between 1 and storage.upload_max_mb")
	}
	if c.Storage.UploadMaxFiles < 1 || c.Storage.UploadMaxFiles > 1000 {
		return fmt.Errorf("storage.upload_max_files must be between 1 and 1000")
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
	if c.FilePicker.MaxPageSize < 1 || c.FilePicker.MaxPageSize > 1000 {
		return fmt.Errorf("file_picker.max_page_size must be between 1 and 1000")
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
		if !dir.AllowDownload && !dir.AllowUpload {
			return fmt.Errorf("storage resource %s must allow download or upload", dir.ID)
		}
		seenDirs[dir.ID] = struct{}{}
	}
	return nil
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
			if !dir.AllowDownload {
				dir.AllowDownload = true
			}
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
