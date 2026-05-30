package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"filetrans-backend/internal/config"
	"filetrans-backend/internal/fsutil"
	"filetrans-backend/internal/security"
	"filetrans-backend/internal/store"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

type Server struct {
	configMu      sync.RWMutex
	configWriteMu sync.Mutex
	config        *config.Config
	configPath    string
	store         *store.Store
	loginLimiter  *loginLimiter
}

type fileListResponse struct {
	Dir         string         `json:"dir"`
	Path        string         `json:"path"`
	Entries     []fsutil.Entry `json:"entries"`
	CanUpload   bool           `json:"canUpload"`
	CanDownload bool           `json:"canDownload"`
}

type uploadedFile struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type uploadResponse struct {
	OK       bool           `json:"ok"`
	Uploaded int            `json:"uploaded"`
	Files    []uploadedFile `json:"files"`
}

type tokenRequest struct {
	Type       string     `json:"type"`
	DirID      string     `json:"dirId"`
	DirIDSnake string     `json:"dir_id"`
	Path       string     `json:"path"`
	ExpiresAt  *time.Time `json:"expiresAt"`
	ExpiresOld *time.Time `json:"expires_at"`
	TTLMinutes int64      `json:"ttlMinutes"`
	TTLSeconds int64      `json:"ttl_seconds"`
	MaxUses    int        `json:"maxUses"`
	MaxUsesOld int        `json:"max_uses"`
}

type downloadLeaseRequest struct {
	DirID string `json:"dirId"`
	Path  string `json:"path"`
}

