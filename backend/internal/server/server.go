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
	config       *config.Config
	store        *store.Store
	loginLimiter *loginLimiter
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
	Root          string `json:"root,omitempty"`
	AllowDownload bool   `json:"allowDownload"`
	AllowUpload   bool   `json:"allowUpload"`
	CanDownload   bool   `json:"canDownload"`
	CanUpload     bool   `json:"canUpload"`
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
	loginLimitWindow = 3 * time.Minute
	loginBlockFor    = 90 * time.Second
	loginMaxFailures = 10
)

func New(cfg *config.Config, st *store.Store) *fiber.App {
	s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter()}
	_ = st.DeleteExpiredSessions(time.Now())
	_ = st.DeleteExpiredTokens(time.Now())
	_ = st.DeleteExpiredDownloadLeases(time.Now())
	app := fiber.New(fiber.Config{
		BodyLimit:    cfg.Storage.UploadMaxMB * 1024 * 1024,
		ErrorHandler: jsonErrorHandler,
	})
	if len(cfg.CORS.AllowOrigins) > 0 {
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

func jsonErrorHandler(c *fiber.Ctx, err error) error {
	code := fiber.StatusInternalServerError
	if e, ok := err.(*fiber.Error); ok {
		code = e.Code
	}
	return c.Status(code).JSON(fiber.Map{"error": err.Error()})
}

func (s *Server) routes(app *fiber.App) {
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
	app.Get("/t/:token/info", s.publicTokenInfo)
	app.Get("/t/:token/upload", s.publicUploadPage)
	app.Post("/t/:token/download-lease", s.createPublicDownloadLease)
	app.Get("/t/download-by-lease", s.downloadByLease)
	app.Get("/t/:token/download", s.publicDownload)
	app.Post("/t/:token/upload", s.publicUpload)
}

func (s *Server) csrfOriginGuard(c *fiber.Ctx) error {
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
	for _, allowed := range s.config.CORS.AllowOrigins {
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
	if s.config.Web.StaticDir == "" {
		return
	}
	if _, err := os.Stat(s.config.Web.StaticDir); err != nil {
		return
	}
	app.Static("/", s.config.Web.StaticDir)
	app.Get("/*", func(c *fiber.Ctx) error {
		if strings.HasPrefix(c.Path(), "/api") || strings.HasPrefix(c.Path(), "/t/") {
			return fiber.ErrNotFound
		}
		return c.SendFile(filepath.Join(s.config.Web.StaticDir, "index.html"))
	})
}

func (s *Server) auth(next fiber.Handler) fiber.Handler {
	return func(c *fiber.Ctx) error {
		id := c.Cookies("sid")
		sess, err := s.store.Session(id)
		now := time.Now()
		grace := time.Duration(s.config.Auth.IdleGraceSeconds) * time.Second
		idleValid := err == nil && now.Before(sess.IdleExpiresAt)
		withinIdleGrace := err == nil && !idleValid && grace > 0 && now.Before(sess.IdleExpiresAt.Add(grace))
		if withinIdleGrace && c.Path() == "/api/auth/heartbeat" {
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
	_ = s.store.DeleteExpiredSessions(time.Now())
	_ = s.store.DeleteExpiredTokens(time.Now())
	if !s.loginLimiter.allow(ip) {
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
		s.loginLimiter.recordFailure(ip)
		_ = s.store.Audit("login_failed", ip, "")
		return fiber.NewError(fiber.StatusUnauthorized, "动态验证码无效，请确认设备时间已同步后重试。")
	}
	s.loginLimiter.reset(ip)
	id, _, err := security.NewToken()
	if err != nil {
		return err
	}
	now := time.Now()
	expiresAt := now.Add(time.Duration(s.config.Auth.SessionTTLSeconds) * time.Second)
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
	if !s.loginLimiter.allow(ip) {
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
		s.loginLimiter.recordFailure(ip)
		_ = s.store.Audit("login_failed", ip, "管理员登录")
		return fiber.ErrUnauthorized
	}
	s.loginLimiter.reset(ip)
	id, _, err := security.NewToken()
	if err != nil {
		return err
	}
	now := time.Now()
	expiresAt := now.Add(time.Duration(s.config.Auth.SessionTTLSeconds) * time.Second)
	idleExpiresAt := s.sessionIdleExpiresAt(now, expiresAt)
	if err := s.store.CreateSessionWithIdle(id, expiresAt, idleExpiresAt, "admin", in.Username); err != nil {
		return err
	}
	s.setSessionCookie(c, id, expiresAt)
	_ = s.store.Audit("login_success", ip, "管理员登录")
	return c.JSON(fiber.Map{"authenticated": true, "role": "admin", "name": in.Username, "expiresAt": expiresAt, "idleExpiresAt": idleExpiresAt})
}

func (s *Server) validateAdminLogin(username, password string) bool {
	if subtle.ConstantTimeCompare([]byte(username), []byte(s.config.Auth.Admin.Username)) != 1 {
		return false
	}
	sum := sha256.Sum256([]byte(password))
	expected, err := hex.DecodeString(s.config.Auth.Admin.PasswordSHA256)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(sum[:], expected) == 1
}

func (s *Server) validateLoginCode(code string) bool {
	code = normalizeLoginCode(code)
	if s.config.Auth.TOTPSecret == "" {
		return s.config.Auth.DevAllowFixedCode && code == "000000"
	}
	ok, err := totp.ValidateCustom(code, s.config.Auth.TOTPSecret, time.Now(), totp.ValidateOpts{
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
	if s.config.Server.TrustProxyHeaders {
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
		Secure:   s.config.Auth.CookieSecure,
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
		Secure:   s.config.Auth.CookieSecure,
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
	idleExpiresAt := now.Add(time.Duration(s.config.Auth.IdleTimeoutSeconds) * time.Second)
	if idleExpiresAt.After(absoluteExpiresAt) {
		return absoluteExpiresAt
	}
	return idleExpiresAt
}

func (s *Server) dirs(c *fiber.Ctx) error {
	_ = s.store.Audit("dirs", s.clientIP(c), "查看目录配置")
	includeRoot := c.Locals("role") == "admin"
	out := make([]dirDTO, 0, len(s.config.Storage.Dirs))
	for _, dir := range s.config.Storage.Dirs {
		item := dirDTO{
			ID:            dir.ID,
			Name:          dir.Name,
			AllowDownload: dir.AllowDownload,
			AllowUpload:   dir.AllowUpload,
			CanDownload:   dir.AllowDownload,
			CanUpload:     dir.AllowUpload,
		}
		if includeRoot {
			item.Root = dir.Path
		}
		out = append(out, item)
	}
	return c.JSON(out)
}

func (s *Server) listFiles(c *fiber.Ctx) error {
	dir, err := s.dirFromQuery(c)
	if err != nil {
		return err
	}
	entries, err := fsutil.List(dir.Path, c.Query("path"))
	if err != nil {
		_ = s.store.Audit("illegal_access", s.clientIP(c), err.Error())
		return fiber.ErrBadRequest
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
	full, safePath, err := fsutil.Resolve(dir.Path, c.Query("path"))
	if err != nil {
		_ = s.store.Audit("illegal_access", s.clientIP(c), err.Error())
		return fiber.ErrBadRequest
	}
	if info, err := os.Stat(full); err != nil || info.IsDir() {
		return fiber.ErrNotFound
	}
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
		_ = s.store.DeleteExpiredDownloadLeases(time.Now())
		return fiber.ErrUnauthorized
	}
	dir, ok := s.config.Dir(lease.DirID)
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
	lease.Hash = hash
	lease.ExpiresAt = now.Add(s.downloadLeaseTTL())
	lease.CreatedAt = now
	if err := s.store.CreateDownloadLease(&lease); err != nil {
		return lease, "", err
	}
	return lease, plain, nil
}

func (s *Server) downloadLeaseTTL() time.Duration {
	ttl := time.Duration(s.config.Downloads.LeaseTTLSeconds) * time.Second
	maxTTL := time.Duration(s.config.Downloads.LeaseMaxTTLSeconds) * time.Second
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
	if s.config.Downloads.ContentHashMaxMB <= 0 {
		return 0
	}
	return int64(s.config.Downloads.ContentHashMaxMB) * 1024 * 1024
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
	full, safePath, err := fsutil.Resolve(dir.Path, rel)
	if err != nil {
		return "", "", nil, fiber.ErrBadRequest
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		return "", "", nil, fiber.ErrNotFound
	}
	return full, safePath, info, nil
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
	targetDir, safeRel, err := fsutil.ResolveForCreate(dir.Path, rel)
	if err != nil {
		_ = s.store.Audit("illegal_access", s.clientIP(c), err.Error())
		return uploadResponse{}, fiber.ErrBadRequest
	}
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return uploadResponse{}, err
	}
	resp := uploadResponse{OK: true, Files: make([]uploadedFile, 0, len(files))}
	for _, fh := range files {
		dst, err := saveFileUniqueAtomic(targetDir, fh)
		if err != nil {
			return uploadResponse{}, err
		}
		name := filepath.Base(dst)
		relPath := filepath.ToSlash(filepath.Join(safeRel, name))
		resp.Files = append(resp.Files, uploadedFile{Name: name, Path: relPath, Size: fh.Size})
	}
	resp.Uploaded = len(resp.Files)
	_ = s.store.Audit("upload", s.clientIP(c), fmt.Sprintf("目录 %s，路径 %s，上传 %d 个文件", dir.ID, displayPath(safeRel), resp.Uploaded))
	return resp, nil
}

func saveFileUniqueAtomic(dir string, fh *multipart.FileHeader) (string, error) {
	name := fsutil.SafeName(fh.Filename)
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; ; i++ {
		candidateName := name
		if i > 0 {
			candidateName = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		dst := filepath.Join(dir, candidateName)
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}

		in, err := fh.Open()
		if err != nil {
			_ = out.Close()
			_ = os.Remove(dst)
			return "", err
		}
		_, copyErr := io.Copy(out, in)
		closeInErr := in.Close()
		closeOutErr := out.Close()
		if copyErr != nil || closeInErr != nil || closeOutErr != nil {
			_ = os.Remove(dst)
			if copyErr != nil {
				return "", copyErr
			}
			if closeInErr != nil {
				return "", closeInErr
			}
			return "", closeOutErr
		}
		return dst, nil
	}
}

func (s *Server) formFiles(c *fiber.Ctx) ([]*multipart.FileHeader, int64, error) {
	form, err := c.MultipartForm()
	if err != nil {
		return nil, 0, fiber.ErrBadRequest
	}
	files := append([]*multipart.FileHeader{}, form.File["file"]...)
	files = append(files, form.File["files"]...)
	if len(files) == 0 {
		return nil, 0, fiber.ErrBadRequest
	}
	if len(files) > s.config.Storage.UploadMaxFiles {
		return nil, 0, fiber.NewError(fiber.StatusRequestEntityTooLarge, fmt.Sprintf("一次最多上传 %d 个文件", s.config.Storage.UploadMaxFiles))
	}
	var total int64
	maxFileBytes := mbToBytes(s.config.Storage.UploadMaxFileMB)
	maxRequestBytes := mbToBytes(s.config.Storage.UploadMaxMB)
	for _, fh := range files {
		if fh.Size > maxFileBytes {
			return nil, 0, fiber.NewError(fiber.StatusRequestEntityTooLarge, fmt.Sprintf("单个文件不能超过 %d MB", s.config.Storage.UploadMaxFileMB))
		}
		if !s.extensionAllowed(fh.Filename) {
			return nil, 0, fiber.NewError(fiber.StatusForbidden, "该文件扩展名不允许上传")
		}
		total += fh.Size
	}
	if total > maxRequestBytes {
		return nil, 0, fiber.NewError(fiber.StatusRequestEntityTooLarge, fmt.Sprintf("单次上传总量不能超过 %d MB", s.config.Storage.UploadMaxMB))
	}
	return files, total, nil
}

func (s *Server) extensionAllowed(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	for _, blocked := range s.config.Storage.BlockedExtensions {
		if ext == blocked {
			return false
		}
	}
	if len(s.config.Storage.AllowedExtensions) == 0 {
		return true
	}
	for _, allowed := range s.config.Storage.AllowedExtensions {
		if ext == allowed {
			return true
		}
	}
	return false
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{attempts: map[string]loginAttempt{}}
}

func (l *loginLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	attempt := l.attempts[key]
	if !attempt.blockedTil.IsZero() && now.Before(attempt.blockedTil) {
		return false
	}
	if now.Sub(attempt.windowFrom) > loginLimitWindow {
		delete(l.attempts, key)
	}
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

func (l *loginLimiter) recordFailure(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.cleanupLocked(now)
	attempt := l.attempts[key]
	if attempt.windowFrom.IsZero() || now.Sub(attempt.windowFrom) > loginLimitWindow {
		attempt = loginAttempt{windowFrom: now}
	}
	attempt.count++
	if attempt.count >= loginMaxFailures {
		attempt.blockedTil = now.Add(loginBlockFor)
		attempt.count = 0
		attempt.windowFrom = now
	}
	l.attempts[key] = attempt
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
		out = append(out, tokenToDTO(t, now, uploadMaxBytes))
	}
	return c.JSON(out)
}

func (s *Server) createToken(c *fiber.Ctx) error {
	var in tokenRequest
	if err := c.BodyParser(&in); err != nil {
		return fiber.ErrBadRequest
	}
	dirID := firstNonEmpty(in.DirID, in.DirIDSnake)
	dir, ok := s.config.Dir(dirID)
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
	var err error
	if in.Type == "download" {
		_, safePath, err = fsutil.Resolve(dir.Path, in.Path)
	} else {
		_, safePath, err = fsutil.ResolveForCreate(dir.Path, in.Path)
	}
	if err != nil {
		return fiber.ErrBadRequest
	}
	plain, hash, err := security.NewToken()
	if err != nil {
		return err
	}
	t := &store.Token{Hash: hash, Type: in.Type, DirID: dirID, Path: safePath, MaxUses: maxInt(in.MaxUses, in.MaxUsesOld), ExpiresAt: tokenExpiry(s.config, in)}
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

func tokenExpiry(cfg *config.Config, in tokenRequest) sql.NullTime {
	expiresAt := in.ExpiresAt
	if expiresAt == nil {
		expiresAt = in.ExpiresOld
	}
	if expiresAt != nil {
		return sql.NullTime{Time: *expiresAt, Valid: true}
	}
	ttlSeconds := in.TTLSeconds
	if ttlSeconds <= 0 && in.TTLMinutes > 0 {
		ttlSeconds = in.TTLMinutes * 60
	}
	if ttlSeconds <= 0 {
		ttlSeconds = cfg.Tokens.DefaultTTLSeconds
	}
	return sql.NullTime{Time: time.Now().Add(time.Duration(ttlSeconds) * time.Second), Valid: true}
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
		return c.JSON(fiber.Map{"valid": false, "reason": "not_found"})
	}
	valid, reason := tokenValidity(t, time.Now())
	if valid && t.Type == "upload" {
		if maxBytes := s.tokenUploadMaxBytes(); maxBytes > 0 && t.UploadedBytes >= maxBytes {
			valid = false
			reason = "upload_quota_exhausted"
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
	_ = s.store.Audit("token_use", s.clientIP(c), fmt.Sprint(t.ID))
	return c.JSON(resp)
}

func (s *Server) reservePublicToken(c *fiber.Ctx, tokenType string, uploadBytes int64) (store.Token, config.Dir, error) {
	hash := security.HashToken(c.Params("token"))
	if _, _, err := s.lookupPublicToken(c, tokenType); err != nil {
		return store.Token{}, config.Dir{}, err
	}
	t, err := s.store.ReserveTokenUse(hash, tokenType, time.Now(), uploadBytes, s.tokenUploadMaxBytes())
	if err != nil {
		_ = s.store.Audit("token_denied", s.clientIP(c), err.Error())
		if errors.Is(err, store.ErrTokenUploadLimitExceeded) {
			return t, config.Dir{}, fiber.NewError(fiber.StatusRequestEntityTooLarge, "upload token quota exceeded")
		}
		if errors.Is(err, store.ErrTokenNotUsable) {
			return t, config.Dir{}, fiber.ErrForbidden
		}
		return t, config.Dir{}, err
	}
	dir, ok := s.config.Dir(t.DirID)
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
	dir, ok := s.config.Dir(t.DirID)
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
	dir, ok := s.config.Dir(dirID)
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
		valid = false
		reason = "upload_quota_exhausted"
	}
	return tokenDTO{ID: t.ID, Type: t.Type, DirID: t.DirID, Path: t.Path, ExpiresAt: expiresAt, MaxUses: t.MaxUses, Uses: t.Uses, UploadedBytes: t.UploadedBytes, Revoked: t.Revoked, Valid: valid, Reason: reason, CreatedAt: t.CreatedAt}
}

func (s *Server) tokenUploadMaxBytes() int64 {
	return mbToBytes(s.config.Tokens.UploadMaxMB)
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