type downloadLeaseResponse struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type tokenDTO struct {
	ID            int64      `json:"id"`
	Type          string     `json:"type"`
	DirID         string     `json:"dirId"`
	Path          string     `json:"path"`
	ExpiresAt     *time.Time `json:"expiresAt"`
	MaxUses       int        `json:"maxUses"`
	Uses          int        `json:"uses"`
	UploadedBytes int64      `json:"uploadedBytes"`
	Revoked       bool       `json:"revoked"`
	Valid         bool       `json:"valid"`
	Reason        string     `json:"reason,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
}

type dirDTO struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Type          string `json:"type"`
	Root          string `json:"root,omitempty"`
	AllowDownload bool   `json:"allowDownload"`
	AllowUpload   bool   `json:"allowUpload"`
	CanDownload   bool   `json:"canDownload"`
	CanUpload     bool   `json:"canUpload"`
}

type safeConfigDTO struct {
	Resources []dirDTO `json:"resources"`
	Storage   struct {
		UploadMaxMB       int      `json:"uploadMaxMB"`
		UploadMaxFileMB   int      `json:"uploadMaxFileMB"`
		UploadMaxFiles    int      `json:"uploadMaxFiles"`
		AllowedExtensions []string `json:"allowedExtensions"`
		BlockedExtensions []string `json:"blockedExtensions"`
	} `json:"storage"`
	Tokens struct {
		DefaultTTLSeconds int64 `json:"defaultTTLSeconds"`
		MaxTTLSeconds     int64 `json:"maxTTLSeconds"`
		UploadMaxMB       int   `json:"uploadMaxMB"`
	} `json:"tokens"`
	Downloads struct {
		LeaseTTLSeconds  int64 `json:"leaseTTLSeconds"`
		ContentHashMaxMB int   `json:"contentHashMaxMB"`
	} `json:"downloads"`
	ConfigWritable bool `json:"configWritable"`
}

type resourceRequest struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Type          string `json:"type"`
	Path          string `json:"path"`
	AllowDownload bool   `json:"allowDownload"`
	AllowUpload   bool   `json:"allowUpload"`
}

type auditDTO struct {
	ID          int64     `json:"id"`
	Action      string    `json:"action"`
	ActionLabel string    `json:"actionLabel"`
	IP          string    `json:"ip"`
	Detail      string    `json:"detail"`
	CreatedAt   time.Time `json:"createdAt"`
}

type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]loginAttempt
}

type loginAttempt struct {
	count      int
	windowFrom time.Time
	blockedTil time.Time
}

const (
	loginLimitWindow      = 3 * time.Minute
	loginBlockFor         = 90 * time.Second
	loginMaxFailures      = 10
	maxUploadNameAttempts = 10000
)

func New(cfg *config.Config, st *store.Store) *fiber.App {
	return NewWithConfigPath(cfg, st, "")
}

func NewWithConfigPath(cfg *config.Config, st *store.Store, configPath string) *fiber.App {
	s := &Server{config: cfg, configPath: configPath, store: st, loginLimiter: newLoginLimiter()}
	// 启动时先做一次轻量清理，避免旧会话、旧令牌和旧票据继续影响新进程。
	_ = st.DeleteExpiredSessions(time.Now())
	_ = st.DeleteExpiredTokens(time.Now())
	_ = st.DeleteExpiredDownloadLeases(time.Now())
	app := fiber.New(fiber.Config{
		BodyLimit:    cfg.Storage.UploadMaxMB * 1024 * 1024,
		ErrorHandler: jsonErrorHandler,
	})
	if len(cfg.CORS.AllowOrigins) > 0 {
		// 接口使用 Cookie 凭据，CORS 必须显式列出允许来源，不能依赖通配符。
		app.Use(cors.New(cors.Config{
			AllowOrigins:     strings.Join(cfg.CORS.AllowOrigins, ","),
			AllowCredentials: true,
		}))
	}
	app.Use(s.csrfOriginGuard)
	s.routes(app)
	s.static(app)
	return app
}

func (s *Server) cfg() *config.Config {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.config
}

func (s *Server) replaceConfig(next *config.Config) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.config = next
}

func jsonErrorHandler(c *fiber.Ctx, err error) error {
	code := fiber.StatusInternalServerError
	message := "服务暂时不可用，请稍后重试。"
	if e, ok := err.(*fiber.Error); ok {
		code = e.Code
		message = e.Message
	}
	// 非业务错误不直接回传底层 err.Error，避免文件路径、SQL 或系统细节出现在客户端。
	return c.Status(code).JSON(fiber.Map{"error": message})
}

func (s *Server) routes(app *fiber.App) {
	// /api 是登录态接口，/t 是公开分享接口；公开下载也走票据，避免 GET 预览直接消耗次数。
	app.Get("/api/health", s.health)
	app.Post("/api/auth/login", s.login)
	app.Post("/api/auth/admin-login", s.adminLogin)
	app.Get("/api/auth/me", s.auth(s.me))
	app.Post("/api/auth/heartbeat", s.auth(s.heartbeat))
	app.Post("/api/auth/logout", s.auth(s.logout))
	app.Get("/api/dirs", s.auth(s.dirs))
	app.Get("/api/files/list", s.auth(s.listFiles))
	app.Get("/api/files/download", s.auth(s.downloadFile))
	app.Post("/api/files/download-lease", s.auth(s.createDownloadLease))
	app.Get("/api/files/download-by-lease", s.downloadByLease)
	app.Post("/api/files/upload", s.auth(s.uploadFiles))
	app.Get("/api/tokens", s.adminOnly(s.listTokens))
	app.Post("/api/tokens", s.adminOnly(s.createToken))
	app.Post("/api/tokens/:id/revoke", s.adminOnly(s.revokeToken))
	app.Delete("/api/tokens/:id", s.adminOnly(s.deleteToken))
	app.Get("/api/audit/logs", s.adminOnly(s.auditLogs))
	app.Get("/api/config", s.adminOnly(s.safeConfig))
	app.Put("/api/config/upload-policy", s.adminOnly(s.updateUploadPolicy))
	app.Get("/api/config/file-picker/roots", s.adminOnly(s.filePickerRoots))
	app.Get("/api/config/file-picker/list", s.adminOnly(s.filePickerList))
	app.Post("/api/config/file-picker/validate", s.adminOnly(s.filePickerValidate))
	app.Post("/api/config/resources", s.adminOnly(s.createResource))
	app.Put("/api/config/resources/:id", s.adminOnly(s.updateResource))
	app.Delete("/api/config/resources/:id", s.adminOnly(s.deleteResource))
	app.Get("/t/:token/info", s.publicTokenInfo)
	app.Get("/t/:token/upload", s.publicUploadPage)
	app.Post("/t/:token/download-lease", s.createPublicDownloadLease)
	app.Get("/t/download-by-lease", s.downloadByLease)
	app.Get("/t/:token/download", s.publicDownload)
	app.Post("/t/:token/upload", s.publicUpload)
}

func (s *Server) csrfOriginGuard(c *fiber.Ctx) error {
	// 只拦截会修改状态的 /api 请求；公开 /t 上传下载由令牌自身约束。
	if !isUnsafeMethod(c.Method()) || !strings.HasPrefix(c.Path(), "/api/") {
		return c.Next()
	}
	origin := strings.TrimSpace(c.Get("Origin"))
	if origin == "" {
		return c.Next()
	}
	if origin == requestOrigin(c) {
		return c.Next()
	}
	for _, allowed := range s.cfg().CORS.AllowOrigins {
		if origin == strings.TrimSpace(allowed) {
			return c.Next()
		}
	}
	_ = s.store.Audit("csrf_denied", s.clientIP(c), origin+" -> "+c.Path())
	return fiber.ErrForbidden
}

func requestOrigin(c *fiber.Ctx) string {
	host := strings.TrimSpace(c.Get("Host"))
	if host == "" {
		host = strings.TrimSpace(c.Hostname())
	}
	if host == "" {
		return ""
	}
	return c.Protocol() + "://" + host
}

func isUnsafeMethod(method string) bool {
	switch method {
	case fiber.MethodPost, fiber.MethodPut, fiber.MethodPatch, fiber.MethodDelete:
		return true
	default:
		return false
	}
}

func (s *Server) static(app *fiber.App) {
	if s.cfg().Web.StaticDir == "" {
		return
	}
	staticDir := s.cfg().Web.StaticDir
	if _, err := os.Stat(staticDir); err != nil {
		return
	}
	app.Static("/", staticDir)
	app.Get("/*", func(c *fiber.Ctx) error {
		// SPA 回退不能吞掉后端接口，否则错误路径会被前端 index.html 掩盖。
		if strings.HasPrefix(c.Path(), "/api") || strings.HasPrefix(c.Path(), "/t/") {
			return fiber.ErrNotFound
		}
		return c.SendFile(filepath.Join(staticDir, "index.html"))
	})
}

func (s *Server) auth(next fiber.Handler) fiber.Handler {
	return func(c *fiber.Ctx) error {
		id := c.Cookies("sid")
		sess, err := s.store.Session(id)
		now := time.Now()
		grace := time.Duration(s.cfg().Auth.IdleGraceSeconds) * time.Second
		idleValid := err == nil && now.Before(sess.IdleExpiresAt)
		withinIdleGrace := err == nil && !idleValid && grace > 0 && now.Before(sess.IdleExpiresAt.Add(grace))
		if withinIdleGrace && c.Path() == "/api/auth/heartbeat" {
			// 只允许心跳在短宽限期内恢复会话，普通业务请求不能借宽限继续访问文件。
			idleValid = true
		}
		if id == "" || err != nil || !now.Before(sess.ExpiresAt) || !idleValid {
			_ = s.store.DeleteExpiredSessionsWithIdleGrace(time.Now(), grace)
			if id != "" && !withinIdleGrace {
				s.clearSessionCookie(c)
			}
			_ = s.store.Audit("unauthorized", s.clientIP(c), c.Path())
			return fiber.ErrUnauthorized
		}
		c.Locals("sessionID", sess.ID)
		c.Locals("sessionExpiresAt", sess.ExpiresAt)
		c.Locals("sessionIdleExpiresAt", sess.IdleExpiresAt)
		c.Locals("role", sess.Role)
		c.Locals("name", sess.Name)
		return next(c)
	}
}

func (s *Server) adminOnly(next fiber.Handler) fiber.Handler {
	return s.auth(func(c *fiber.Ctx) error {
		if c.Locals("role") != "admin" {
			_ = s.store.Audit("forbidden", s.clientIP(c), c.Path())
			return fiber.ErrForbidden
		}
		return next(c)
	})
}

func (s *Server) health(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"ok": true})
}

func (s *Server) login(c *fiber.Ctx) error {
	ip := s.clientIP(c)
	// 登录入口顺手清理过期状态，减少后台定时任务依赖。
	_ = s.store.DeleteExpiredSessions(time.Now())
	_ = s.store.DeleteExpiredTokens(time.Now())
	if !s.loginLimiter.reserve(ip) {
		_ = s.store.Audit("login_rate_limited", ip, "")
		c.Set("Retry-After", strconv.Itoa(int(s.loginLimiter.retryAfter(ip).Seconds())))
		return fiber.NewError(fiber.StatusTooManyRequests, "尝试次数较多，请稍候再试。")
	}
	var in struct {
		Code string `json:"code"`
	}
	if err := c.BodyParser(&in); err != nil {
		return fiber.ErrBadRequest
	}
	in.Code = normalizeLoginCode(in.Code)
	if len(in.Code) != 6 {
		return fiber.NewError(fiber.StatusBadRequest, "请输入 6 位动态验证码。")
	}
	if !s.validateLoginCode(in.Code) {
		_ = s.store.Audit("login_failed", ip, "")
		return fiber.NewError(fiber.StatusUnauthorized, "动态验证码无效，请确认设备时间已同步后重试。")
	}
	s.loginLimiter.reset(ip)
	id, _, err := security.NewToken()
	if err != nil {
		return err
	}
	now := time.Now()
	expiresAt := now.Add(time.Duration(s.cfg().Auth.SessionTTLSeconds) * time.Second)
	idleExpiresAt := s.sessionIdleExpiresAt(now, expiresAt)
	if err := s.store.CreateSessionWithIdle(id, expiresAt, idleExpiresAt, "user", ""); err != nil {
		return err
	}
	s.setSessionCookie(c, id, expiresAt)
	_ = s.store.Audit("login_success", ip, "")
	return c.JSON(fiber.Map{"authenticated": true, "role": "user", "expiresAt": expiresAt, "idleExpiresAt": idleExpiresAt})
}

func (s *Server) adminLogin(c *fiber.Ctx) error {
	ip := s.clientIP(c)
	_ = s.store.DeleteExpiredSessions(time.Now())
	_ = s.store.DeleteExpiredTokens(time.Now())
	if !s.loginLimiter.reserve(ip) {
		_ = s.store.Audit("login_rate_limited", ip, "管理员登录")
		c.Set("Retry-After", strconv.Itoa(int(s.loginLimiter.retryAfter(ip).Seconds())))
		return fiber.NewError(fiber.StatusTooManyRequests, "尝试次数较多，请稍候再试。")
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.BodyParser(&in); err != nil {
		return fiber.ErrBadRequest
	}
	if !s.validateAdminLogin(in.Username, in.Password) {
		_ = s.store.Audit("login_failed", ip, "管理员登录")
		return fiber.ErrUnauthorized
	}
	s.loginLimiter.reset(ip)
	id, _, err := security.NewToken()
	if err != nil {
		return err
	}
	now := time.Now()
	expiresAt := now.Add(time.Duration(s.cfg().Auth.SessionTTLSeconds) * time.Second)
	idleExpiresAt := s.sessionIdleExpiresAt(now, expiresAt)
	if err := s.store.CreateSessionWithIdle(id, expiresAt, idleExpiresAt, "admin", in.Username); err != nil {
		return err
	}
	s.setSessionCookie(c, id, expiresAt)
	_ = s.store.Audit("login_success", ip, "管理员登录")
	return c.JSON(fiber.Map{"authenticated": true, "role": "admin", "name": in.Username, "expiresAt": expiresAt, "idleExpiresAt": idleExpiresAt})
}

func (s *Server) validateAdminLogin(username, password string) bool {
	// 用户名和密码哈希都使用常量时间比较，降低可观测时序差异。
	if subtle.ConstantTimeCompare([]byte(username), []byte(s.cfg().Auth.Admin.Username)) != 1 {
		return false
	}
	sum := sha256.Sum256([]byte(password))
	expected, err := hex.DecodeString(s.cfg().Auth.Admin.PasswordSHA256)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(sum[:], expected) == 1
}

func (s *Server) validateLoginCode(code string) bool {
	code = normalizeLoginCode(code)
	if s.cfg().Auth.TOTPSecret == "" {
		// 固定验证码只允许显式开发开关开启，生产配置校验会阻止空 Secret。
		return s.cfg().Auth.DevAllowFixedCode && code == "000000"
	}
	ok, err := totp.ValidateCustom(code, s.cfg().Auth.TOTPSecret, time.Now(), totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	return err == nil && ok
}

func normalizeLoginCode(code string) string {
	var b strings.Builder
	for _, r := range code {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (s *Server) clientIP(c *fiber.Ctx) string {
	if s.cfg().Server.TrustProxyHeaders {
		// 只有部署在可信代理后才读取这些头，避免直连场景客户端伪造审计 IP。
		if xff := strings.TrimSpace(c.Get("X-Forwarded-For")); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[0]); ip != "" {
				return ip
			}
		}
		if realIP := strings.TrimSpace(c.Get("X-Real-IP")); realIP != "" {
			return realIP
		}
	}
	return c.IP()
}

func (s *Server) setSessionCookie(c *fiber.Ctx, value string, expiresAt time.Time) {
	c.Cookie(&fiber.Cookie{
		Name:     "sid",
		Value:    value,
		Path:     "/",
		HTTPOnly: true,
		Secure:   s.cfg().Auth.CookieSecure,
		SameSite: "Lax",
		Expires:  expiresAt,
	})
}

func (s *Server) clearSessionCookie(c *fiber.Ctx) {
	c.Cookie(&fiber.Cookie{
		Name:     "sid",
		Value:    "",
		Path:     "/",
		HTTPOnly: true,
		Secure:   s.cfg().Auth.CookieSecure,
		SameSite: "Lax",
		Expires:  time.Unix(0, 0),
	})
}

func (s *Server) me(c *fiber.Ctx) error {
	out := fiber.Map{"authenticated": true, "role": c.Locals("role")}
	if expiresAt, ok := c.Locals("sessionExpiresAt").(time.Time); ok {
		out["expiresAt"] = expiresAt
	}
	if idleExpiresAt, ok := c.Locals("sessionIdleExpiresAt").(time.Time); ok {
		out["idleExpiresAt"] = idleExpiresAt
	}
	if name, ok := c.Locals("name").(string); ok && name != "" {
		out["name"] = name
	}
	return c.JSON(out)
}

func (s *Server) heartbeat(c *fiber.Ctx) error {
	now := time.Now()
	expiresAt, ok := c.Locals("sessionExpiresAt").(time.Time)
	if !ok {
		return fiber.ErrUnauthorized
	}
	idleExpiresAt := s.sessionIdleExpiresAt(now, expiresAt)
	// 心跳只在前端确认用户活跃时发送，用来延长空闲登录态；已开始的下载不依赖这个状态。
	if err := s.store.TouchSession(c.Cookies("sid"), now, idleExpiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fiber.ErrUnauthorized
		}
		return err
	}
	return c.JSON(fiber.Map{"ok": true, "idleExpiresAt": idleExpiresAt})
}

func (s *Server) logout(c *fiber.Ctx) error {
	if err := s.store.DeleteSession(c.Cookies("sid")); err != nil {
		return err
	}
	s.clearSessionCookie(c)
	_ = s.store.Audit("logout", s.clientIP(c), "退出登录")
	return c.JSON(fiber.Map{"ok": true})
}

func (s *Server) sessionIdleExpiresAt(now, absoluteExpiresAt time.Time) time.Time {
	idleExpiresAt := now.Add(time.Duration(s.cfg().Auth.IdleTimeoutSeconds) * time.Second)
	if idleExpiresAt.After(absoluteExpiresAt) {
		return absoluteExpiresAt
	}
	return idleExpiresAt
}

func (s *Server) dirs(c *fiber.Ctx) error {
	_ = s.store.Audit("dirs", s.clientIP(c), "查看目录配置")
	includeRoot := c.Locals("role") == "admin"
	resources := s.cfg().Resources()
	out := make([]dirDTO, 0, len(resources))
	for _, dir := range resources {
		out = append(out, dirToDTO(dir, includeRoot))
	}
	return c.JSON(out)
}

func dirToDTO(dir config.Dir, includeRoot bool) dirDTO {
	item := dirDTO{
		ID:            dir.ID,
		Name:          dir.Name,
		Type:          dir.Type,
		AllowDownload: dir.AllowDownload,
		AllowUpload:   dir.AllowUpload,
		CanDownload:   dir.AllowDownload,
		CanUpload:     dir.AllowUpload,
	}
	if includeRoot {
		// 真实根路径只给管理员看，普通用户只拿到逻辑资源 ID 和权限标记。
		item.Root = dir.Path
	}
	return item
}

func (s *Server) safeConfig(c *fiber.Ctx) error {
	cfg := s.cfg()
	out := safeConfigDTO{Resources: make([]dirDTO, 0, len(cfg.Resources())), ConfigWritable: s.configPath != ""}
	for _, dir := range cfg.Resources() {
		out.Resources = append(out.Resources, dirToDTO(dir, true))
	}
	out.Storage.UploadMaxMB = cfg.Storage.UploadMaxMB
	out.Storage.UploadMaxFileMB = cfg.Storage.UploadMaxFileMB
	out.Storage.UploadMaxFiles = cfg.Storage.UploadMaxFiles
	out.Storage.AllowedExtensions = append([]string{}, cfg.Storage.AllowedExtensions...)
	out.Storage.BlockedExtensions = append([]string{}, cfg.Storage.BlockedExtensions...)
	out.Tokens.DefaultTTLSeconds = cfg.Tokens.DefaultTTLSeconds
	out.Tokens.MaxTTLSeconds = cfg.Tokens.MaxTTLSeconds
	out.Tokens.UploadMaxMB = cfg.Tokens.UploadMaxMB
	out.Downloads.LeaseTTLSeconds = cfg.Downloads.LeaseTTLSeconds
	out.Downloads.ContentHashMaxMB = cfg.Downloads.ContentHashMaxMB
	_ = s.store.Audit("config_view", s.clientIP(c), "查看可视化配置")
	return c.JSON(out)
}

func (s *Server) createResource(c *fiber.Ctx) error {
	var in resourceRequest
	if err := c.BodyParser(&in); err != nil {
		return fiber.ErrBadRequest
	}
	resource, err := resourceFromRequest(in)
	if err != nil {
		return err
	}
	if err := validateResourcePath(resource); err != nil {
		return err
	}
	if err := s.updateConfigResources(func(resources []config.Dir) ([]config.Dir, error) {
		for _, existing := range resources {
			if existing.ID == resource.ID {
				return nil, fiber.NewError(fiber.StatusConflict, "资源 ID 已存在。")
			}
		}
		return append(resources, resource), nil
	}); err != nil {
		return err
	}
	_ = s.store.Audit("config_resource_create", s.clientIP(c), fmt.Sprintf("新增%s资源 %s", resourceTypeLabel(resource.Type), resource.ID))
	return c.JSON(dirToDTO(resource, true))
}

func (s *Server) updateResource(c *fiber.Ctx) error {
	var in resourceRequest
	if err := c.BodyParser(&in); err != nil {
		return fiber.ErrBadRequest
	}
	in.ID = c.Params("id")
	resource, err := resourceFromRequest(in)
	if err != nil {
		return err
	}
	if err := validateResourcePath(resource); err != nil {
		return err
	}
	if err := s.updateConfigResources(func(resources []config.Dir) ([]config.Dir, error) {
		for i, existing := range resources {
			if existing.ID == resource.ID {
				resources[i] = resource
				return resources, nil
			}
		}
		return nil, fiber.ErrNotFound
	}); err != nil {
		return err
	}
	_ = s.store.Audit("config_resource_update", s.clientIP(c), fmt.Sprintf("修改%s资源 %s", resourceTypeLabel(resource.Type), resource.ID))
	return c.JSON(dirToDTO(resource, true))
}

func (s *Server) deleteResource(c *fiber.Ctx) error {
	id := strings.TrimSpace(c.Params("id"))
	if id == "" {
		return fiber.ErrBadRequest
	}
	if err := s.updateConfigResources(func(resources []config.Dir) ([]config.Dir, error) {
		out := make([]config.Dir, 0, len(resources))
		found := false
		for _, existing := range resources {
			if existing.ID == id {
				found = true
				continue
			}
			out = append(out, existing)
		}
		if !found {
			return nil, fiber.ErrNotFound
		}
		return out, nil
	}); err != nil {
		return err
	}
	_ = s.store.Audit("config_resource_delete", s.clientIP(c), "删除资源 "+id)
	return c.JSON(fiber.Map{"ok": true})
}

func (s *Server) updateUploadPolicy(c *fiber.Ctx) error {
	var in uploadPolicyRequest
	if err := c.BodyParser(&in); err != nil {
		return fiber.ErrBadRequest
	}
	if err := s.updateConfig(func(next *config.Config) error {
		next.Storage.AllowedExtensions = append([]string{}, in.AllowedExtensions...)
		next.Storage.BlockedExtensions = append([]string{}, in.BlockedExtensions...)
		return nil
	}); err != nil {
		return err
	}
	saved := s.cfg()
	_ = s.store.Audit("config_upload_policy_update", s.clientIP(c), fmt.Sprintf("允许 %d 项，阻断 %d 项", len(saved.Storage.AllowedExtensions), len(saved.Storage.BlockedExtensions)))
	return c.JSON(uploadPolicyResponse{AllowedExtensions: saved.Storage.AllowedExtensions, BlockedExtensions: saved.Storage.BlockedExtensions})
}

func (s *Server) updateConfigResources(mutator func([]config.Dir) ([]config.Dir, error)) error {
	var oldResources []config.Dir
	var nextResources []config.Dir
	if err := s.updateConfig(func(next *config.Config) error {
		oldResources = next.Resources()
		resources, err := mutator(next.Resources())
		if err != nil {
			return err
		}
		next.SetResources(resources)
		nextResources = next.Resources()
		return nil
	}); err != nil {
		return err
	}
	changedIDs := changedResourceIDs(oldResources, nextResources)
	if len(changedIDs) > 0 {
		// 资源根路径、类型或权限变化后，旧令牌不能自动指向新位置；统一撤销相关令牌并清理下载票据。
		if err := s.store.RevokeTokensByDirIDsAndLeases(changedIDs); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) updateConfig(mutator func(*config.Config) error) error {
	if s.configPath == "" {
		return fiber.NewError(fiber.StatusServiceUnavailable, "当前服务未记录配置文件路径，不能在线写回配置。")
	}
	s.configWriteMu.Lock()
	defer s.configWriteMu.Unlock()
	next := s.cfg().Clone()
	if err := mutator(next); err != nil {
		return err
	}
	normalized, err := next.NormalizedClone()
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "配置写入失败："+err.Error())
	}
	if err := config.SaveAtomic(s.configPath, normalized); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "配置写入失败："+err.Error())
	}
	// 写回成功后再替换内存配置，新请求立即看到新的策略、选择器根和共享资源。
	s.replaceConfig(normalized)
	return nil
}

func changedResourceIDs(oldResources, newResources []config.Dir) []string {
	newByID := map[string]config.Dir{}
	for _, resource := range newResources {
		newByID[resource.ID] = resource
	}
	changed := make([]string, 0)
	for _, old := range oldResources {
		current, ok := newByID[old.ID]
		if !ok || !sameResourceAuthorization(old, current) {
			changed = append(changed, old.ID)
		}
	}
	return changed
}

func sameResourceAuthorization(left, right config.Dir) bool {
	return left.ID == right.ID && left.Type == right.Type && left.Path == right.Path && left.AllowDownload == right.AllowDownload && left.AllowUpload == right.AllowUpload
}

func resourceFromRequest(in resourceRequest) (config.Dir, error) {
	resource := config.Dir{
		ID:            strings.TrimSpace(in.ID),
		Name:          strings.TrimSpace(in.Name),
		Type:          strings.ToLower(strings.TrimSpace(in.Type)),
		Path:          strings.TrimSpace(in.Path),
		AllowDownload: in.AllowDownload,
		AllowUpload:   in.AllowUpload,
	}
	if resource.Type == "" || resource.Type == "dir" || resource.Type == "folder" {
		resource.Type = config.ResourceDirectory
	}
	if resource.Type == config.ResourceFile {
		resource.AllowDownload = true
		resource.AllowUpload = false
	}
	if resource.Name == "" {
		resource.Name = resource.ID
	}
	if resource.ID == "" || resource.Path == "" {
		return resource, fiber.NewError(fiber.StatusBadRequest, "资源 ID 和路径不能为空。")
	}
	if !validAPIResourceID(resource.ID) {
		return resource, fiber.NewError(fiber.StatusBadRequest, "资源 ID 只能包含字母、数字、短横线和下划线。")
	}
	if resource.Type != config.ResourceDirectory && resource.Type != config.ResourceFile {
		return resource, fiber.NewError(fiber.StatusBadRequest, "资源类型只能是目录或单文件。")
	}
	if !resource.AllowDownload && !resource.AllowUpload {
		return resource, fiber.NewError(fiber.StatusBadRequest, "至少需要允许下载或上传其中一项。")
	}
	return resource, nil
}

func validAPIResourceID(id string) bool {
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return id != ""
}

func validateResourcePath(resource config.Dir) error {
	realPath, err := resolvedAbsPath(resource.Path)
	if err != nil {
		return friendlyPathError(err, "路径不存在，请先确认服务端路径。")
	}
	if isDangerousRoot(realPath) {
		return fiber.NewError(fiber.StatusBadRequest, "不能把系统根目录或关键系统目录加入共享。")
	}
	info, err := os.Stat(resource.Path)
	if err != nil {
		return friendlyPathError(err, "路径不存在，请先确认服务端路径。")
	}
	if resource.Type == config.ResourceFile {
		if info.IsDir() {
			return fiber.NewError(fiber.StatusBadRequest, "单文件资源必须指向具体文件，不能指向目录。")
		}
		file, err := os.Open(resource.Path)
		if err != nil {
			return friendlyPathError(err, "文件不可读取，请检查服务端权限。")
		}
		_ = file.Close()
		return nil
	}
	if !info.IsDir() {
		return fiber.NewError(fiber.StatusBadRequest, "目录资源必须指向目录，不能指向文件。")
	}
	if resource.AllowDownload {
		dir, err := os.Open(resource.Path)
		if err != nil {
			return friendlyPathError(err, "目录不可读取，请检查服务端权限。")
		}
		_ = dir.Close()
	}
	if resource.AllowUpload {
		test, err := os.CreateTemp(resource.Path, ".write-test-*.tmp")
		if err != nil {
			return friendlyPathError(err, "目录不可写入，请检查服务端权限。")
		}
		name := test.Name()
		_ = test.Close()
		_ = os.Remove(name)
	}
	return nil
}

func isDangerousRoot(path string) bool {
	original := strings.TrimSpace(strings.ToLower(path))
	winOriginal := strings.TrimRight(strings.ReplaceAll(original, "\\", "/"), "/")
	if len(winOriginal) == 2 && winOriginal[1] == ':' || winOriginal == "c:/users" || strings.HasPrefix(winOriginal, "c:/windows") || strings.HasPrefix(winOriginal, "c:/program files") || strings.HasPrefix(winOriginal, "c:/programdata") {
		return true
	}
	clean, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		clean = filepath.Clean(path)
	}
	if clean == string(filepath.Separator) {
		return true
	}
	lower := strings.ToLower(clean)
	winLower := strings.TrimRight(strings.ReplaceAll(lower, "\\", "/"), "/")
	prefixDangerous := []string{"/etc", "/bin", "/sbin", "/proc", "/sys", "/dev", "/run", "/boot", "/root", "/usr", "/lib", "/lib64", `c:\windows`, `c:\program files`, `c:\program files (x86)`, `c:\programdata`}
	for _, value := range prefixDangerous {
		value = filepath.Clean(strings.ToLower(value))
		winValue := strings.TrimRight(strings.ReplaceAll(value, "\\", "/"), "/")
		if lower == value || strings.HasPrefix(lower, value+string(filepath.Separator)) || strings.HasPrefix(lower, value+`\`) || winLower == winValue || strings.HasPrefix(winLower, winValue+"/") {
			return true
		}
	}
	// /home、/var、/mnt 等顶层位置可包含合法业务目录，但直接共享整个顶层目录过宽，先拦截根本身。
	exactDangerous := []string{"/home", "/var", "/opt", "/tmp", "/srv", "/mnt", "/media", `c:\`, `d:\`, `e:\`, `c:\users`}
	for _, value := range exactDangerous {
		value = filepath.Clean(strings.ToLower(value))
		winValue := strings.TrimRight(strings.ReplaceAll(value, "\\", "/"), "/")
		if lower == value || winLower == winValue {
			return true
		}
	}
	for _, part := range strings.FieldsFunc(lower, func(r rune) bool { return r == '/' || r == '\\' }) {
		switch part {
		case ".ssh", ".gnupg", ".kube":
			return true
		}
	}
	return false
}

func resolvedAbsPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// 使用真实路径做危险目录判断，避免相对路径或符号链接绕过配置管理页面的安全护栏。
	return filepath.EvalSymlinks(abs)
}

func resourceTypeLabel(value string) string {
	if value == config.ResourceFile {
		return "单文件"
	}
	return "目录"
}

func (s *Server) listFiles(c *fiber.Ctx) error {
	dir, err := s.dirFromQuery(c)
	if err != nil {
		return err
	}
	if isFileResource(dir) {
		if err := validateFileResourceListPath(dir, c.Query("path")); err != nil {
			_ = s.store.Audit("illegal_access", s.clientIP(c), fmt.Sprintf("单文件资源 %s 路径校验失败", dir.ID))
			return err
		}
		entry, err := fileResourceEntry(dir)
		if err != nil {
			return err
		}
		_ = s.store.Audit("file_list", s.clientIP(c), fmt.Sprintf("单文件资源 %s", dir.ID))
		return c.JSON(fileListResponse{Dir: dir.ID, Path: "", Entries: []fsutil.Entry{entry}, CanUpload: false, CanDownload: dir.AllowDownload})
	}
	entries, err := fsutil.List(dir.Path, c.Query("path"))
	if err != nil {
		_ = s.store.Audit("illegal_access", s.clientIP(c), fmt.Sprintf("目录 %s 列表路径校验失败", dir.ID))
		return friendlyPathError(err, "路径不存在，请检查路径或返回上级目录。")
	}
	_, safePath, _ := fsutil.Resolve(dir.Path, c.Query("path"))
	_ = s.store.Audit("file_list", s.clientIP(c), fmt.Sprintf("目录 %s，路径 %s", dir.ID, displayPath(safePath)))
	return c.JSON(fileListResponse{Dir: dir.ID, Path: safePath, Entries: entries, CanUpload: dir.AllowUpload, CanDownload: dir.AllowDownload})
}

func (s *Server) downloadFile(c *fiber.Ctx) error {
	dir, err := s.dirFromQuery(c)
	if err != nil {
		return err
	}
	if !dir.AllowDownload {
		return fiber.ErrForbidden
	}
	full, safePath, info, err := s.resolveDownloadFile(dir, c.Query("path"))
	if err != nil {
		return err
	}
	_ = info
	_ = s.store.Audit("download", s.clientIP(c), fmt.Sprintf("目录 %s，文件 %s", dir.ID, displayPath(safePath)))
	return c.Download(full)
}

func (s *Server) createDownloadLease(c *fiber.Ctx) error {
	var in downloadLeaseRequest
	if err := c.BodyParser(&in); err != nil {
		return fiber.ErrBadRequest
	}
	dir, err := s.dirByID(in.DirID)
	if err != nil {
		return err
	}
	if !dir.AllowDownload {
		return fiber.ErrForbidden
	}
	// 下载不直接暴露长期会话 URL，而是先把当前文件状态绑定到短期票据。
	full, safePath, info, err := s.resolveDownloadFile(dir, in.Path)
	if err != nil {
		return err
	}
	fileSHA256, err := s.downloadLeaseFileHash(full, info)
	if err != nil {
		return err
	}
	lease, plain, err := s.createLeaseRecord(store.DownloadLease{
		Source:     "session",
		SessionID:  sql.NullString{String: fmt.Sprint(c.Locals("sessionID")), Valid: true},
		Role:       fmt.Sprint(c.Locals("role")),
		DirID:      dir.ID,
		Path:       safePath,
		FileSize:   info.Size(),
		FileMtime:  normalizedFileMtime(info),
		FileSHA256: fileSHA256,
	})
	if err != nil {
		return err
	}
	_ = s.store.Audit("download_lease_create", s.clientIP(c), fmt.Sprintf("目录 %s，文件 %s", dir.ID, displayPath(safePath)))
	return c.JSON(downloadLeaseResponse{URL: s.downloadLeaseURL(plain, false), ExpiresAt: lease.ExpiresAt})
}

func (s *Server) createPublicDownloadLease(c *fiber.Ctx) error {
	lease, plain, err := s.createPublicDownloadLeaseRecord(c)
	if err != nil {
		return err
	}
	return c.JSON(downloadLeaseResponse{URL: s.downloadLeaseURL(plain, true), ExpiresAt: lease.ExpiresAt})
}

func (s *Server) downloadByLease(c *fiber.Ctx) error {
	plain := strings.TrimSpace(c.Query("lease"))
	if plain == "" {
		return fiber.ErrUnauthorized
	}
	hash := security.HashToken(plain)
	lease, err := s.store.DownloadLeaseByHash(hash)
	if err != nil || !time.Now().Before(lease.ExpiresAt) {
		// 票据过期或不存在时统一返回未授权，不暴露是否曾存在。
		_ = s.store.DeleteExpiredDownloadLeases(time.Now())
		return fiber.ErrUnauthorized
	}
	dir, ok := s.cfg().Dir(lease.DirID)
	if !ok || !dir.AllowDownload {
		return fiber.ErrForbidden
	}
	full, _, info, err := s.resolveDownloadFile(dir, lease.Path)
	if err != nil {
		return err
	}
	// 下载票据绑定文件大小、修改时间和可选内容哈希，避免同一路径文件被替换后继续复用旧授权。
	if info.Size() != lease.FileSize || !normalizedFileMtime(info).Equal(lease.FileMtime.UTC()) {
		_ = s.store.Audit("download_lease_file_changed", s.clientIP(c), fmt.Sprintf("目录 %s，文件 %s", lease.DirID, displayPath(lease.Path)))
		return fiber.NewError(fiber.StatusConflict, "文件已变化，请重新获取下载链接。")
	}
	if lease.FileSHA256.Valid && strings.TrimSpace(lease.FileSHA256.String) != "" {
		currentHash, err := fileSHA256Hex(full)
		if err != nil {
			return err
		}
		if currentHash != lease.FileSHA256.String {
			_ = s.store.Audit("download_lease_file_changed", s.clientIP(c), fmt.Sprintf("目录 %s，文件 %s，内容哈希不匹配", lease.DirID, displayPath(lease.Path)))
			return fiber.NewError(fiber.StatusConflict, "文件内容已变化，请重新获取下载链接。")
		}
	}
	if !lease.LastUsedAt.Valid {
		_ = s.store.Audit("download_lease_use", s.clientIP(c), fmt.Sprintf("首次使用%s下载票据，目录 %s，文件 %s", lease.Source, lease.DirID, displayPath(lease.Path)))
	}
	_ = s.store.TouchDownloadLease(hash, time.Now())
	return c.Download(full)
}

func (s *Server) createLeaseRecord(lease store.DownloadLease) (store.DownloadLease, string, error) {
	_ = s.store.DeleteExpiredDownloadLeases(time.Now())
	plain, hash, err := security.NewToken()
	if err != nil {
		return lease, "", err
	}
	now := time.Now()
	// 数据库只保存哈希；plain 只返回给本次响应，用于浏览器跳转下载。
	lease.Hash = hash
	if lease.ExpiresAt.IsZero() {
		lease.ExpiresAt = now.Add(s.downloadLeaseTTL())
	}
	lease.CreatedAt = now
	if err := s.store.CreateDownloadLease(&lease); err != nil {
		return lease, "", err
	}
	return lease, plain, nil
}

func publicLeaseExpiry(now time.Time, ttl time.Duration, tokenExpiresAt sql.NullTime) time.Time {
	// 公开下载票据不应长于公开 token 自身，避免 token 过期后仍留下不可见的下载授权窗口。
	expiresAt := now.Add(ttl)
	if tokenExpiresAt.Valid && tokenExpiresAt.Time.Before(expiresAt) {
		return tokenExpiresAt.Time
	}
	return expiresAt
}

func (s *Server) downloadLeaseTTL() time.Duration {
	ttl := time.Duration(s.cfg().Downloads.LeaseTTLSeconds) * time.Second
	maxTTL := time.Duration(s.cfg().Downloads.LeaseMaxTTLSeconds) * time.Second
	// 二次夹紧可防止旧配置或手工构造的 Config 绕过 config.normalize。
	if maxTTL > 0 && ttl > maxTTL {
		return maxTTL
	}
	return ttl
}

func (s *Server) downloadLeaseURL(plain string, public bool) string {
	path := "/api/files/download-by-lease"
	if public {
		path = "/t/download-by-lease"
	}
	return path + "?lease=" + url.QueryEscape(plain)
}

func (s *Server) downloadLeaseFileHash(full string, info os.FileInfo) (sql.NullString, error) {
	maxBytes := s.downloadLeaseHashMaxBytes()
	if maxBytes > 0 && info.Size() > maxBytes {
		// 大文件默认跳过内容哈希，用大小和 mtime 兜底，避免 Range 续传前反复读完整文件。
		return sql.NullString{String: "", Valid: true}, nil
	}
	// 内容哈希让小文件票据具备内容级绑定；大文件可通过配置选择是否启用，避免 Range 续传反复扫完整文件。
	hash, err := fileSHA256Hex(full)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: hash, Valid: true}, nil
}

func (s *Server) downloadLeaseHashMaxBytes() int64 {
	if s.cfg().Downloads.ContentHashMaxMB <= 0 {
		return 0
	}
	return int64(s.cfg().Downloads.ContentHashMaxMB) * 1024 * 1024
}

func fileSHA256Hex(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Server) resolveDownloadFile(dir config.Dir, rel string) (string, string, os.FileInfo, error) {
	if isFileResource(dir) {
		safeRel, err := fsutil.CleanRel(rel)
		if err != nil {
			return "", "", nil, friendlyPathError(err, "文件路径不存在，请刷新文件列表后重试。")
		}
		name := filepath.Base(dir.Path)
		if safeRel != "" && safeRel != name {
			return "", "", nil, fiber.ErrNotFound
		}
		info, err := os.Stat(dir.Path)
		if err != nil || info.IsDir() {
			return "", "", nil, fiber.ErrNotFound
		}
		return dir.Path, "", info, nil
	}
	full, safePath, err := fsutil.Resolve(dir.Path, rel)
	if err != nil {
		return "", "", nil, friendlyPathError(err, "文件路径不存在，请刷新文件列表后重试。")
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		return "", "", nil, fiber.ErrNotFound
	}
	return full, safePath, info, nil
}

func isFileResource(dir config.Dir) bool {
	return dir.Type == config.ResourceFile
}

func fileResourceEntry(dir config.Dir) (fsutil.Entry, error) {
	info, err := os.Stat(dir.Path)
	if err != nil || info.IsDir() {
		return fsutil.Entry{}, fiber.ErrNotFound
	}
	name := filepath.Base(dir.Path)
	return fsutil.Entry{Name: name, IsDir: false, Size: info.Size(), ModifiedAt: info.ModTime().Format(time.RFC3339), Path: name}, nil
}

func validateFileResourceListPath(dir config.Dir, rel string) error {
	safeRel, err := fsutil.CleanRel(rel)
	if err != nil {
		return friendlyPathError(err, "路径不存在，请检查路径或返回上级目录。")
	}
	if safeRel == "" || safeRel == filepath.Base(dir.Path) {
		return nil
	}
	return fiber.NewError(fiber.StatusNotFound, "单文件资源没有子目录，请返回资源根路径。")
}

func friendlyPathError(err error, missingMessage string) error {
	// 把底层文件系统错误翻译成前端可直接展示的中文提示，同时保留非法路径的 400 语义。
	if errors.Is(err, fsutil.ErrUnsafePath) {
		return fiber.NewError(fiber.StatusBadRequest, "路径不合法，请不要使用绝对路径或 ..。")
	}
	if errors.Is(err, os.ErrNotExist) {
		return fiber.NewError(fiber.StatusNotFound, missingMessage)
	}
	return err
}

func normalizedFileMtime(info os.FileInfo) time.Time {
	return info.ModTime().UTC()
}

func (s *Server) uploadFiles(c *fiber.Ctx) error {
	dirID := firstNonEmpty(c.FormValue("dirId"), c.Query("dirId"))
	dir, err := s.dirByID(dirID)
	if err != nil {
		return err
	}
	files, _, err := s.formFiles(c)
	if err != nil {
		return err
	}
	resp, err := s.saveUploads(c, dir, c.FormValue("path"), files)
	if err != nil {
		return err
	}
	return c.JSON(resp)
}

func (s *Server) saveUploads(c *fiber.Ctx, dir config.Dir, rel string, files []*multipart.FileHeader) (uploadResponse, error) {
	if !dir.AllowUpload {
		return uploadResponse{}, fiber.ErrForbidden
	}
	// 上传路径允许在首次使用时创建资源根目录；普通浏览仍保持只读，不会隐式创建目录。
	if err := os.MkdirAll(dir.Path, 0755); err != nil {
		return uploadResponse{}, friendlyPathError(err, "上传目录不存在或不可访问。")
	}
	targetDir, safeRel, err := fsutil.ResolveForCreate(dir.Path, rel)
	if err != nil {
		_ = s.store.Audit("illegal_access", s.clientIP(c), fmt.Sprintf("目录 %s 上传路径校验失败", dir.ID))
		return uploadResponse{}, fiber.ErrBadRequest
	}
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return uploadResponse{}, err
	}
	if err := fsutil.EnsureInside(dir.Path, targetDir); err != nil {
		_ = s.store.Audit("illegal_access", s.clientIP(c), fmt.Sprintf("目录 %s 上传路径校验失败", dir.ID))
		return uploadResponse{}, friendlyPathError(err, "上传目录不存在或不可访问。")
	}
	// 单个文件逐个落盘，任一失败会清理本请求已保存文件，保证公开令牌回滚时不会留下未计费文件。
	resp := uploadResponse{OK: true, Files: make([]uploadedFile, 0, len(files))}
	saved := make([]string, 0, len(files))
	for _, fh := range files {
		dst, size, err := saveFileUniqueAtomic(targetDir, fh, mbToBytes(s.cfg().Storage.UploadMaxFileMB))
		if err != nil {
			for _, path := range saved {
				_ = os.Remove(path)
			}
			return uploadResponse{}, err
		}
		saved = append(saved, dst)
		name := filepath.Base(dst)
		relPath := filepath.ToSlash(filepath.Join(safeRel, name))
		resp.Files = append(resp.Files, uploadedFile{Name: name, Path: relPath, Size: size})
	}
	resp.Uploaded = len(resp.Files)
	_ = s.store.Audit("upload", s.clientIP(c), fmt.Sprintf("目录 %s，路径 %s，上传 %d 个文件", dir.ID, displayPath(safeRel), resp.Uploaded))
	return resp, nil
}

func saveFileUniqueAtomic(dir string, fh *multipart.FileHeader, maxBytes int64) (string, int64, error) {
	name := fsutil.SafeName(fh.Filename)
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	tmp, err := os.CreateTemp(dir, ".upload-*.tmp")
	if err != nil {
		return "", 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	in, err := fh.Open()
	if err != nil {
		_ = tmp.Close()
		return "", 0, err
	}
	// FileHeader.Size 由解析器填充，但落盘时仍按实际读取字节数二次限流，避免任何解析差异绕过单文件上限。
	written, copyErr := io.Copy(tmp, io.LimitReader(in, maxBytes+1))
	closeInErr := in.Close()
	closeOutErr := tmp.Close()
	if copyErr != nil || closeInErr != nil || closeOutErr != nil {
		if copyErr != nil {
			return "", 0, copyErr
		}
		if closeInErr != nil {
			return "", 0, closeInErr
		}
		return "", 0, closeOutErr
	}
	if written > maxBytes {
		return "", 0, fiber.NewError(fiber.StatusRequestEntityTooLarge, "单个文件超过大小限制。")
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		return "", 0, err
	}

	for i := 0; i < maxUploadNameAttempts; i++ {
		candidateName := name
		if i > 0 {
			// 同名文件使用递增后缀，不覆盖已有文件。
			candidateName = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		dst := filepath.Join(dir, candidateName)
		// 先写入一个同目录临时文件，再把同一个临时文件提交到首个可用文件名，避免并发同名大文件重复写入。
		done, err := commitTempFile(tmpName, dst)
		if err != nil {
			return "", 0, err
		}
		if !done {
			continue
		}
		return dst, written, nil
	}
	return "", 0, fiber.NewError(fiber.StatusConflict, "同名文件过多，请更换文件名后重试。")
}

func commitTempFile(tmpName, dst string) (bool, error) {
	if err := os.Link(tmpName, dst); err == nil {
		return true, nil
	} else if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	// 某些 Windows、网络盘或受限挂载不支持硬链接；降级为 O_EXCL 复制，仍保证不覆盖已有文件。
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	in, err := os.Open(tmpName)
	if err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return false, err
	}
	_, copyErr := io.Copy(out, in)
	closeInErr := in.Close()
	closeOutErr := out.Close()
	if copyErr != nil || closeInErr != nil || closeOutErr != nil {
		_ = os.Remove(dst)
		if copyErr != nil {
			return false, copyErr
		}
		if closeInErr != nil {
			return false, closeInErr
		}
		return false, closeOutErr
	}
	return true, nil
}

func (s *Server) formFiles(c *fiber.Ctx) ([]*multipart.FileHeader, int64, error) {
	form, err := c.MultipartForm()
	if err != nil {
		return nil, 0, fiber.ErrBadRequest
	}
	// 同时兼容旧字段 file 和新字段 files，便于公开页与登录态页面共用后端。
	files := append([]*multipart.FileHeader{}, form.File["file"]...)
	files = append(files, form.File["files"]...)
	if len(files) == 0 {
		return nil, 0, fiber.ErrBadRequest
	}
	cfg := s.cfg()
	if len(files) > cfg.Storage.UploadMaxFiles {
		return nil, 0, fiber.NewError(fiber.StatusRequestEntityTooLarge, fmt.Sprintf("一次最多上传 %d 个文件", cfg.Storage.UploadMaxFiles))
	}
	var total int64
	maxFileBytes := mbToBytes(cfg.Storage.UploadMaxFileMB)
	maxRequestBytes := mbToBytes(cfg.Storage.UploadMaxMB)
	for _, fh := range files {
		safeName := fsutil.SafeName(fh.Filename)
		if fh.Size > maxFileBytes {
			return nil, 0, fiber.NewError(fiber.StatusRequestEntityTooLarge, fmt.Sprintf("单个文件不能超过 %d MB", cfg.Storage.UploadMaxFileMB))
		}
		if !s.extensionAllowed(safeName) {
			return nil, 0, fiber.NewError(fiber.StatusForbidden, "该文件扩展名不允许上传")
		}
		total += fh.Size
	}
	if total > maxRequestBytes {
		return nil, 0, fiber.NewError(fiber.StatusRequestEntityTooLarge, fmt.Sprintf("单次上传总量不能超过 %d MB", cfg.Storage.UploadMaxMB))
	}
	return files, total, nil
}

func (s *Server) extensionAllowed(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	// 黑名单优先级高于白名单，确保危险扩展名不会因白名单误配而放行。
	cfg := s.cfg()
	for _, blocked := range cfg.Storage.BlockedExtensions {
		if ext == blocked {
			return false
		}
	}
	if len(cfg.Storage.AllowedExtensions) == 0 {
		return true
	}
	for _, allowed := range cfg.Storage.AllowedExtensions {
		if ext == allowed {
			return true
		}
	}
	return false
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{attempts: map[string]loginAttempt{}}
}

func (l *loginLimiter) reserve(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.cleanupLocked(now)
	attempt := l.attempts[key]
	if !attempt.blockedTil.IsZero() && now.Before(attempt.blockedTil) {
		return false
	}
	if attempt.windowFrom.IsZero() || now.Sub(attempt.windowFrom) > loginLimitWindow {
		// 旧窗口外的失败次数不再参与限速，避免偶发错误长期影响登录。
		attempt = loginAttempt{windowFrom: now}
	}
	attempt.count++
	if attempt.count > loginMaxFailures {
		attempt.blockedTil = now.Add(loginBlockFor)
		attempt.count = 0
		attempt.windowFrom = now
		l.attempts[key] = attempt
		return false
	}
	l.attempts[key] = attempt
	return true
}

func (l *loginLimiter) retryAfter(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	remaining := time.Until(l.attempts[key].blockedTil)
	if remaining <= 0 {
		return time.Second
	}
	return remaining
}

func (l *loginLimiter) cleanupLocked(now time.Time) {
	for key, attempt := range l.attempts {
		if !attempt.blockedTil.IsZero() && now.Before(attempt.blockedTil) {
			continue
		}
		if now.Sub(attempt.windowFrom) > loginLimitWindow {
			delete(l.attempts, key)
		}
	}
}

func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, key)
}

func (s *Server) listTokens(c *fiber.Ctx) error {
	tokens, err := s.store.Tokens()
	if err != nil {
		return err
	}
	out := make([]tokenDTO, 0, len(tokens))
	now := time.Now()
	uploadMaxBytes := s.tokenUploadMaxBytes()
	for _, t := range tokens {
		dto := tokenToDTO(t, now, uploadMaxBytes)
		if dto.Valid {
			dir, ok := s.cfg().Dir(t.DirID)
			if !ok {
				dto.Valid = false
				dto.Reason = "resource_unavailable"
			} else if t.Type == "download" && !dir.AllowDownload || t.Type == "upload" && !dir.AllowUpload {
				dto.Valid = false
				dto.Reason = "permission_disabled"
			}
		}
		out = append(out, dto)
	}
	return c.JSON(out)
}

func (s *Server) createToken(c *fiber.Ctx) error {
	var in tokenRequest
	if err := c.BodyParser(&in); err != nil {
		return fiber.ErrBadRequest
	}
	dirID := firstNonEmpty(in.DirID, in.DirIDSnake)
	dir, ok := s.cfg().Dir(dirID)
	if !ok {
		return fiber.ErrBadRequest
	}
	if in.Type != "download" && in.Type != "upload" {
		return fiber.ErrBadRequest
	}
	if in.Type == "download" && !dir.AllowDownload {
		return fiber.ErrForbidden
	}
	if in.Type == "upload" && !dir.AllowUpload {
		return fiber.ErrForbidden
	}
	var safePath string
	var fullPath string
	var err error
	if in.Type == "download" && isFileResource(dir) {
		fullPath, safePath, _, err = s.resolveDownloadFile(dir, in.Path)
	} else if in.Type == "download" {
		// 下载令牌必须提前确认具体文件存在，避免对外发出不可用链接。
		fullPath, safePath, err = fsutil.Resolve(dir.Path, in.Path)
	} else {
		// 上传令牌允许未来创建子目录，但必须确保最近存在父目录没有符号链接逃逸。
		fullPath, safePath, err = fsutil.ResolveForCreate(dir.Path, in.Path)
	}
	if err != nil {
		if in.Type == "download" {
			return friendlyPathError(err, "下载文件不存在，请先在文件浏览页确认文件路径。")
		}
		return friendlyPathError(err, "路径不存在，请先在文件浏览页确认后再创建令牌。")
	}
	if in.Type == "download" {
		info, statErr := os.Stat(fullPath)
		if statErr != nil {
			return friendlyPathError(statErr, "下载文件不存在，请先在文件浏览页确认文件路径。")
		}
		if info.IsDir() {
			return fiber.NewError(fiber.StatusBadRequest, "下载令牌需要指向具体文件，不能指向目录。")
		}
	} else if err := ensureUploadTokenTarget(fullPath); err != nil {
		return err
	}
	plain, hash, err := security.NewToken()
	if err != nil {
		return err
	}
	t := &store.Token{Hash: hash, Type: in.Type, DirID: dirID, Path: safePath, MaxUses: maxInt(in.MaxUses, in.MaxUsesOld), ExpiresAt: tokenExpiry(s.cfg(), in)}
	if err := s.store.CreateToken(t); err != nil {
		return err
	}
	base := "/t/" + url.PathEscape(plain)
	publicURL := base + "/download"
	if in.Type == "upload" {
		publicURL = base + "/upload"
	}
	_ = s.store.Audit("token_create", s.clientIP(c), fmt.Sprintf("创建%s令牌 #%d", tokenTypeLabel(t.Type), t.ID))
	return c.JSON(fiber.Map{"id": t.ID, "token": plain, "url": publicURL, "infoUrl": base + "/info"})
}

func ensureUploadTokenTarget(fullPath string) error {
	if fullPath == "" {
		return nil
	}
	current := fullPath
	for {
		info, err := os.Stat(current)
		if err == nil {
			if info.IsDir() {
				return nil
			}
			return fiber.NewError(fiber.StatusBadRequest, "上传令牌需要指向目录，不能指向已存在文件。")
		}
		if !errors.Is(err, os.ErrNotExist) {
			return friendlyPathError(err, "上传目录不可访问，请确认路径。")
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

func tokenExpiry(cfg *config.Config, in tokenRequest) sql.NullTime {
	// 兼容新旧字段优先级：显式过期时间 > ttl_seconds/ttlMinutes > 默认 TTL。
	now := time.Now()
	maxExpiresAt := now.Add(time.Duration(cfg.Tokens.MaxTTLSeconds) * time.Second)
	expiresAt := in.ExpiresAt
	if expiresAt == nil {
		expiresAt = in.ExpiresOld
	}
	if expiresAt != nil {
		if cfg.Tokens.MaxTTLSeconds > 0 && expiresAt.After(maxExpiresAt) {
			return sql.NullTime{Time: maxExpiresAt, Valid: true}
		}
		return sql.NullTime{Time: *expiresAt, Valid: true}
	}
	ttlSeconds := in.TTLSeconds
	if ttlSeconds <= 0 && in.TTLMinutes > 0 {
		ttlSeconds = in.TTLMinutes * 60
	}
	if ttlSeconds <= 0 {
		ttlSeconds = cfg.Tokens.DefaultTTLSeconds
	}
	if cfg.Tokens.MaxTTLSeconds > 0 && ttlSeconds > cfg.Tokens.MaxTTLSeconds {
		ttlSeconds = cfg.Tokens.MaxTTLSeconds
	}
	return sql.NullTime{Time: now.Add(time.Duration(ttlSeconds) * time.Second), Valid: true}
}

func (s *Server) revokeToken(c *fiber.Ctx) error {
	if err := s.store.RevokeTokenAndLeases(c.Params("id")); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fiber.ErrNotFound
		}
		return err
	}
	_ = s.store.Audit("token_revoke", s.clientIP(c), "撤销令牌 #"+c.Params("id"))
	return c.JSON(fiber.Map{"ok": true})
}

func (s *Server) deleteToken(c *fiber.Ctx) error {
	if err := s.store.DeleteTokenAndLeases(c.Params("id")); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fiber.ErrNotFound
		}
		return err
	}
	_ = s.store.Audit("token_delete", s.clientIP(c), "删除令牌 #"+c.Params("id"))
	return c.JSON(fiber.Map{"ok": true})
}

func (s *Server) auditLogs(c *fiber.Ctx) error {
	limit, err := strconv.Atoi(c.Query("limit", "100"))
	if err != nil || limit < 1 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	logs, err := s.store.AuditLogs(limit)
	if err != nil {
		return err
	}
	out := make([]auditDTO, 0, len(logs))
	for _, l := range logs {
		out = append(out, auditDTO{ID: l.ID, Action: l.Action, ActionLabel: actionLabel(l.Action), IP: l.IP, Detail: l.Detail, CreatedAt: l.CreatedAt})
	}
	return c.JSON(out)
}

func (s *Server) publicTokenInfo(c *fiber.Ctx) error {
	t, err := s.store.TokenByHash(security.HashToken(c.Params("token")))
	if err != nil {
		// 公开查询不区分不存在和不可用的内部细节，只返回前端可渲染的失效原因。
		return c.JSON(fiber.Map{"valid": false, "reason": "not_found"})
	}
	valid, reason := tokenValidity(t, time.Now())
	if valid && t.Type == "upload" {
		if maxBytes := s.tokenUploadMaxBytes(); maxBytes > 0 && t.UploadedBytes >= maxBytes {
			valid = false
			reason = "upload_quota_exhausted"
		}
	}
	if valid {
		dir, ok := s.cfg().Dir(t.DirID)
		if !ok {
			valid = false
			reason = "resource_unavailable"
		} else if t.Type == "download" && !dir.AllowDownload || t.Type == "upload" && !dir.AllowUpload {
			valid = false
			reason = "permission_disabled"
		}
	}
	if !valid {
		return c.JSON(fiber.Map{"valid": false, "reason": reason})
	}
	out := fiber.Map{"valid": true, "type": t.Type, "path": t.Path, "expiresAt": nil, "maxUses": t.MaxUses, "uses": t.Uses, "uploadedBytes": t.UploadedBytes, "uploadMaxBytes": s.tokenUploadMaxBytes()}
	if t.ExpiresAt.Valid {
		out["expiresAt"] = t.ExpiresAt.Time
	}
	return c.JSON(out)
}

func (s *Server) publicUploadPage(c *fiber.Ctx) error {
	t, dir, err := s.lookupPublicToken(c, "upload")
	if err != nil {
		return err
	}
	filePath := t.Path
	if filePath == "" {
		filePath = "/"
	}
	c.Type("html", "utf-8")
	return c.SendString(fmt.Sprintf(`<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>临时上传</title>
  <style>
    body{margin:0;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#f6f7fb;color:#1f2937}
    main{max-width:520px;margin:8vh auto;padding:28px;background:#fff;border-radius:20px;box-shadow:0 20px 60px rgba(15,23,42,.12)}
    h1{margin:0 0 10px;font-size:28px} p{color:#64748b;line-height:1.6} input{display:block;width:100%%;margin:18px 0;padding:14px;border:1px solid #d9e2ef;border-radius:12px}
    button{width:100%%;border:0;border-radius:12px;padding:14px 18px;background:#2563eb;color:white;font-weight:700;cursor:pointer}
    small{display:block;margin-top:14px;color:#94a3b8;word-break:break-all}
  </style>
</head>
<body>
<main>
  <h1>临时上传</h1>
  <p>请选择需要上传的文件。文件将上传到目录「%s」的路径「%s」。</p>
  <form method="post" enctype="multipart/form-data">
    <input type="file" name="files" multiple required>
    <button type="submit">开始上传</button>
  </form>
  <small>链接会按创建时设置的有效期和使用次数自动失效。</small>
</main>
</body>
</html>`, htmlEscape(dir.Name), htmlEscape(filePath)))
}

func (s *Server) publicDownload(c *fiber.Ctx) error {
	if _, _, err := s.lookupPublicToken(c, "download"); err != nil {
		return err
	}
	endpoint := "/t/" + url.PathEscape(c.Params("token")) + "/download-lease"
	c.Type("html", "utf-8")
	// 兼容旧后端下载链接：GET 只展示确认页，真正消耗次数的操作放到用户点击后的 POST。
	return c.SendString(fmt.Sprintf(`<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>确认下载</title>
  <style>
    body{margin:0;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#f6f7fb;color:#1f2937}
    main{max-width:520px;margin:8vh auto;padding:28px;background:#fff;border-radius:20px;box-shadow:0 20px 60px rgba(15,23,42,.12)}
    h1{margin:0 0 10px;font-size:28px} p{color:#64748b;line-height:1.6} button{width:100%%;border:0;border-radius:12px;padding:14px 18px;background:#2563eb;color:white;font-weight:700;cursor:pointer}
    small{display:block;margin-top:14px;color:#94a3b8;word-break:break-all}
  </style>
</head>
<body>
<main>
  <h1>确认下载</h1>
  <p>点击按钮后会兑换短期下载票据并开始下载。这样可以避免链接预览或安全扫描提前消耗一次性下载次数。</p>
  <button id="download" type="button">开始下载</button>
  <small id="message">如果你看到此兼容页面，也可以改用前端分享页完成下载。</small>
</main>
<script>
document.getElementById('download').addEventListener('click', async () => {
  const button = document.getElementById('download');
  const message = document.getElementById('message');
  button.disabled = true;
  button.textContent = '准备下载…';
  try {
    const response = await fetch(%s, { method: 'POST' });
    const payload = await response.json();
    if (!response.ok || !payload.url) throw new Error(payload.error || '下载链接创建失败');
    window.location.href = payload.url;
  } catch (err) {
    button.disabled = false;
    button.textContent = '重试下载';
    message.textContent = err instanceof Error ? err.message : '下载链接创建失败';
  }
});
</script>
</body>
</html>`, strconv.Quote(endpoint)))
}

func (s *Server) createPublicDownloadLeaseRecord(c *fiber.Ctx) (store.DownloadLease, string, error) {
	t, dir, err := s.lookupPublicToken(c, "download")
	if err != nil {
		return store.DownloadLease{}, "", err
	}
	full, safePath, info, err := s.resolveDownloadFile(dir, t.Path)
	if err != nil {
		return store.DownloadLease{}, "", err
	}
	fileSHA256, err := s.downloadLeaseFileHash(full, info)
	if err != nil {
		return store.DownloadLease{}, "", err
	}
	reserved, _, err := s.reservePublicToken(c, "download", 0)
	if err != nil {
		return store.DownloadLease{}, "", err
	}
	lease, plain, err := s.createLeaseRecord(store.DownloadLease{
		Source:     "public_token",
		TokenID:    sql.NullInt64{Int64: reserved.ID, Valid: true},
		DirID:      dir.ID,
		Path:       safePath,
		FileSize:   info.Size(),
		FileMtime:  normalizedFileMtime(info),
		FileSHA256: fileSHA256,
		ExpiresAt:  publicLeaseExpiry(time.Now(), s.downloadLeaseTTL(), reserved.ExpiresAt),
	})
	if err != nil {
		_ = s.store.ReleaseTokenUse(reserved.ID, 0)
		return lease, "", err
	}
	// 公开下载令牌在兑换下载票据时消耗一次次数；后续 Range 续传只校验票据，不重复扣次数。
	_ = s.store.Audit("public_download_lease_create", s.clientIP(c), fmt.Sprintf("令牌 #%d，文件 %s", reserved.ID, displayPath(safePath)))
	return lease, plain, nil
}

func (s *Server) publicUpload(c *fiber.Ctx) error {
	// 公开上传先做轻量令牌校验，再解析 multipart，避免无效 token 也能迫使服务端处理大请求体。
	if _, _, err := s.lookupPublicToken(c, "upload"); err != nil {
		return err
	}
	if contentLength := int64(c.Request().Header.ContentLength()); contentLength > 0 && contentLength > mbToBytes(s.cfg().Storage.UploadMaxMB) {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, fmt.Sprintf("单次上传总量不能超过 %d MB", s.cfg().Storage.UploadMaxMB))
	}
	files, totalBytes, err := s.formFiles(c)
	if err != nil {
		return err
	}
	t, dir, err := s.reservePublicToken(c, "upload", totalBytes)
	if err != nil {
		return err
	}
	resp, err := s.saveUploads(c, dir, t.Path, files)
	if err != nil {
		_ = s.store.ReleaseTokenUse(t.ID, totalBytes)
		_ = s.store.Audit("token_upload_failed", s.clientIP(c), fmt.Sprint(t.ID))
		return err
	}
	actualBytes := uploadResponseBytes(resp)
	if err := s.store.AdjustTokenUploadedBytes(t.ID, actualBytes-totalBytes, s.tokenUploadMaxBytes()); err != nil {
		cleanupUploadedResponse(dir, resp)
		_ = s.store.ReleaseTokenUse(t.ID, totalBytes)
		_ = s.store.Audit("token_upload_failed", s.clientIP(c), fmt.Sprint(t.ID))
		if errors.Is(err, store.ErrTokenUploadLimitExceeded) {
			return fiber.NewError(fiber.StatusRequestEntityTooLarge, "upload token quota exceeded")
		}
		return err
	}
	_ = s.store.Audit("token_use", s.clientIP(c), fmt.Sprint(t.ID))
	return c.JSON(resp)
}

func uploadResponseBytes(resp uploadResponse) int64 {
	var total int64
	for _, file := range resp.Files {
		total += file.Size
	}
	return total
}

func cleanupUploadedResponse(dir config.Dir, resp uploadResponse) {
	for _, file := range resp.Files {
		full, _, err := fsutil.Resolve(dir.Path, file.Path)
		if err == nil {
			_ = os.Remove(full)
		}
	}
}

func (s *Server) reservePublicToken(c *fiber.Ctx, tokenType string, uploadBytes int64) (store.Token, config.Dir, error) {
	hash := security.HashToken(c.Params("token"))
	// 先做一次只读校验给出稳定错误，再用条件更新原子预占次数和容量。
	if _, _, err := s.lookupPublicToken(c, tokenType); err != nil {
		return store.Token{}, config.Dir{}, err
	}
	t, err := s.store.ReserveTokenUse(hash, tokenType, time.Now(), uploadBytes, s.tokenUploadMaxBytes())
	if err != nil {
		_ = s.store.Audit("token_denied", s.clientIP(c), "公开令牌不可用或已超出限制")
		if errors.Is(err, store.ErrTokenUploadLimitExceeded) {
			return t, config.Dir{}, fiber.NewError(fiber.StatusRequestEntityTooLarge, "upload token quota exceeded")
		}
		if errors.Is(err, store.ErrTokenNotUsable) {
			return t, config.Dir{}, fiber.ErrForbidden
		}
		return t, config.Dir{}, err
	}
	dir, ok := s.cfg().Dir(t.DirID)
	if !ok {
		return t, dir, fiber.ErrForbidden
	}
	if tokenType == "download" && !dir.AllowDownload {
		return t, dir, fiber.ErrForbidden
	}
	if tokenType == "upload" && !dir.AllowUpload {
		return t, dir, fiber.ErrForbidden
	}
	return t, dir, nil
}

func (s *Server) lookupPublicToken(c *fiber.Ctx, tokenType string) (store.Token, config.Dir, error) {
	t, err := s.store.TokenByHash(security.HashToken(c.Params("token")))
	if err != nil {
		return t, config.Dir{}, fiber.ErrNotFound
	}
	if t.Type != tokenType {
		return t, config.Dir{}, fiber.ErrForbidden
	}
	if ok, _ := tokenValidity(t, time.Now()); !ok {
		return t, config.Dir{}, fiber.ErrForbidden
	}
	if tokenType == "upload" {
		if maxBytes := s.tokenUploadMaxBytes(); maxBytes > 0 && t.UploadedBytes >= maxBytes {
			return t, config.Dir{}, fiber.ErrForbidden
		}
	}
	dir, ok := s.cfg().Dir(t.DirID)
	if !ok {
		return t, dir, fiber.ErrForbidden
	}
	if tokenType == "download" && !dir.AllowDownload {
		return t, dir, fiber.ErrForbidden
	}
	if tokenType == "upload" && !dir.AllowUpload {
		return t, dir, fiber.ErrForbidden
	}
	return t, dir, nil
}

func htmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return replacer.Replace(value)
}

func (s *Server) dirFromQuery(c *fiber.Ctx) (config.Dir, error) {
	return s.dirByID(c.Query("dirId"))
}

func (s *Server) dirByID(dirID string) (config.Dir, error) {
	dir, ok := s.cfg().Dir(dirID)
	if !ok {
		return dir, fiber.ErrNotFound
	}
	return dir, nil
}

func tokenToDTO(t store.Token, now time.Time, uploadMaxBytes int64) tokenDTO {
	var expiresAt *time.Time
	if t.ExpiresAt.Valid {
		expiresAt = &t.ExpiresAt.Time
	}
	valid, reason := tokenValidity(t, now)
	if valid && t.Type == "upload" && uploadMaxBytes > 0 && t.UploadedBytes >= uploadMaxBytes {
		// 上传容量是令牌级限制，和 maxUses 独立显示，前端据 reason 给出更准确文案。
		valid = false
		reason = "upload_quota_exhausted"
	}
	return tokenDTO{ID: t.ID, Type: t.Type, DirID: t.DirID, Path: t.Path, ExpiresAt: expiresAt, MaxUses: t.MaxUses, Uses: t.Uses, UploadedBytes: t.UploadedBytes, Revoked: t.Revoked, Valid: valid, Reason: reason, CreatedAt: t.CreatedAt}
}

func (s *Server) tokenUploadMaxBytes() int64 {
	return mbToBytes(s.cfg().Tokens.UploadMaxMB)
}

func mbToBytes(value int) int64 {
	if value <= 0 {
		return 0
	}
	return int64(value) * 1024 * 1024
}

func tokenValidity(t store.Token, now time.Time) (bool, string) {
	if t.Revoked {
		return false, "revoked"
	}
	if t.ExpiresAt.Valid && !now.Before(t.ExpiresAt.Time) {
		return false, "expired"
	}
	if t.MaxUses > 0 && t.Uses >= t.MaxUses {
		return false, "exhausted"
	}
	return true, ""
}

func actionLabel(action string) string {
	// 后端统一补充中文动作名，前端无需维护另一份审计动作映射。
	labels := map[string]string{
		"file_list":                    "文件列表",
		"dirs":                         "查看目录",
		"download":                     "文件下载",
		"upload":                       "文件上传",
		"login_success":                "登录成功",
		"login_failed":                 "登录失败",
		"login_rate_limited":           "登录限速",
		"logout":                       "退出登录",
		"unauthorized":                 "未认证访问",
		"forbidden":                    "权限不足",
		"illegal_access":               "非法访问",
		"token_create":                 "创建令牌",
		"token_revoke":                 "撤销令牌",
		"token_delete":                 "删除令牌",
		"token_use":                    "使用令牌",
		"token_denied":                 "令牌拒绝",
		"csrf_denied":                  "跨站请求拦截",
		"token_download_failed":        "令牌下载失败",
		"token_upload_failed":          "令牌上传失败",
		"download_lease_create":        "创建下载票据",
		"public_download_lease_create": "创建公开下载票据",
		"download_lease_use":           "使用下载票据",
		"download_lease_file_changed":  "下载票据文件变化",
		"config_view":                  "查看配置",
		"config_resource_create":       "新增共享资源",
		"config_resource_update":       "修改共享资源",
		"config_resource_delete":       "删除共享资源",
		"config_upload_policy_update":  "修改上传策略",
		"file_picker_select":           "选择服务端路径",
		"file_picker_denied":           "文件选择拒绝",
	}
	if label, ok := labels[action]; ok {
		return label
	}
	return action
}

func displayPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return "/"
	}
	return path
}

func tokenTypeLabel(tokenType string) string {
	if tokenType == "upload" {
		return "上传"
	}
	if tokenType == "download" {
		return "下载"
	}
	return tokenType
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func maxInt(values ...int) int {
	max := 0
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	return max
}
