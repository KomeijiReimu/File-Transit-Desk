package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	runtime        *Runtime
	configMu       sync.RWMutex
	configWriteMu  sync.Mutex
	transferGateMu sync.RWMutex
	config         *config.Config
	configPath     string
	store          *store.Store
	loginLimiter   *loginLimiter
	transfers      *transferRegistry
	// beforeUploadTransferRegister is a narrow deterministic test seam immediately before gate admission.
	beforeUploadTransferRegister func()
	// beforeUploadFinalCommit is a narrow deterministic test seam after staging close and before the commit gate.
	beforeUploadFinalCommit func()
	// afterDownloadFileHash is a deterministic test seam before final path identity validation.
	afterDownloadFileHash func()
	// duringDownloadFileHash is a deterministic test seam after opening/stat and before hashing bytes.
	duringDownloadFileHash func()
	// beforeDownloadHashAcquire is a deterministic test seam immediately before non-waiting slot acquisition.
	beforeDownloadHashAcquire func()
	// beforeDownloadFinalValidation is a deterministic test seam before canonical path identity re-resolution.
	beforeDownloadFinalValidation func()
	// beforeResourceFileOpen is a deterministic test seam between initial stat and safe open.
	beforeResourceFileOpen     func()
	prepareConfig              func(string, *config.Config) (*config.PreparedSave, *config.Config, error)
	commitPreparedConfig       func(*config.PreparedSave) (bool, error)
	revokeResourceAccess       func([]string) error
	adminVerifySlots           chan struct{}
	verifyAdminPHC             func(string, []byte) (bool, error)
	proxyResolver              *proxyResolver
	devMode                    bool
	devFrontendPort            int
	limiterMu                  sync.Mutex
	rateLimiter                *windowLimiter
	auditLimiter               *windowLimiter
	lookupSession              func(string) (store.Session, error)
	availableDiskSpace         func(string) (uint64, uint64, error)
	openDirectory              func(string) (fsutil.DirectoryReader, error)
	downloadHashMu             sync.Mutex
	downloadHashSlots          chan struct{}
	downloadHashFlights        map[string]*downloadHashFlight
	uploadCleanupMu            sync.Mutex
	uploadCleanupRunning       bool
	uploadCleanupPendingRoots  map[string]config.Dir
	uploadCleanupPendingSource uploadTempCleanupSource
	uploadTempWalker           uploadTempWalkFunc
	maintenanceNow             func() time.Time
	maintenanceContext         func() context.Context
	maintenanceWG              *sync.WaitGroup
}

type Options struct {
	DevMode          bool
	DevFrontendPort  int
	uploadTempWalker uploadTempWalkFunc
	maintenanceNow   func() time.Time
	runtime          *Runtime
}

type fileListResponse struct {
	Dir            string         `json:"dir"`
	Path           string         `json:"path"`
	Entries        []fsutil.Entry `json:"entries"`
	CanUpload      bool           `json:"canUpload"`
	CanDownload    bool           `json:"canDownload"`
	Page           int64          `json:"page"`
	PageSize       int64          `json:"pageSize"`
	HasMore        bool           `json:"hasMore"`
	Truncated      bool           `json:"truncated"`
	TotalKnown     bool           `json:"totalKnown"`
	Total          *int64         `json:"total"`
	ScannedEntries int            `json:"scannedEntries"`
	ScanLimit      int            `json:"scanLimit"`
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

type uploadLeaseRequest struct {
	DirID    string `json:"dirId"`
	Path     string `json:"path"`
	FileName string `json:"fileName"`
	FileSize int64  `json:"fileSize"`
}

type uploadLeaseResponse struct {
	Lease        string    `json:"lease"`
	UploadURL    string    `json:"uploadUrl"`
	RawUploadURL string    `json:"rawUploadUrl"`
	ExpiresAt    time.Time `json:"expiresAt"`
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
		UploadMaxMB                         int      `json:"uploadMaxMB"`
		UploadMaxFileMB                     int      `json:"uploadMaxFileMB"`
		UploadMaxFiles                      int      `json:"uploadMaxFiles"`
		AllowedExtensions                   []string `json:"allowedExtensions"`
		BlockedExtensions                   []string `json:"blockedExtensions"`
		DirectoryListScanLimit              int      `json:"directoryListScanLimit"`
		DirectoryListMaxPageSize            int      `json:"directoryListMaxPageSize"`
		UploadTempCleanupMaxEntries         int      `json:"uploadTempCleanupMaxEntries"`
		UploadTempCleanupMaxDurationSeconds int      `json:"uploadTempCleanupMaxDurationSeconds"`
	} `json:"storage"`
	FilePicker struct {
		MaxScanEntries int `json:"maxScanEntries"`
		MaxPageSize    int `json:"maxPageSize"`
	} `json:"filePicker"`
	Tokens struct {
		DefaultTTLSeconds int64 `json:"defaultTTLSeconds"`
		MaxTTLSeconds     int64 `json:"maxTTLSeconds"`
		UploadMaxMB       int   `json:"uploadMaxMB"`
	} `json:"tokens"`
	Downloads struct {
		LeaseTTLSeconds          int64 `json:"leaseTTLSeconds"`
		ContentHashMaxMB         int   `json:"contentHashMaxMB"`
		MaxConcurrentHashes      int   `json:"maxConcurrentHashes"`
		VerifyHashOnEveryRequest bool  `json:"verifyHashOnEveryRequest"`
	} `json:"downloads"`
	Auth struct {
		UploadLeaseTTLSeconds int64 `json:"uploadLeaseTTLSeconds"`
	} `json:"auth"`
	Server struct {
		KeepaliveIdleTimeoutSeconds int64 `json:"keepaliveIdleTimeoutSeconds"`
	} `json:"server"`
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
	Status      string    `json:"status"`
	IP          string    `json:"ip"`
	Detail      string    `json:"detail"`
	CreatedAt   time.Time `json:"createdAt"`
}

func auditDTOFromStore(log store.AuditLog) auditDTO {
	status := "ok"
	if store.IsAuditFailureAction(log.Action) {
		status = "failed"
	}
	return auditDTO{ID: log.ID, Action: log.Action, ActionLabel: actionLabel(log.Action), Status: status, IP: log.IP, Detail: log.Detail, CreatedAt: log.CreatedAt}
}

type auditPageDTO struct {
	Logs       []auditDTO `json:"logs"`
	Page       int        `json:"page"`
	PageSize   int        `json:"pageSize"`
	Total      int        `json:"total"`
	TotalPages int        `json:"totalPages"`
}

type uploadLimitsDTO struct {
	UploadMaxMB        int      `json:"uploadMaxMB"`
	UploadMaxFileMB    int      `json:"uploadMaxFileMB"`
	UploadMaxFiles     int      `json:"uploadMaxFiles"`
	UploadMaxBytes     int64    `json:"uploadMaxBytes"`
	UploadMaxFileBytes int64    `json:"uploadMaxFileBytes"`
	AllowedExtensions  []string `json:"allowedExtensions"`
	BlockedExtensions  []string `json:"blockedExtensions"`
}

type shareOriginDTO struct {
	Origin string `json:"origin"`
	Label  string `json:"label"`
	Source string `json:"source"`
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

type codedAPIError struct {
	status  int
	code    string
	message string
}

func (e *codedAPIError) Error() string { return e.message }

func newCodedAPIError(status int, code, message string) error {
	return &codedAPIError{status: status, code: code, message: message}
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
	app, err := NewWithOptions(cfg, st, configPath, Options{DevFrontendPort: 5173})
	if err != nil {
		panic(err)
	}
	return app
}

func NewWithOptions(cfg *config.Config, st *store.Store, configPath string, options Options) (*fiber.App, error) {
	if options.DevFrontendPort < 1 || options.DevFrontendPort > 65535 {
		return nil, fmt.Errorf("dev frontend port must be between 1 and 65535")
	}
	if !options.DevMode && cfg.Auth.DevAllowFixedCode && cfg.Auth.TOTPSecret == "" {
		return nil, fmt.Errorf("auth.dev_allow_fixed_code with an empty TOTP secret requires explicit dev mode")
	}
	if cfg.Server.KeepaliveIdleTimeoutSeconds < 1 || cfg.Server.KeepaliveIdleTimeoutSeconds > 86400 {
		return nil, fmt.Errorf("server keepalive idle timeout must be between 1 and 86400 seconds")
	}
	resolver, err := newProxyResolver(cfg.Server)
	if err != nil {
		return nil, err
	}
	bodyLimit, err := checkedFiberBodyLimit(cfg.Storage.UploadMaxMB, int64(^uint(0)>>1))
	if err != nil {
		return nil, err
	}
	runtime := options.runtime
	compatibilityRuntime := runtime == nil
	if compatibilityRuntime {
		runtime = newRuntime(st)
	}
	s := &Server{runtime: runtime, config: cfg, configPath: configPath, store: st, loginLimiter: newLoginLimiter(), rateLimiter: newWindowLimiter(), auditLimiter: newWindowLimiter(), transfers: newTransferRegistry(), adminVerifySlots: newAdminVerifySlots(cfg.Abuse.Login.MaxConcurrentAdminVerifications), proxyResolver: resolver, devMode: options.DevMode, devFrontendPort: options.DevFrontendPort, downloadHashSlots: make(chan struct{}, cfg.Downloads.MaxConcurrentHashes), downloadHashFlights: make(map[string]*downloadHashFlight)}
	runtime.server = s
	s.uploadTempWalker = options.uploadTempWalker
	s.maintenanceNow = options.maintenanceNow
	st.SetAuditPolicy(cfg.Audit.Retain, cfg.Audit.PruneEveryWrites)
	if cfg.Auth.Admin.PasswordHash == "" && cfg.Auth.Admin.PasswordSHA256 != "" {
		log.Printf("level=WARN event=legacy_admin_password_sha256")
	}
	s.warnLegacyResourcesOutsideAllowlist()
	app := fiber.New(fiber.Config{
		BodyLimit:         bodyLimit,
		StreamRequestBody: true,
		ErrorHandler:      jsonErrorHandler,
		IdleTimeout:       time.Duration(cfg.Server.KeepaliveIdleTimeoutSeconds) * time.Second,
	})
	app.Use(capabilityResponseHeaders)
	app.Use(runtime.requestAdmission)
	// 接口使用 Cookie 凭据，CORS 必须显式列出允许来源，不能依赖通配符；
	// 同时动态允许同一主机名的开发前端端口，保证一键启动默认直连后端可用。
	app.Use(cors.New(cors.Config{
		AllowOriginsFunc: func(origin string) bool {
			for _, allowed := range s.cfg().CORS.AllowOrigins {
				if strings.TrimSpace(allowed) == origin {
					return true
				}
			}
			return s.devMode && developmentFrontendOrigin(origin, s.devFrontendPort)
		},
		AllowHeaders:     "Origin, Content-Type, Accept, Authorization",
		AllowMethods:     "GET,POST,PUT,DELETE,OPTIONS",
		AllowCredentials: true,
	}))
	app.Use(s.csrfOriginGuard)
	s.routes(app)
	s.static(app)
	runtime.App = app
	if compatibilityRuntime {
		s.maintenanceContext = func() context.Context { return runtime.ctx }
		s.maintenanceWG = &runtime.wg
		s.triggerCurrentUploadTempCleanup(uploadCleanupSourceStartup)
	} else {
		runtime.startMaintenance()
	}
	runtime.initialized.Store(true)
	if compatibilityRuntime {
		app.Hooks().OnShutdown(func() error {
			runtime.cancel()
			runtime.wg.Wait()
			return nil
		})
	}
	return app, nil
}

func NewRuntimeWithOptions(cfg *config.Config, st *store.Store, configPath string, options Options) (*Runtime, error) {
	runtime := newRuntime(st)
	options.runtime = runtime
	if _, err := NewWithOptions(cfg, st, configPath, options); err != nil {
		runtime.cancel()
		return nil, err
	}
	return runtime, nil
}

func checkedFiberBodyLimit(uploadMB int, maxInt int64) (int, error) {
	const bytesPerMiB int64 = 1024 * 1024
	if uploadMB <= 0 || maxInt <= 0 || int64(uploadMB) > maxInt/bytesPerMiB {
		return 0, fmt.Errorf("storage.upload_max_mb is not representable on this platform")
	}
	return int(int64(uploadMB) * bytesPerMiB), nil
}

func capabilityResponseHeaders(c *fiber.Ctx) error {
	path := c.Path()
	sensitive := path == "/t" || strings.HasPrefix(path, "/t/")
	if path == "/api/tokens" {
		sensitive = true
	}
	switch path {
	case "/api/files/download-lease", "/api/files/download-by-lease", "/api/files/upload-lease", "/api/files/upload-raw-by-lease", "/api/files/upload-by-lease":
		sensitive = true
	}
	if sensitive {
		c.Set(fiber.HeaderCacheControl, "no-store")
		c.Set("Pragma", "no-cache")
		c.Set("Referrer-Policy", "no-referrer")
		c.Set("X-Robots-Tag", "noindex, nofollow, noarchive")
	}
	return c.Next()
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
	payload := fiber.Map{}
	if e, ok := err.(*codedAPIError); ok {
		code = e.status
		message = e.message
		payload["code"] = e.code
	} else if e, ok := err.(*fiber.Error); ok {
		code = e.Code
		message = e.Message
	}
	// 非业务错误不直接回传底层 err.Error，避免文件路径、SQL 或系统细节出现在客户端。
	payload["error"] = message
	return c.Status(code).JSON(payload)
}

func (s *Server) routes(app *fiber.App) {
	// /api 是登录态接口，/t 是公开分享接口；公开下载也走票据，避免 GET 预览直接消耗次数。
	app.Get("/api/health/live", s.healthLive)
	app.Get("/api/health/ready", s.healthReady)
	app.Get("/api/health", s.healthReady)
	app.Post("/api/auth/login", s.login)
	app.Post("/api/auth/admin-login", s.adminLogin)
	app.Get("/api/auth/me", s.auth(s.me))
	app.Post("/api/auth/heartbeat", s.auth(s.heartbeat))
	app.Post("/api/auth/logout", s.auth(s.logout))
	app.Get("/api/dirs", s.auth(s.dirs))
	app.Get("/api/upload-policy", s.auth(s.uploadPolicy))
	app.Get("/api/files/list", s.auth(s.listFiles))
	app.Get("/api/files/download", s.auth(s.downloadFile))
	app.Post("/api/files/download-lease", s.auth(s.createDownloadLease))
	app.Get("/api/files/download-by-lease", s.downloadByLease)
	app.Post("/api/files/upload-lease", s.auth(s.createUploadLease))
	app.Post("/api/files/upload-raw-by-lease", s.uploadRawByLease)
	app.Post("/api/files/upload-by-lease", s.uploadByLease)
	app.Post("/api/files/upload", s.auth(s.uploadFiles))
	app.Get("/api/transfers/active", s.adminOnly(s.activeTransfers))
	app.Post("/api/transfers/:id/cancel", s.adminOnly(s.cancelTransfer))
	app.Get("/api/tokens", s.adminOnly(s.listTokens))
	app.Get("/api/share-origins", s.adminOnly(s.shareOrigins))
	app.Post("/api/tokens", s.adminOnly(s.createToken))
	app.Post("/api/tokens/:id/revoke", s.adminOnly(s.revokeToken))
	app.Delete("/api/tokens/:id", s.adminOnly(s.deleteToken))
	app.Get("/api/audit/logs", s.adminOnly(s.auditLogs))
	app.Get("/api/admin/audit", s.adminOnly(s.auditLogs))
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
	app.Post("/t/:token/upload-lease", s.createPublicUploadLease)
	app.Get("/t/download-by-lease", s.downloadByLease)
	app.Post("/t/upload-raw-by-lease", s.publicUploadRawByLease)
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
	request := s.requestOrigin(c)
	if origin == request {
		return c.Next()
	}
	for _, allowed := range s.cfg().CORS.AllowOrigins {
		if origin == strings.TrimSpace(allowed) {
			return c.Next()
		}
	}
	if s.devMode && sameHostDevelopmentFrontendOrigin(origin, request, s.devFrontendPort) {
		return c.Next()
	}
	s.sampledRequestAudit(c, "csrf_denied", "csrf", "跨站请求来源被拒绝")
	return fiber.ErrForbidden
}

func (s *Server) requestOrigin(c *fiber.Ctx) string {
	proto := "http"
	if c.Context().IsTLS() {
		proto = "https"
	}
	return s.resolveRequestOrigin(socketRemoteIP(c), proto, string(c.Context().Host()), c.Get("X-Forwarded-Proto"), c.Get("X-Forwarded-Host"))
}

func (s *Server) resolveRequestOrigin(remote netip.Addr, proto, hostValue, forwardedProtoValue, forwardedHostValue string) string {
	if s.proxyResolver != nil && s.proxyResolver.isTrusted(remote) {
		if forwardedProto, ok := validForwardedProto(forwardedProtoValue); ok {
			proto = forwardedProto
		}
		if forwardedHost, ok := normalizeOriginHost(forwardedHostValue); ok {
			hostValue = forwardedHost
		}
	}
	host, ok := normalizeOriginHost(hostValue)
	if !ok {
		return ""
	}
	return proto + "://" + host
}

func isUnsafeMethod(method string) bool {
	switch method {
	case fiber.MethodPost, fiber.MethodPut, fiber.MethodPatch, fiber.MethodDelete:
		return true
	default:
		return false
	}
}

func developmentFrontendOrigin(origin string, port int) bool {
	parsed, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" && parsed.Path != "/" {
		return false
	}
	if parsed.Port() != strconv.Itoa(port) {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate()
	}
	return false
}

func sameHostDevelopmentFrontendOrigin(origin, request string, port int) bool {
	if !developmentFrontendOrigin(origin, port) {
		return false
	}
	originURL, err := url.Parse(strings.TrimSpace(origin))
	if err != nil {
		return false
	}
	requestURL, err := url.Parse(strings.TrimSpace(request))
	if err != nil {
		return false
	}
	return strings.EqualFold(originURL.Hostname(), requestURL.Hostname())
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
		if id == "" {
			s.sampledRequestAudit(c, "unauthorized", "", "缺少会话凭据")
			return fiber.ErrUnauthorized
		}
		var sess store.Session
		var err error
		if s.lookupSession != nil {
			sess, err = s.lookupSession(id)
		} else {
			sess, err = s.store.Session(id)
		}
		now := time.Now()
		grace := time.Duration(s.cfg().Auth.IdleGraceSeconds) * time.Second
		idleValid := err == nil && now.Before(sess.IdleExpiresAt)
		withinIdleGrace := err == nil && !idleValid && grace > 0 && now.Before(sess.IdleExpiresAt.Add(grace))
		if withinIdleGrace && c.Path() == "/api/auth/heartbeat" {
			// 只允许心跳在短宽限期内恢复会话，普通业务请求不能借宽限继续访问文件。
			idleValid = true
		}
		if err != nil || !now.Before(sess.ExpiresAt) || !idleValid {
			if id != "" && !withinIdleGrace {
				s.clearSessionCookie(c)
			}
			s.sampledRequestAudit(c, "unauthorized", "", "会话无效或已过期")
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
			s.sampledRequestAudit(c, "forbidden", "", "管理员权限不足")
			return fiber.ErrForbidden
		}
		return next(c)
	})
}

func (s *Server) login(c *fiber.Ctx) error {
	ip := s.clientIP(c)
	if err := s.checkLoginAdmission(c, "user", ip); err != nil {
		return err
	}
	var in struct {
		Code string `json:"code"`
	}
	if err := c.BodyParser(&in); err != nil {
		s.recordLoginFailure("user", ip)
		return fiber.ErrBadRequest
	}
	in.Code = normalizeLoginCode(in.Code)
	if len(in.Code) != 6 {
		s.recordLoginFailure("user", ip)
		return fiber.NewError(fiber.StatusBadRequest, "请输入 6 位动态验证码。")
	}
	if !s.validateLoginCode(in.Code) {
		s.recordLoginFailure("user", ip)
		s.criticalAudit("login_failed", ip, "")
		return fiber.NewError(fiber.StatusUnauthorized, "动态验证码无效，请确认设备时间已同步后重试。")
	}
	s.loginLimiter.reset("user:" + ip)
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
	s.criticalAudit("login_success", ip, "")
	return c.JSON(fiber.Map{"authenticated": true, "role": "user", "expiresAt": expiresAt, "idleExpiresAt": idleExpiresAt})
}

func (s *Server) adminLogin(c *fiber.Ctx) error {
	ip := s.clientIP(c)
	if err := s.checkLoginAdmission(c, "admin", ip); err != nil {
		return err
	}
	if err := protectAdminLoginBody(c); err != nil {
		s.recordLoginFailure("admin", ip)
		return err
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.BodyParser(&in); err != nil {
		s.recordLoginFailure("admin", ip)
		return fiber.ErrBadRequest
	}
	valid, capacityExhausted := s.validateAdminLogin(in.Username, in.Password)
	if capacityExhausted {
		c.Set("Retry-After", "1")
		return newCodedAPIError(fiber.StatusServiceUnavailable, "auth_capacity_exhausted", "管理员认证繁忙，请稍后重试。")
	}
	if !valid {
		s.recordLoginFailure("admin", ip)
		s.criticalAudit("login_failed", ip, "管理员登录")
		return fiber.ErrUnauthorized
	}
	s.loginLimiter.reset("admin:" + ip)
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
	s.criticalAudit("login_success", ip, "管理员登录")
	return c.JSON(fiber.Map{"authenticated": true, "role": "admin", "name": in.Username, "expiresAt": expiresAt, "idleExpiresAt": idleExpiresAt})
}

const maxAdminLoginBodyBytes = 4096

func protectAdminLoginBody(c *fiber.Ctx) error {
	rejectLarge := func() error {
		return newCodedAPIError(fiber.StatusRequestEntityTooLarge, "auth_request_too_large", "管理员登录请求过大。")
	}
	if contentLength := c.Request().Header.ContentLength(); contentLength > maxAdminLoginBodyBytes {
		return rejectLarge()
	}
	if stream := c.Request().BodyStream(); stream != nil {
		body, err := io.ReadAll(io.LimitReader(stream, maxAdminLoginBodyBytes+1))
		if err != nil {
			return newCodedAPIError(fiber.StatusBadRequest, "auth_request_invalid", "管理员登录请求无效。")
		}
		if len(body) > maxAdminLoginBodyBytes {
			return rejectLarge()
		}
		c.Request().SetBodyRaw(body)
		return nil
	}
	if len(c.Body()) > maxAdminLoginBodyBytes {
		return rejectLarge()
	}
	return nil
}

func newAdminVerifySlots(limit int) chan struct{} {
	if limit <= 0 {
		return nil
	}
	return make(chan struct{}, limit)
}

func (s *Server) validateAdminLogin(username, password string) (bool, bool) {
	cfg := s.cfg()
	var providedUsername, expectedUsername [128]byte
	copy(providedUsername[:], []byte(username))
	copy(expectedUsername[:], []byte(cfg.Auth.Admin.Username))
	usernameValid := len(username) <= len(providedUsername) && subtle.ConstantTimeCompare(providedUsername[:], expectedUsername[:]) == 1 && subtle.ConstantTimeEq(int32(len(username)), int32(len(cfg.Auth.Admin.Username))) == 1
	if len(password) > 1024 {
		return false, false
	}
	passwordValid := false
	if cfg.Auth.Admin.PasswordHash != "" {
		if s.adminVerifySlots != nil {
			select {
			case s.adminVerifySlots <- struct{}{}:
				defer func() { <-s.adminVerifySlots }()
			default:
				return false, true
			}
		}
		verify := security.Verify
		if s.verifyAdminPHC != nil {
			verify = s.verifyAdminPHC
		}
		ok, err := verify(cfg.Auth.Admin.PasswordHash, []byte(password))
		passwordValid = err == nil && ok
	} else {
		sum := sha256.Sum256([]byte(password))
		expected, err := hex.DecodeString(cfg.Auth.Admin.PasswordSHA256)
		passwordValid = err == nil && subtle.ConstantTimeCompare(sum[:], expected) == 1
	}
	return usernameValid && passwordValid, false
}

func (s *Server) validateLoginCode(code string) bool {
	code = normalizeLoginCode(code)
	if s.cfg().Auth.TOTPSecret == "" {
		return s.devMode && s.cfg().Auth.DevAllowFixedCode && code == "000000"
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
	if s.proxyResolver != nil {
		return s.proxyResolver.resolveClientIP(c)
	}
	return socketRemoteIP(c).String()
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

func (s *Server) uploadPolicy(c *fiber.Ctx) error {
	cfg := s.cfg()
	return c.JSON(uploadLimitsDTO{
		UploadMaxMB:        cfg.Storage.UploadMaxMB,
		UploadMaxFileMB:    cfg.Storage.UploadMaxFileMB,
		UploadMaxFiles:     cfg.Storage.UploadMaxFiles,
		UploadMaxBytes:     mbToBytes(cfg.Storage.UploadMaxMB),
		UploadMaxFileBytes: mbToBytes(cfg.Storage.UploadMaxFileMB),
		AllowedExtensions:  append([]string{}, cfg.Storage.AllowedExtensions...),
		BlockedExtensions:  append([]string{}, cfg.Storage.BlockedExtensions...),
	})
}

func (s *Server) shareOrigins(c *fiber.Ctx) error {
	scheme, port := shareOriginSchemePort(c.Query("currentOrigin"), c)
	items := make([]shareOriginDTO, 0)
	seen := map[string]bool{}
	for _, ip := range localShareIPs() {
		origin := originFromIP(scheme, ip, port)
		if origin == "" || seen[origin] {
			continue
		}
		seen[origin] = true
		label := "本机地址 " + ip.String()
		if ip.IsPrivate() {
			label = "局域网 " + ip.String()
		}
		items = append(items, shareOriginDTO{Origin: origin, Label: label, Source: "interface"})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Origin < items[j].Origin })
	return c.JSON(items)
}

func shareOriginSchemePort(currentOrigin string, c *fiber.Ctx) (string, string) {
	scheme, port := "http", ""
	if parsed, err := url.Parse(strings.TrimSpace(currentOrigin)); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		scheme = parsed.Scheme
		port = parsed.Port()
		return scheme, port
	}
	if c.Protocol() != "" {
		scheme = c.Protocol()
	}
	if host := c.Hostname(); host != "" {
		if _, p, err := net.SplitHostPort(host); err == nil {
			port = p
		} else if strings.Contains(host, ":") && !strings.Contains(host, "]") {
			port = ""
		} else if idx := strings.LastIndex(host, ":"); idx > -1 {
			port = host[idx+1:]
		}
	}
	return scheme, port
}

func localShareIPs() []net.IP {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]net.IP, 0)
	seen := map[string]bool{}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
				continue
			}
			if v4 := ip.To4(); v4 != nil {
				ip = v4
			}
			key := ip.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, ip)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func originFromIP(scheme string, ip net.IP, port string) string {
	if ip == nil {
		return ""
	}
	host := ip.String()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host = net.JoinHostPort(strings.Trim(host, "[]"), port)
		if strings.Contains(ip.String(), ":") {
			host = "[" + ip.String() + "]:" + port
		}
	}
	return scheme + "://" + host
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
	out.Storage.DirectoryListScanLimit = cfg.Storage.DirectoryListScanLimit
	out.Storage.DirectoryListMaxPageSize = cfg.Storage.DirectoryListMaxPageSize
	out.Storage.UploadTempCleanupMaxEntries = cfg.Storage.UploadTempCleanupMaxEntries
	out.Storage.UploadTempCleanupMaxDurationSeconds = cfg.Storage.UploadTempCleanupMaxDurationSeconds
	out.FilePicker.MaxScanEntries = cfg.FilePicker.MaxScanEntries
	out.FilePicker.MaxPageSize = cfg.FilePicker.MaxPageSize
	out.Tokens.DefaultTTLSeconds = cfg.Tokens.DefaultTTLSeconds
	out.Tokens.MaxTTLSeconds = cfg.Tokens.MaxTTLSeconds
	out.Tokens.UploadMaxMB = cfg.Tokens.UploadMaxMB
	out.Downloads.LeaseTTLSeconds = cfg.Downloads.LeaseTTLSeconds
	out.Downloads.ContentHashMaxMB = cfg.Downloads.ContentHashMaxMB
	out.Downloads.MaxConcurrentHashes = cfg.Downloads.MaxConcurrentHashes
	out.Downloads.VerifyHashOnEveryRequest = cfg.Downloads.VerifyHashOnEveryRequest
	out.Auth.UploadLeaseTTLSeconds = cfg.Auth.UploadLeaseTTLSeconds
	out.Server.KeepaliveIdleTimeoutSeconds = cfg.Server.KeepaliveIdleTimeoutSeconds
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
	if err := s.updateConfigResources(func(resources []config.Dir) ([]config.Dir, error) {
		for _, existing := range resources {
			if existing.ID == resource.ID {
				return nil, fiber.NewError(fiber.StatusConflict, "资源 ID 已存在。")
			}
		}
		if !resource.AllowDownload && !resource.AllowUpload {
			return nil, fiber.NewError(fiber.StatusBadRequest, "新资源至少需要启用下载或上传权限。")
		}
		if err := validateResourcePathWithHook(resource, s.beforeResourceFileOpen); err != nil {
			return nil, err
		}
		if err := s.validateResourceSelection(s.cfg(), resource); err != nil {
			return nil, err
		}
		canonicalPath, err := fsutil.Canonical(resource.Path)
		if err != nil {
			return nil, friendlyPathError(err, "路径不存在，请先确认服务端路径。")
		}
		resource.Path = canonicalPath
		return append(resources, resource), nil
	}); err != nil {
		return err
	}
	s.criticalAudit("config_resource_create", s.clientIP(c), fmt.Sprintf("新增%s资源 %s", resourceTypeLabel(resource.Type), resource.ID))
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
	if err := s.updateConfigResources(func(resources []config.Dir) ([]config.Dir, error) {
		for i, existing := range resources {
			if existing.ID == resource.ID {
				cfg := s.cfg()
				if err := s.validateProtectedResourcePath(cfg, existing); err != nil {
					return nil, err
				}
				if s.resourceRequiresLegacyRestrictions(cfg, existing) {
					if err := validateLegacyResourceUpdate(existing, resource); err != nil {
						return nil, err
					}
				} else {
					if err := s.validateResourceSelection(cfg, resource); err != nil {
						return nil, err
					}
					if err := validateResourcePathWithHook(resource, s.beforeResourceFileOpen); err != nil {
						return nil, err
					}
					if resource.Path != existing.Path {
						canonicalPath, err := fsutil.Canonical(resource.Path)
						if err != nil {
							return nil, friendlyPathError(err, "路径不存在，请先确认服务端路径。")
						}
						resource.Path = canonicalPath
					}
				}
				resources[i] = resource
				return resources, nil
			}
		}
		return nil, fiber.ErrNotFound
	}); err != nil {
		return err
	}
	s.criticalAudit("config_resource_update", s.clientIP(c), fmt.Sprintf("修改%s资源 %s", resourceTypeLabel(resource.Type), resource.ID))
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
	s.criticalAudit("config_resource_delete", s.clientIP(c), "删除资源 "+id)
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
	s.criticalAudit("config_upload_policy_update", s.clientIP(c), fmt.Sprintf("允许 %d 项，阻断 %d 项", len(saved.Storage.AllowedExtensions), len(saved.Storage.BlockedExtensions)))
	return c.JSON(uploadPolicyResponse{AllowedExtensions: saved.Storage.AllowedExtensions, BlockedExtensions: saved.Storage.BlockedExtensions})
}

func (s *Server) updateConfigResources(mutator func([]config.Dir) ([]config.Dir, error)) error {
	var oldResources []config.Dir
	var changedIDs []string
	if err := s.updateConfigWithBeforePublish(func(next *config.Config) error {
		resources, err := mutator(next.Resources())
		if err != nil {
			return err
		}
		next.SetResources(resources)
		return nil
	}, func(old, next *config.Config) error {
		oldResources = old.Resources()
		changedIDs = changedResourceIDs(oldResources, next.Resources())
		if len(changedIDs) == 0 {
			return nil
		}
		return s.revokeResourceAuthorizations(changedIDs)
	}, func() {
		if len(changedIDs) > 0 {
			s.transfers.cancelUploadsByDirIDs(changedIDs)
		}
	}); err != nil {
		return err
	}
	if len(changedIDs) > 0 {
		wanted := make(map[string]struct{}, len(changedIDs))
		for _, id := range changedIDs {
			wanted[id] = struct{}{}
		}
		roots := make([]config.Dir, 0, len(changedIDs))
		for _, dir := range oldResources {
			if _, ok := wanted[dir.ID]; ok {
				roots = append(roots, dir)
			}
		}
		s.triggerUploadTempCleanup(uploadTempCleanupRequest{Source: uploadCleanupSourceResourceChange, Roots: roots})
	}
	return nil
}

func (s *Server) revokeResourceAuthorizations(dirIDs []string) error {
	if s.revokeResourceAccess != nil {
		return s.revokeResourceAccess(dirIDs)
	}
	return s.store.RevokeTokensByDirIDsAndLeases(dirIDs)
}

func (s *Server) updateConfig(mutator func(*config.Config) error) error {
	return s.updateConfigWithBeforePublish(mutator, nil, nil)
}

func (s *Server) updateConfigWithBeforePublish(mutator func(*config.Config) error, beforePublish func(old, next *config.Config) error, afterPublished func()) error {
	if s.configPath == "" {
		return fiber.NewError(fiber.StatusServiceUnavailable, "当前服务未记录配置文件路径，不能在线写回配置。")
	}
	s.configWriteMu.Lock()
	defer s.configWriteMu.Unlock()
	old := s.cfg().Clone()
	next := old.Clone()
	if err := mutator(next); err != nil {
		return err
	}
	prepared, normalized, err := s.prepareConfigSave(s.configPath, next)
	if err != nil {
		log.Printf("[ERROR] config prepare failed: %v", err)
		if errors.Is(err, config.ErrInvalidConfig) {
			return fiber.NewError(fiber.StatusBadRequest, "配置内容无效，请检查后重试。")
		}
		return fiber.NewError(fiber.StatusServiceUnavailable, "配置暂时无法写入，请稍后重试。")
	}
	defer prepared.Abort()
	if beforePublish != nil {
		if err := beforePublish(old, normalized); err != nil {
			log.Printf("[CRITICAL] config authorization revocation failed; prepared config aborted: %v", err)
			return fiber.NewError(fiber.StatusInternalServerError, "配置发布前置处理失败，请稍后重试。")
		}
	}
	gateHeld := afterPublished != nil
	if gateHeld {
		s.transferGateMu.Lock()
	}
	published, commitErr := s.commitConfigSave(prepared)
	if published {
		// rename 成功后磁盘配置已经对新进程可见，即使目录同步失败也必须切换当前进程内存配置。
		s.replaceConfig(normalized)
		if afterPublished != nil {
			afterPublished()
		}
	}
	if gateHeld {
		s.transferGateMu.Unlock()
	}
	if commitErr != nil {
		if published {
			log.Printf("[CRITICAL] config published but parent directory sync failed: %v", commitErr)
			if beforePublish != nil {
				if auditErr := s.store.Audit("config_resource_published_sync_failed", "", "资源配置已发布，但目录同步失败"); auditErr != nil {
					log.Printf("[CRITICAL] failed to persist config sync failure audit: %v", auditErr)
				}
			}
		} else {
			log.Printf("[ERROR] config publish failed after authorization revocation: %v", commitErr)
		}
		return fiber.NewError(fiber.StatusInternalServerError, "配置发布失败，请稍后重试。")
	}
	return nil
}

func (s *Server) prepareConfigSave(path string, next *config.Config) (*config.PreparedSave, *config.Config, error) {
	if s.prepareConfig != nil {
		return s.prepareConfig(path, next)
	}
	return config.PrepareAtomic(path, next)
}

func (s *Server) commitConfigSave(prepared *config.PreparedSave) (bool, error) {
	if s.commitPreparedConfig != nil {
		return s.commitPreparedConfig(prepared)
	}
	return prepared.Commit()
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
	return validateResourcePathWithHook(resource, nil)
}

func validateResourcePathWithHook(resource config.Dir, beforeOpen func()) error {
	info, err := os.Stat(resource.Path)
	if err != nil {
		return friendlyPathError(err, "路径不存在，请先确认服务端路径。")
	}
	if resource.Type == config.ResourceFile {
		if !info.Mode().IsRegular() {
			return resourceFileNotRegularError()
		}
		if beforeOpen != nil {
			beforeOpen()
		}
		file, err := openDownloadFile(resource.Path)
		if err != nil {
			return resourceFileChangedError()
		}
		opened, statErr := file.Stat()
		closeErr := file.Close()
		if statErr != nil || closeErr != nil {
			return resourceFileChangedError()
		}
		if !opened.Mode().IsRegular() {
			return resourceFileNotRegularError()
		}
		if !os.SameFile(info, opened) || info.Size() != opened.Size() || !info.ModTime().Equal(opened.ModTime()) {
			return resourceFileChangedError()
		}
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

func resourceFileNotRegularError() error {
	return newCodedAPIError(fiber.StatusBadRequest, "resource_file_not_regular", "单文件资源目标不可用。")
}

func resourceFileChangedError() error {
	return newCodedAPIError(fiber.StatusConflict, "resource_file_changed", "单文件资源在校验期间发生变化，请重试。")
}

func (s *Server) validateResourceSelection(cfg *config.Config, resource config.Dir) error {
	if err := s.validateProtectedResourcePath(cfg, resource); err != nil {
		return err
	}
	if !resourceWithinPickerRoots(cfg.FilePicker.Roots, resource) {
		return newCodedAPIError(fiber.StatusForbidden, "resource_path_outside_allowlist", "资源路径不在服务端允许范围内。")
	}
	return nil
}

func resourceWithinPickerRoots(roots []config.FilePickerRoot, resource config.Dir) bool {
	for _, root := range roots {
		if resource.Type == config.ResourceFile && !root.AllowSelectFiles || resource.Type == config.ResourceDirectory && !root.AllowSelectDirs {
			continue
		}
		inside, err := fsutil.IsInside(root.Path, resource.Path)
		if err == nil && inside {
			return true
		}
	}
	return false
}

func (s *Server) resourceRequiresLegacyRestrictions(cfg *config.Config, resource config.Dir) bool {
	return !resourceWithinPickerRoots(cfg.FilePicker.Roots, resource)
}

func validateLegacyResourceUpdate(current, next config.Dir) error {
	if current.Path != next.Path || current.Type != next.Type || next.AllowDownload && !current.AllowDownload || next.AllowUpload && !current.AllowUpload {
		return newCodedAPIError(fiber.StatusForbidden, "resource_path_outside_allowlist", "旧资源只能删除、修改显示名称或收紧权限。")
	}
	return nil
}

type protectedResourcePaths struct {
	files       []string
	directories []string
	staticDir   string
	configDir   string
	err         error
}

func (s *Server) protectedResourcePaths(cfg *config.Config) protectedResourcePaths {
	paths := protectedResourcePaths{}
	appendFile := func(path string) {
		if strings.TrimSpace(path) != "" {
			paths.files = append(paths.files, path)
		}
	}
	appendFile(s.configPath)
	if s.configPath != "" {
		appendFile(s.configPath + ".bak")
		paths.configDir = filepath.Dir(s.configPath)
	}
	appendFile(cfg.Database.Path)
	if cfg.Database.Path != "" {
		appendFile(cfg.Database.Path + "-wal")
		appendFile(cfg.Database.Path + "-shm")
		appendFile(cfg.Database.Path + "-journal")
		if canonicalDB, err := fsutil.Canonical(cfg.Database.Path); err == nil {
			appendFile(canonicalDB + "-wal")
			appendFile(canonicalDB + "-shm")
			appendFile(canonicalDB + "-journal")
		}
	}
	if executable, err := os.Executable(); err == nil {
		appendFile(executable)
	} else {
		paths.err = err
	}
	if strings.TrimSpace(cfg.Web.StaticDir) != "" {
		paths.staticDir = cfg.Web.StaticDir
		paths.directories = append(paths.directories, cfg.Web.StaticDir)
	}
	return paths
}

func (s *Server) validateProtectedResourcePath(cfg *config.Config, resource config.Dir) error {
	protected := s.protectedResourcePaths(cfg)
	reject := func() error {
		return newCodedAPIError(fiber.StatusForbidden, "resource_path_protected", "该路径受服务端保护，不能配置为资源。")
	}
	if protected.err != nil {
		return reject()
	}
	if resource.Type == config.ResourceFile {
		if protected.configDir != "" {
			sameDir, err := fsutil.SamePath(filepath.Dir(resource.Path), protected.configDir)
			if err != nil {
				return reject()
			}
			matched, matchErr := filepath.Match(".config-*.yaml.tmp", strings.ToLower(filepath.Base(resource.Path)))
			if matchErr != nil || sameDir && matched {
				return reject()
			}
		}
		for _, protectedFile := range protected.files {
			equal, err := canonicalPathsEqual(resource.Path, protectedFile)
			if err != nil || equal {
				return reject()
			}
		}
		for _, protectedDir := range protected.directories {
			inside, err := fsutil.IsInside(protectedDir, resource.Path)
			if err != nil || inside {
				return reject()
			}
		}
		return nil
	}
	for _, protectedFile := range protected.files {
		contains, err := fsutil.IsInside(resource.Path, protectedFile)
		if err != nil || contains {
			return reject()
		}
	}
	for _, protectedDir := range protected.directories {
		contains, err := fsutil.IsInside(resource.Path, protectedDir)
		if err != nil || contains {
			return reject()
		}
		inside, err := fsutil.IsInside(protectedDir, resource.Path)
		if err != nil || inside {
			return reject()
		}
	}
	if resource.AllowUpload && protected.staticDir != "" {
		left, leftErr := fsutil.IsInside(resource.Path, protected.staticDir)
		right, rightErr := fsutil.IsInside(protected.staticDir, resource.Path)
		if leftErr != nil || rightErr != nil || left || right {
			return reject()
		}
	}
	return nil
}

func canonicalPathsEqual(left, right string) (bool, error) {
	return fsutil.SamePath(left, right)
}

func (s *Server) warnLegacyResourcesOutsideAllowlist() {
	legacyIDs := make([]string, 0)
	cfg := s.cfg()
	for _, resource := range cfg.Resources() {
		if !resourceWithinPickerRoots(cfg.FilePicker.Roots, resource) {
			legacyIDs = append(legacyIDs, resource.ID)
		}
	}
	if len(legacyIDs) > 0 {
		sort.Strings(legacyIDs)
		log.Printf("level=WARN event=legacy_resources_outside_allowlist resource_ids=%q", strings.Join(legacyIDs, ","))
	}
}

func resourceAuthorizationFingerprint(dir config.Dir) string {
	canonical := canonicalResourcePath(dir.Path)
	material := strings.Join([]string{
		"v1",
		dir.ID,
		dir.Type,
		canonical,
		strconv.FormatBool(dir.AllowUpload),
		strconv.FormatBool(dir.AllowDownload),
	}, "\x00")
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])
}

func resourceAuthorizationFingerprintMatches(stored string, dir config.Dir) bool {
	return stored != "" && stored == resourceAuthorizationFingerprint(dir)
}

func canonicalResourcePath(path string) string {
	cleaned := filepath.Clean(strings.TrimSpace(path))
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return cleaned
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(real)
	}
	return filepath.Clean(abs)
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
	cfg := s.cfg()
	page, err := parseListingPage(c, cfg.Storage.DirectoryListMaxPageSize, cfg.Storage.DirectoryListScanLimit, true)
	if err != nil {
		return err
	}
	if isFileResource(dir) {
		if err := validateFileResourceListPath(dir, c.Query("path")); err != nil {
			s.criticalAudit("illegal_access", s.clientIP(c), fmt.Sprintf("单文件资源 %s 路径校验失败", dir.ID))
			return err
		}
		entry, err := fileResourceEntry(dir)
		if err != nil {
			return err
		}
		entries := []fsutil.Entry{}
		if page.Page == 1 {
			entries = append(entries, entry)
		}
		total := int64(1)
		_ = s.store.Audit("file_list", s.clientIP(c), fmt.Sprintf("单文件资源 %s", dir.ID))
		return c.JSON(fileListResponse{Dir: dir.ID, Path: "", Entries: entries, CanUpload: false, CanDownload: dir.AllowDownload, Page: page.Page, PageSize: page.PageSize, HasMore: false, TotalKnown: true, Total: &total, ScannedEntries: 1, ScanLimit: cfg.Storage.DirectoryListScanLimit})
	}
	result, err := fsutil.ListDirectory(dir.Path, c.Query("path"), fsutil.ListOptions{ScanLimit: cfg.Storage.DirectoryListScanLimit, Page: page.Page, PageSize: page.PageSize, OpenDir: s.openDirectory})
	if err != nil {
		if errors.Is(err, fsutil.ErrPageOutOfRange) {
			return mapListingError(err)
		}
		s.criticalAudit("illegal_access", s.clientIP(c), fmt.Sprintf("目录 %s 列表路径校验失败", dir.ID))
		return friendlyPathError(err, "路径不存在，请检查路径或返回上级目录。")
	}
	if !dir.AllowDownload {
		for i := range result.Entries {
			result.Entries[i].Downloadable = false
		}
	}
	_, safePath, _ := fsutil.Resolve(dir.Path, c.Query("path"))
	_ = s.store.Audit("file_list", s.clientIP(c), fmt.Sprintf("目录 %s，路径 %s", dir.ID, displayPath(safePath)))
	return c.JSON(fileListResponse{Dir: dir.ID, Path: safePath, Entries: result.Entries, CanUpload: dir.AllowUpload, CanDownload: dir.AllowDownload, Page: result.Page, PageSize: result.PageSize, HasMore: result.HasMore, Truncated: result.Truncated, TotalKnown: result.TotalKnown, Total: result.Total, ScannedEntries: result.ScannedEntries, ScanLimit: result.ScanLimit})
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
	full, info, err = s.revalidateDownloadFile(dir, c.Query("path"), info)
	if err != nil {
		return err
	}
	s.bestEffortAudit("download", s.clientIP(c), fmt.Sprintf("目录 %s，文件 %s", dir.ID, displayPath(safePath)))
	done := s.registerDownloadTransfer(c, "session", dir.ID, safePath, info.Size())
	defer done()
	return c.Download(full)
}

func (s *Server) createDownloadLease(c *fiber.Ctx) error {
	sessionID := fmt.Sprint(c.Locals("sessionID"))
	if err := s.checkLeaseCreationRate(c, "session:"+sessionID, false); err != nil {
		return err
	}
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
	fileSHA256, checkedInfo, err := s.downloadLeaseFileHash(c, full, info)
	if err != nil {
		return err
	}
	full, info, err = s.revalidateDownloadFile(dir, safePath, checkedInfo)
	if err != nil {
		return err
	}
	_ = full
	lease, plain, err := s.createLeaseRecord(store.DownloadLease{
		Source:              "session",
		SessionID:           sql.NullString{String: sessionID, Valid: true},
		Role:                fmt.Sprint(c.Locals("role")),
		DirID:               dir.ID,
		Path:                safePath,
		FileSize:            info.Size(),
		FileMtime:           normalizedFileMtime(info),
		FileSHA256:          fileSHA256,
		ResourceFingerprint: resourceAuthorizationFingerprint(dir),
	})
	if err != nil {
		return s.creationStoreError(c, err)
	}
	s.criticalAudit("download_lease_create", s.clientIP(c), fmt.Sprintf("目录 %s，文件 %s", dir.ID, displayPath(safePath)))
	return c.JSON(downloadLeaseResponse{URL: s.downloadLeaseURL(plain, false), ExpiresAt: lease.ExpiresAt})
}

func (s *Server) createPublicDownloadLease(c *fiber.Ctx) error {
	tokenHash := security.HashToken(c.Params("token"))
	if err := s.checkLeaseCreationRate(c, "public:"+tokenHash, true); err != nil {
		return err
	}
	lease, plain, err := s.createPublicDownloadLeaseRecord(c)
	if err != nil {
		return s.creationStoreError(c, err)
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
		return fiber.ErrUnauthorized
	}
	dir, ok := s.cfg().Dir(lease.DirID)
	if !ok {
		return fiber.ErrForbidden
	}
	if !resourceAuthorizationFingerprintMatches(lease.ResourceFingerprint, dir) {
		s.criticalAudit("download_lease_resource_changed", s.clientIP(c), fmt.Sprintf("票据 #%d，目录 %s", lease.ID, lease.DirID))
		return fiber.NewError(fiber.StatusForbidden, "下载票据绑定的资源已变化，请重新获取下载链接。")
	}
	if !dir.AllowDownload {
		return fiber.ErrForbidden
	}
	full, _, info, err := s.resolveDownloadFile(dir, lease.Path)
	if err != nil {
		return downloadFileSafetyError(errDownloadFileChanged)
	}
	// 下载票据绑定文件大小、修改时间和可选内容哈希，避免同一路径文件被替换后继续复用旧授权。
	if info.Size() != lease.FileSize || !normalizedFileMtime(info).Equal(lease.FileMtime.UTC()) {
		s.criticalAudit("download_lease_file_changed", s.clientIP(c), fmt.Sprintf("目录 %s，文件 %s", lease.DirID, displayPath(lease.Path)))
		return fiber.NewError(fiber.StatusConflict, "文件已变化，请重新获取下载链接。")
	}
	firstUse := false
	if !lease.LastUsedAt.Valid {
		full, info, firstUse, err = s.ensureDownloadLeaseFirstUse(c, lease, dir, full, info)
	} else {
		checkedInfo := info
		if s.cfg().Downloads.VerifyHashOnEveryRequest {
			checkedInfo, err = s.verifyDownloadLeaseContent(c, lease, full, info)
		}
		if err == nil {
			full, info, err = s.revalidateDownloadFile(dir, lease.Path, checkedInfo)
		}
	}
	if err != nil {
		return downloadHashRequestError(c, err)
	}
	if firstUse {
		_ = s.store.Audit("download_lease_use", s.clientIP(c), fmt.Sprintf("首次使用%s下载票据，目录 %s，文件 %s", lease.Source, lease.DirID, displayPath(lease.Path)))
	}
	done := s.registerDownloadTransfer(c, "download_lease", lease.DirID, lease.Path, info.Size())
	defer done()
	return c.Download(full)
}

func (s *Server) registerDownloadTransfer(c *fiber.Ctx, source, dirID, path string, size int64) func() {
	id, _, _ := security.NewToken()
	rec := &transferRecord{ID: id, Type: "download", Status: transferActive, Source: source, DirID: dirID, Path: filepath.Dir(path), FileName: filepath.Base(path), TotalBytes: size, StartedAt: time.Now(), UpdatedAt: time.Now(), ClientIP: s.clientIP(c), Cancelable: false, BestEffort: true}
	s.transfers.add(rec)
	return func() { s.transfers.remove(id) }
}

func (s *Server) createLeaseRecord(lease store.DownloadLease) (store.DownloadLease, string, error) {
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
	limits := s.cfg().Abuse.Creation
	if err := s.store.CreateDownloadLeaseLimited(&lease, limits.MaxOutstandingLeasesTotal, limits.MaxOutstandingLeasesOwner); err != nil {
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

func (s *Server) downloadLeaseFileHash(c *fiber.Ctx, full string, info os.FileInfo) (sql.NullString, os.FileInfo, error) {
	maxBytes := s.downloadLeaseHashMaxBytes()
	if maxBytes > 0 && info.Size() > maxBytes {
		// 大文件默认跳过内容哈希，用大小和 mtime 兜底，避免 Range 续传前反复读完整文件。
		return sql.NullString{String: "", Valid: true}, info, nil
	}
	// 内容哈希让小文件票据具备内容级绑定；大文件可通过配置选择是否启用，避免 Range 续传反复扫完整文件。
	hash, checkedInfo, err := s.hashDownloadFile(full)
	if err != nil {
		return sql.NullString{}, nil, downloadHashRequestError(c, err)
	}
	if s.afterDownloadFileHash != nil {
		s.afterDownloadFileHash()
	}
	return sql.NullString{String: hash, Valid: true}, checkedInfo, nil
}

func (s *Server) downloadLeaseHashMaxBytes() int64 {
	if s.cfg().Downloads.ContentHashMaxMB <= 0 {
		return 0
	}
	return int64(s.cfg().Downloads.ContentHashMaxMB) * 1024 * 1024
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
		full, err := fsutil.Canonical(dir.Path)
		if err != nil {
			return "", "", nil, fiber.ErrNotFound
		}
		info, err := os.Stat(full)
		if err != nil || !info.Mode().IsRegular() {
			return "", "", nil, fiber.ErrNotFound
		}
		return full, "", info, nil
	}
	full, safePath, err := fsutil.Resolve(dir.Path, rel)
	if err != nil {
		return "", "", nil, friendlyPathError(err, "文件路径不存在，请刷新文件列表后重试。")
	}
	info, err := os.Stat(full)
	if err != nil || !info.Mode().IsRegular() {
		return "", "", nil, fiber.ErrNotFound
	}
	return full, safePath, info, nil
}

func isFileResource(dir config.Dir) bool {
	return dir.Type == config.ResourceFile
}

func fileResourceEntry(dir config.Dir) (fsutil.Entry, error) {
	info, err := os.Stat(dir.Path)
	if err != nil || !info.Mode().IsRegular() {
		return fsutil.Entry{}, fiber.ErrNotFound
	}
	name := filepath.Base(dir.Path)
	return fsutil.Entry{Name: name, IsDir: false, Size: info.Size(), ModifiedAt: info.ModTime().Format(time.RFC3339), Path: name, Type: "file", MetadataKnown: true, Downloadable: dir.AllowDownload}, nil
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
	if length := c.Request().Header.ContentLength(); length > 0 && int64(length) > mbToBytes(s.cfg().Storage.UploadMaxMB) {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, fmt.Sprintf("单次上传总量不能超过 %d MB", s.cfg().Storage.UploadMaxMB))
	}
	dirID := c.Query("dirId")
	rel := c.Query("path")
	var dir config.Dir
	var err error
	if dirID != "" {
		dir, err = s.dirByID(dirID)
		if err != nil {
			return err
		}
	}
	expectedFingerprint := ""
	if dir.ID != "" {
		expectedFingerprint = resourceAuthorizationFingerprint(dir)
	}
	permitID, err := s.acquireUploadPermit(c, dir.ID, expectedFingerprint, "session", fmt.Sprint(c.Locals("sessionID")))
	if err != nil {
		return err
	}
	defer s.transfers.releaseUploadPermit(permitID)
	resp, err := s.saveStreamingMultipart(c, streamUploadOptions{dir: dir, rel: rel, source: "session", ownerType: "session", ownerID: fmt.Sprint(c.Locals("sessionID")), permitID: permitID, requireTargetBeforeFile: c.Query("dirId") == "", expectedSize: -1, expectedFingerprint: expectedFingerprint})
	if err != nil {
		return err
	}
	return c.JSON(resp)
}

func (s *Server) createUploadLease(c *fiber.Ctx) error {
	sessionID := fmt.Sprint(c.Locals("sessionID"))
	if err := s.checkLeaseCreationRate(c, "session:"+sessionID, false); err != nil {
		return err
	}
	var in uploadLeaseRequest
	if err := c.BodyParser(&in); err != nil {
		return fiber.ErrBadRequest
	}
	dir, err := s.dirByID(in.DirID)
	if err != nil {
		return err
	}
	if !dir.AllowUpload {
		return fiber.ErrForbidden
	}
	if in.FileSize < 0 || in.FileSize > mbToBytes(s.cfg().Storage.UploadMaxFileMB) || in.FileSize > mbToBytes(s.cfg().Storage.UploadMaxMB) {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, fmt.Sprintf("单个文件不能超过 %d MB", s.cfg().Storage.UploadMaxFileMB))
	}
	name := fsutil.SafeName(in.FileName)
	if name == "" || !s.extensionAllowed(name) {
		return fiber.NewError(fiber.StatusForbidden, "该文件扩展名不允许上传")
	}
	if err := os.MkdirAll(dir.Path, 0755); err != nil {
		return friendlyPathError(err, "上传目录不存在或不可访问。")
	}
	_, safeRel, err := fsutil.ResolveForCreate(dir.Path, in.Path)
	if err != nil {
		s.criticalAudit("illegal_access", s.clientIP(c), fmt.Sprintf("目录 %s 上传票据路径校验失败", dir.ID))
		return fiber.ErrBadRequest
	}
	if err := s.checkUploadDiskReserve(c, dir.Path, in.FileSize); err != nil {
		return err
	}
	plain, hash, err := security.NewToken()
	if err != nil {
		return err
	}
	now := time.Now()
	expiresAt := now.Add(time.Duration(s.cfg().Auth.UploadLeaseTTLSeconds) * time.Second)
	lease := store.UploadLease{Hash: hash, Source: "session", SessionID: sessionID, Role: fmt.Sprint(c.Locals("role")), DirID: dir.ID, Path: safeRel, FileName: name, FileSize: in.FileSize, ResourceFingerprint: resourceAuthorizationFingerprint(dir), ExpiresAt: expiresAt, CreatedAt: now, ClientIP: s.clientIP(c)}
	limits := s.cfg().Abuse.Creation
	if err := s.store.CreateUploadLeaseLimited(&lease, limits.MaxOutstandingLeasesTotal, limits.MaxOutstandingLeasesOwner); err != nil {
		return s.creationStoreError(c, err)
	}
	s.criticalAudit("upload_lease_create", s.clientIP(c), fmt.Sprintf("目录 %s，路径 %s，文件 %s", dir.ID, displayPath(safeRel), name))
	return c.JSON(uploadLeaseResponse{Lease: plain, UploadURL: "/api/files/upload-by-lease", RawUploadURL: "/api/files/upload-raw-by-lease", ExpiresAt: expiresAt})
}

func (s *Server) uploadByLease(c *fiber.Ctx) error {
	plain := bearerToken(c.Get("Authorization"))
	if plain == "" {
		return fiber.ErrUnauthorized
	}
	hash := security.HashToken(plain)
	now := time.Now()
	lease, err := s.store.UploadLeaseByHash(hash)
	if err != nil {
		return fiber.ErrUnauthorized
	}
	if lease.UsedAt.Valid || !now.Before(lease.ExpiresAt) {
		return fiber.ErrUnauthorized
	}
	if lease.Source == "" {
		lease.Source = "session"
	}
	if lease.Source != "session" {
		return fiber.ErrUnauthorized
	}
	dir, ok := s.cfg().Dir(lease.DirID)
	if !ok || !dir.AllowUpload {
		return fiber.ErrForbidden
	}
	if !resourceAuthorizationFingerprintMatches(lease.ResourceFingerprint, dir) {
		s.criticalAudit("upload_lease_resource_changed", s.clientIP(c), fmt.Sprintf("票据 #%d，目录 %s", lease.ID, lease.DirID))
		return fiber.NewError(fiber.StatusForbidden, "上传票据绑定的资源已变化，请重新创建上传票据。")
	}
	lease, err = s.store.ReserveUploadLease(hash, now)
	if err != nil {
		return fiber.ErrUnauthorized
	}
	dir, ok = s.cfg().Dir(lease.DirID)
	if !ok || !dir.AllowUpload || !resourceAuthorizationFingerprintMatches(lease.ResourceFingerprint, dir) {
		s.criticalAudit("upload_lease_resource_changed", s.clientIP(c), fmt.Sprintf("票据 #%d，目录 %s", lease.ID, lease.DirID))
		return fiber.NewError(fiber.StatusForbidden, "上传票据绑定的资源已变化，请重新创建上传票据。")
	}
	ownerType, ownerID := uploadLeaseOwner(lease)
	permitID, err := s.acquireUploadPermit(c, dir.ID, lease.ResourceFingerprint, ownerType, ownerID)
	if err != nil {
		s.criticalAudit("upload_lease_failed", s.clientIP(c), fmt.Sprint(lease.ID))
		return err
	}
	defer s.transfers.releaseUploadPermit(permitID)
	resp, err := s.saveStreamingMultipart(c, streamUploadOptions{dir: dir, rel: lease.Path, source: "upload_lease", ownerType: ownerType, ownerID: ownerID, permitID: permitID, transferID: uploadLeaseTransferID(lease.ID), lease: &lease, fixedFileName: lease.FileName, expectedSize: lease.FileSize, expectedFingerprint: lease.ResourceFingerprint})
	if err != nil {
		s.criticalAudit("upload_lease_failed", s.clientIP(c), fmt.Sprint(lease.ID))
		return err
	}
	_ = s.store.Audit("upload_lease_use", s.clientIP(c), fmt.Sprintf("票据 #%d", lease.ID))
	return c.JSON(resp)
}

func (s *Server) uploadRawByLease(c *fiber.Ctx) error {
	resp, err := s.handleRawUploadByLease(c, "session")
	if err != nil {
		return err
	}
	return c.JSON(resp)
}

func (s *Server) publicUploadRawByLease(c *fiber.Ctx) error {
	resp, err := s.handleRawUploadByLease(c, "public_token")
	if err != nil {
		return err
	}
	return c.JSON(resp)
}

func (s *Server) handleRawUploadByLease(c *fiber.Ctx, wantSource string) (uploadResponse, error) {
	plain := bearerToken(c.Get("Authorization"))
	if plain == "" {
		return uploadResponse{}, fiber.ErrUnauthorized
	}
	hash := security.HashToken(plain)
	now := time.Now()
	lease, err := s.store.UploadLeaseByHash(hash)
	if err != nil {
		return uploadResponse{}, fiber.ErrUnauthorized
	}
	if lease.Source == "" {
		lease.Source = "session"
	}
	if lease.Source != wantSource || lease.UsedAt.Valid || !now.Before(lease.ExpiresAt) {
		return uploadResponse{}, fiber.ErrUnauthorized
	}
	dir, ok := s.cfg().Dir(lease.DirID)
	if !ok || !dir.AllowUpload {
		return uploadResponse{}, fiber.ErrForbidden
	}
	if !resourceAuthorizationFingerprintMatches(lease.ResourceFingerprint, dir) {
		s.criticalAudit("upload_lease_resource_changed", s.clientIP(c), fmt.Sprintf("票据 #%d，目录 %s", lease.ID, lease.DirID))
		return uploadResponse{}, fiber.NewError(fiber.StatusForbidden, "上传票据绑定的资源已变化，请重新创建上传票据。")
	}
	contentLength := int64(c.Request().Header.ContentLength())
	if contentLength < 0 {
		return uploadResponse{}, fiber.NewError(fiber.StatusLengthRequired, "原始上传需要 Content-Length。")
	}
	if contentLength != lease.FileSize {
		return uploadResponse{}, fiber.NewError(fiber.StatusBadRequest, "上传内容大小与票据不一致。")
	}
	lease, err = s.store.ReserveUploadLease(hash, now)
	if err != nil {
		return uploadResponse{}, fiber.ErrUnauthorized
	}
	dir, ok = s.cfg().Dir(lease.DirID)
	if !ok || !dir.AllowUpload || !resourceAuthorizationFingerprintMatches(lease.ResourceFingerprint, dir) {
		s.criticalAudit("upload_lease_resource_changed", s.clientIP(c), fmt.Sprintf("票据 #%d，目录 %s", lease.ID, lease.DirID))
		return uploadResponse{}, fiber.NewError(fiber.StatusForbidden, "上传票据绑定的资源已变化，请重新创建上传票据。")
	}
	var reservedToken store.Token
	publicReserved := false
	currentDir := dir
	ownerType, ownerID := uploadLeaseOwner(lease)
	permitID, err := s.acquireUploadPermit(c, currentDir.ID, lease.ResourceFingerprint, ownerType, ownerID)
	if err != nil {
		s.criticalAudit("upload_lease_failed", s.clientIP(c), fmt.Sprint(lease.ID))
		return uploadResponse{}, err
	}
	defer s.transfers.releaseUploadPermit(permitID)
	if wantSource == "public_token" {
		if !lease.TokenID.Valid {
			return uploadResponse{}, fiber.ErrUnauthorized
		}
		reservedToken, err = s.store.ReserveTokenUseByID(lease.TokenID.Int64, "upload", time.Now(), contentLength, s.tokenUploadMaxBytes())
		if err != nil {
			if errors.Is(err, store.ErrTokenUploadLimitExceeded) {
				s.criticalAudit("token_upload_denied", s.clientIP(c), fmt.Sprintf("上传票据 #%d 公开令牌容量不足", lease.ID))
				return uploadResponse{}, fiber.NewError(fiber.StatusRequestEntityTooLarge, "公开上传令牌剩余容量不足。")
			}
			return uploadResponse{}, fiber.ErrForbidden
		}
		currentDir, ok = s.cfg().Dir(lease.DirID)
		if !ok || !currentDir.AllowUpload || reservedToken.DirID != lease.DirID || reservedToken.Path != lease.Path || !resourceAuthorizationFingerprintMatches(lease.ResourceFingerprint, currentDir) || !resourceAuthorizationFingerprintMatches(reservedToken.ResourceFingerprint, currentDir) {
			s.releaseTokenUse(reservedToken.ID, contentLength, "public raw resource recheck")
			return uploadResponse{}, fiber.ErrForbidden
		}
		publicReserved = true
	}
	resp, err := s.saveRawUpload(c, currentDir, lease, permitID)
	if err != nil {
		if publicReserved {
			s.releaseTokenUse(reservedToken.ID, contentLength, "public raw upload failure")
		}
		s.criticalAudit("upload_lease_failed", s.clientIP(c), fmt.Sprint(lease.ID))
		return uploadResponse{}, err
	}
	if publicReserved {
		actualBytes := uploadResponseBytes(resp)
		if err := s.store.AdjustTokenUploadedBytes(reservedToken.ID, actualBytes-contentLength, s.tokenUploadMaxBytes()); err != nil {
			cleanupUploadedResponse(currentDir, resp)
			s.releaseTokenUse(reservedToken.ID, contentLength, "public raw byte adjustment")
			if errors.Is(err, store.ErrTokenUploadLimitExceeded) {
				s.criticalAudit("token_upload_denied", s.clientIP(c), fmt.Sprintf("公开令牌 #%d 容量调整失败", reservedToken.ID))
				return uploadResponse{}, fiber.NewError(fiber.StatusRequestEntityTooLarge, "公开上传令牌剩余容量不足。")
			}
			return uploadResponse{}, err
		}
	}
	_ = s.store.Audit("upload_lease_use", s.clientIP(c), fmt.Sprintf("票据 #%d", lease.ID))
	return resp, nil
}

func (s *Server) saveRawUpload(c *fiber.Ctx, dir config.Dir, lease store.UploadLease, permitID string) (uploadResponse, error) {
	if err := os.MkdirAll(dir.Path, 0755); err != nil {
		return uploadResponse{}, friendlyPathError(err, "上传目录不存在或不可访问。")
	}
	targetDir, safeRel, err := fsutil.ResolveForCreate(dir.Path, lease.Path)
	if err != nil {
		s.criticalAudit("illegal_access", s.clientIP(c), fmt.Sprintf("目录 %s 上传路径校验失败", dir.ID))
		return uploadResponse{}, fiber.ErrBadRequest
	}
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return uploadResponse{}, err
	}
	if err := fsutil.EnsureInside(dir.Path, targetDir); err != nil {
		return uploadResponse{}, friendlyPathError(err, "上传目录不存在或不可访问。")
	}
	body := c.Request().BodyStream()
	if body == nil {
		if lease.FileSize == 0 {
			body = bytes.NewReader(nil)
		} else {
			return uploadResponse{}, fiber.NewError(fiber.StatusBadRequest, "原始上传请求流不可用，请使用直连后端上传。")
		}
	}
	ownerType, ownerID := uploadLeaseOwner(lease)
	dst, size, err := s.saveRawUniqueAtomic(c, targetDir, safeRel, lease.FileName, body, lease.Source, ownerType, ownerID, permitID, uploadLeaseTransferID(lease.ID), dir.ID, lease.FileSize, lease.ResourceFingerprint)
	if err != nil {
		return uploadResponse{}, err
	}
	name := filepath.Base(dst)
	resp := uploadResponse{OK: true, Uploaded: 1, Files: []uploadedFile{{Name: name, Path: filepath.ToSlash(filepath.Join(safeRel, name)), Size: size}}}
	_ = s.store.Audit("upload", s.clientIP(c), fmt.Sprintf("目录 %s，路径 %s，上传 1 个文件", dir.ID, displayPath(safeRel)))
	return resp, nil
}

func (s *Server) registerUploadTransfer(c *fiber.Ctx, rec *transferRecord, expectedFingerprint string) error {
	if s.beforeUploadTransferRegister != nil {
		s.beforeUploadTransferRegister()
	}
	s.transferGateMu.RLock()
	defer s.transferGateMu.RUnlock()
	currentDir, ok := s.cfg().Dir(rec.DirID)
	if !ok || !currentDir.AllowUpload || !resourceAuthorizationFingerprintMatches(expectedFingerprint, currentDir) {
		return fiber.NewError(fiber.StatusForbidden, "上传资源已变化，请重新开始上传。")
	}
	if !s.transfers.hasBoundUploadPermit(rec.PermitID, rec.DirID, expectedFingerprint) {
		return s.uploadAdmissionError(c, errUploadScopedCapacity)
	}
	s.transfers.add(rec)
	return nil
}

func (s *Server) acquireUploadPermit(c *fiber.Ctx, dirID, expectedFingerprint, ownerType, ownerID string) (string, error) {
	id, _, err := security.NewToken()
	if err != nil {
		return "", err
	}
	conn := c.Context().Conn()
	permit := &uploadPermit{ID: id, DirID: dirID, ResourceFingerprint: expectedFingerprint, OwnerType: ownerType, OwnerID: ownerID, cancel: func() {
		if conn != nil {
			_ = conn.SetReadDeadline(time.Now())
			_ = conn.Close()
		}
	}}
	s.transferGateMu.RLock()
	defer s.transferGateMu.RUnlock()
	if dirID != "" {
		currentDir, ok := s.cfg().Dir(dirID)
		if !ok || !currentDir.AllowUpload || !resourceAuthorizationFingerprintMatches(expectedFingerprint, currentDir) {
			return "", fiber.NewError(fiber.StatusForbidden, "上传资源已变化，请重新开始上传。")
		}
	}
	limits := s.cfg().Abuse.Uploads
	if err := s.transfers.tryAcquireUploadPermit(permit, uploadAdmissionLimits{Global: limits.Global, PerResource: limits.PerResource, PerSession: limits.PerSession, PerToken: limits.PerToken}); err != nil {
		return "", s.uploadAdmissionError(c, err)
	}
	return id, nil
}

func (s *Server) bindUploadPermit(c *fiber.Ctx, permitID string, dir config.Dir, expectedFingerprint string) error {
	s.transferGateMu.RLock()
	defer s.transferGateMu.RUnlock()
	currentDir, ok := s.cfg().Dir(dir.ID)
	if !ok || !currentDir.AllowUpload || !resourceAuthorizationFingerprintMatches(expectedFingerprint, currentDir) {
		return fiber.NewError(fiber.StatusForbidden, "上传资源已变化，请重新开始上传。")
	}
	if err := s.transfers.bindUploadPermit(permitID, dir.ID, expectedFingerprint, s.cfg().Abuse.Uploads.PerResource); err != nil {
		return s.uploadAdmissionError(c, err)
	}
	return nil
}

func (s *Server) uploadAdmissionError(c *fiber.Ctx, err error) error {
	c.Set("Retry-After", "5")
	if errors.Is(err, errUploadGlobalCapacity) {
		return newCodedAPIError(fiber.StatusServiceUnavailable, "upload_capacity_exhausted", "上传并发容量已满，请稍后重试。")
	}
	return newCodedAPIError(fiber.StatusTooManyRequests, "upload_concurrency_limited", "当前资源或凭据的并发上传已达上限。")
}

func (s *Server) commitUploadCandidate(ctx context.Context, permitID, dirID, expectedFingerprint, stagingPath, destinationPath string) (bool, error) {
	if s.beforeUploadFinalCommit != nil {
		s.beforeUploadFinalCommit()
	}
	s.transferGateMu.RLock()
	defer s.transferGateMu.RUnlock()
	if ctx != nil {
		select {
		case <-ctx.Done():
			return false, fiber.NewError(fiber.StatusRequestTimeout, "上传已取消。")
		default:
		}
	}
	if !s.transfers.hasBoundUploadPermit(permitID, dirID, expectedFingerprint) {
		return false, fiber.NewError(fiber.StatusForbidden, "上传资源已变化，请重新开始上传。")
	}
	currentDir, ok := s.cfg().Dir(dirID)
	if !ok || !currentDir.AllowUpload || !resourceAuthorizationFingerprintMatches(expectedFingerprint, currentDir) {
		return false, fiber.NewError(fiber.StatusForbidden, "上传资源已变化，请重新开始上传。")
	}
	return promoteUploadNoReplace(stagingPath, destinationPath)
}

func (s *Server) checkUploadDiskReserve(c *fiber.Ctx, path string, declaredSize int64) error {
	checker := s.availableDiskSpace
	if checker == nil {
		checker = fsutil.AvailableDiskSpace
	}
	available, total, err := checker(path)
	if err != nil || total == 0 || available > total {
		c.Set("Retry-After", "60")
		return newCodedAPIError(fiber.StatusServiceUnavailable, "storage_reserve_unavailable", "存储空间暂时不可用，请稍后重试。")
	}
	storage := s.cfg().Storage
	reserveMB := saturatingUintMultiply(uint64(storage.MinFreeMB), 1024*1024)
	percentReserve := uint64(0)
	if storage.MinFreePercent > 0 {
		percent := uint64(storage.MinFreePercent)
		percentReserve = total/100*percent + total%100*percent/100
	}
	reserve := reserveMB
	if percentReserve > reserve {
		reserve = percentReserve
	}
	declared := uint64(0)
	if declaredSize > 0 {
		declared = uint64(declaredSize)
	}
	if available < reserve || declared > available-reserve {
		c.Set("Retry-After", "60")
		return newCodedAPIError(fiber.StatusServiceUnavailable, "storage_reserve_unavailable", "存储空间不足以安全开始上传，请稍后重试。")
	}
	return nil
}

func saturatingUintMultiply(left, right uint64) uint64 {
	if left == 0 || right == 0 {
		return 0
	}
	if left > ^uint64(0)/right {
		return ^uint64(0)
	}
	return left * right
}

func (s *Server) activeTransfers(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"transfers": s.transfers.list()})
}

func (s *Server) cancelTransfer(c *fiber.Ctx) error {
	if s.transfers.cancel(c.Params("id")) {
		return c.JSON(fiber.Map{"ok": true})
	}
	return fiber.NewError(fiber.StatusConflict, "该传输不可可靠取消或已结束。")
}

type streamUploadOptions struct {
	dir                     config.Dir
	rel                     string
	source                  string
	ownerType               string
	ownerID                 string
	permitID                string
	transferID              string
	requireTargetBeforeFile bool
	lease                   *store.UploadLease
	fixedFileName           string
	expectedSize            int64
	expectedFingerprint     string
}

// testMultipartBodyFallback is nil in production. Fiber's in-memory app.Test transport does not expose
// a request body stream, so package tests install a test-only reader without changing production routes.
var testMultipartBodyFallback func(*fiber.Ctx) io.Reader

func (s *Server) saveStreamingMultipart(c *fiber.Ctx, opts streamUploadOptions) (uploadResponse, error) {
	if opts.permitID == "" {
		return uploadResponse{}, s.uploadAdmissionError(c, errUploadScopedCapacity)
	}
	if opts.dir.ID != "" && !opts.dir.AllowUpload {
		return uploadResponse{}, fiber.ErrForbidden
	}
	mediaType, params, err := mime.ParseMediaType(c.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") || params["boundary"] == "" {
		return uploadResponse{}, fiber.NewError(fiber.StatusBadRequest, "上传请求格式不正确，请重新选择文件后再试。")
	}
	body := c.Request().BodyStream()
	if body == nil && testMultipartBodyFallback != nil {
		body = testMultipartBodyFallback(c)
	}
	if body == nil {
		return uploadResponse{}, fiber.NewError(fiber.StatusBadRequest, "上传请求流不可用，请使用直连后端上传。")
	}
	reader := multipart.NewReader(body, params["boundary"])
	targetReady := false
	resolveTarget := func(rel string) (string, string, error) {
		if opts.dir.ID == "" {
			return "", "", fiber.NewError(fiber.StatusBadRequest, "请把 dirId 放在查询参数中，或放在文件字段之前。")
		}
		if !opts.dir.AllowUpload {
			return "", "", fiber.ErrForbidden
		}
		if err := os.MkdirAll(opts.dir.Path, 0755); err != nil {
			return "", "", friendlyPathError(err, "上传目录不存在或不可访问。")
		}
		targetDir, safeRel, err := fsutil.ResolveForCreate(opts.dir.Path, rel)
		if err != nil {
			s.criticalAudit("illegal_access", s.clientIP(c), fmt.Sprintf("目录 %s 上传路径校验失败", opts.dir.ID))
			return "", "", fiber.ErrBadRequest
		}
		if err := os.MkdirAll(targetDir, 0755); err != nil {
			return "", "", err
		}
		if err := fsutil.EnsureInside(opts.dir.Path, targetDir); err != nil {
			s.criticalAudit("illegal_access", s.clientIP(c), fmt.Sprintf("目录 %s 上传路径校验失败", opts.dir.ID))
			return "", "", friendlyPathError(err, "上传目录不存在或不可访问。")
		}
		targetReady = true
		return targetDir, safeRel, nil
	}
	targetDir, safeRel := "", ""
	if opts.dir.ID != "" {
		var err error
		targetDir, safeRel, err = resolveTarget(opts.rel)
		if err != nil {
			return uploadResponse{}, err
		}
	}
	resp := uploadResponse{OK: true}
	saved := []string{}
	var total int64
	fileCount := 0
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanupPaths(saved)
			return uploadResponse{}, fiber.NewError(fiber.StatusBadRequest, "上传请求读取失败，请重试。")
		}
		fieldName := part.FormName()
		fileName := part.FileName()
		if fileName == "" {
			value, _ := io.ReadAll(io.LimitReader(part, 4097))
			if len(value) > 4096 {
				cleanupPaths(saved)
				return uploadResponse{}, fiber.NewError(fiber.StatusRequestEntityTooLarge, "上传表单字段过大。")
			}
			total += int64(len(value))
			if total > mbToBytes(s.cfg().Storage.UploadMaxMB) {
				cleanupPaths(saved)
				return uploadResponse{}, fiber.NewError(fiber.StatusRequestEntityTooLarge, fmt.Sprintf("单次上传总量不能超过 %d MB", s.cfg().Storage.UploadMaxMB))
			}
			if fileCount > 0 && opts.requireTargetBeforeFile && (fieldName == "dirId" || fieldName == "path") {
				cleanupPaths(saved)
				return uploadResponse{}, fiber.NewError(fiber.StatusBadRequest, "请把上传目录和路径放在请求参数中，不能放在文件内容之后。")
			}
			if opts.lease == nil {
				switch fieldName {
				case "dirId":
					if fileCount == 0 && c.Query("dirId") == "" {
						d, err := s.dirByID(strings.TrimSpace(string(value)))
						if err != nil {
							return uploadResponse{}, err
						}
						opts.dir = d
						opts.expectedFingerprint = resourceAuthorizationFingerprint(d)
						if err := s.bindUploadPermit(c, opts.permitID, d, opts.expectedFingerprint); err != nil {
							return uploadResponse{}, err
						}
						targetDir, safeRel, err = resolveTarget(opts.rel)
						if err != nil {
							return uploadResponse{}, err
						}
					}
				case "path":
					if fileCount == 0 && c.Query("path") == "" {
						opts.rel = string(value)
						if opts.dir.ID != "" {
							targetDir, safeRel, err = resolveTarget(opts.rel)
						}
						if err != nil {
							return uploadResponse{}, err
						}
					}
				}
			}
			_ = part.Close()
			continue
		}
		if !targetReady {
			cleanupPaths(saved)
			return uploadResponse{}, fiber.NewError(fiber.StatusBadRequest, "请把上传目录和路径放在查询参数中，或放在文件字段之前。")
		}
		fileCount++
		if fileCount > s.cfg().Storage.UploadMaxFiles || opts.lease != nil && fileCount > 1 {
			cleanupPaths(saved)
			return uploadResponse{}, fiber.NewError(fiber.StatusRequestEntityTooLarge, fmt.Sprintf("一次最多上传 %d 个文件", s.cfg().Storage.UploadMaxFiles))
		}
		if opts.fixedFileName != "" {
			fileName = opts.fixedFileName
		}
		safeName := fsutil.SafeName(fileName)
		if safeName == "" || !s.extensionAllowed(safeName) {
			cleanupPaths(saved)
			return uploadResponse{}, fiber.NewError(fiber.StatusForbidden, "该文件扩展名不允许上传")
		}
		diskSize := opts.expectedSize
		if diskSize < 0 {
			diskSize = int64(c.Request().Header.ContentLength()) - total
			if diskSize < 0 {
				diskSize = mbToBytes(s.cfg().Storage.UploadMaxFileMB)
			}
		}
		if err := s.checkUploadDiskReserve(c, targetDir, diskSize); err != nil {
			cleanupPaths(saved)
			return uploadResponse{}, err
		}
		dst, size, err := s.savePartUniqueAtomic(c, targetDir, safeRel, safeName, part, opts.source, opts.ownerType, opts.ownerID, opts.permitID, opts.transferID, opts.dir.ID, opts.expectedSize, diskSize, opts.expectedFingerprint)
		_ = part.Close()
		if err != nil {
			cleanupPaths(saved)
			return uploadResponse{}, err
		}
		total += size
		if total > mbToBytes(s.cfg().Storage.UploadMaxMB) {
			_ = os.Remove(dst)
			cleanupPaths(saved)
			return uploadResponse{}, fiber.NewError(fiber.StatusRequestEntityTooLarge, fmt.Sprintf("单次上传总量不能超过 %d MB", s.cfg().Storage.UploadMaxMB))
		}
		saved = append(saved, dst)
		finalName := filepath.Base(dst)
		resp.Files = append(resp.Files, uploadedFile{Name: finalName, Path: filepath.ToSlash(filepath.Join(safeRel, finalName)), Size: size})
	}
	if fileCount == 0 {
		return uploadResponse{}, fiber.ErrBadRequest
	}
	resp.Uploaded = len(resp.Files)
	_ = s.store.Audit("upload", s.clientIP(c), fmt.Sprintf("目录 %s，路径 %s，上传 %d 个文件", opts.dir.ID, displayPath(safeRel), resp.Uploaded))
	return resp, nil
}

func (s *Server) savePartUniqueAtomic(c *fiber.Ctx, dir, rel, name string, part *multipart.Part, source, ownerType, ownerID, permitID, transferID, dirID string, expectedSize, diskSize int64, expectedFingerprint string) (string, int64, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	maxBytes := mbToBytes(s.cfg().Storage.UploadMaxFileMB)
	if expectedSize >= 0 && expectedSize < maxBytes {
		maxBytes = expectedSize
	}
	if err := s.checkUploadDiskReserve(c, dir, diskSize); err != nil {
		return "", 0, err
	}
	tmp, err := os.CreateTemp(dir, ".upload-*.tmp")
	if err != nil {
		return "", 0, err
	}
	tmpName := tmp.Name()
	ctx, cancel := context.WithCancel(context.Background())
	id := transferID
	if id == "" {
		id, _, _ = security.NewToken()
	}
	conn := c.Context().Conn()
	rec := &transferRecord{ID: id, Type: "upload", Status: transferActive, Source: source, OwnerType: ownerType, OwnerID: ownerID, PermitID: permitID, DirID: dirID, Path: rel, FileName: name, TotalBytes: expectedSize, ClientIP: s.clientIP(c), Cancelable: true, TempPath: tmpName, cancel: func() {
		cancel()
		// 管理员取消必须能打断正在阻塞的 multipart 读取；仅取消自建 context 不足以唤醒 part.Read。
		_ = tmp.Close()
		if conn != nil {
			_ = conn.SetReadDeadline(time.Now())
			_ = conn.Close()
		}
	}}
	if err := s.registerUploadTransfer(c, rec, expectedFingerprint); err != nil {
		cancel()
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", 0, err
	}
	defer func() {
		cancel()
		s.transfers.remove(id)
		_ = os.Remove(tmpName)
	}()

	buf := make([]byte, 256*1024)
	var written int64
	for {
		select {
		case <-ctx.Done():
			_ = tmp.Close()
			return "", written, fiber.NewError(fiber.StatusRequestTimeout, "上传已取消。")
		case <-c.Context().Done():
			_ = tmp.Close()
			return "", written, fiber.NewError(fiber.StatusRequestTimeout, "上传连接已断开。")
		default:
		}
		n, readErr := part.Read(buf)
		if n > 0 {
			written += int64(n)
			if written > maxBytes {
				_ = tmp.Close()
				return "", written, fiber.NewError(fiber.StatusRequestEntityTooLarge, fmt.Sprintf("单个文件不能超过 %d MB", s.cfg().Storage.UploadMaxFileMB))
			}
			if _, err := tmp.Write(buf[:n]); err != nil {
				_ = tmp.Close()
				return "", written, err
			}
			now := time.Now()
			s.transfers.update(id, func(r *transferRecord) {
				deltaBytes := written - r.lastBytes
				deltaSeconds := now.Sub(r.lastSpeedAt).Seconds()
				if deltaSeconds > 0 {
					r.CurrentSpeedBps = int64(float64(deltaBytes) / deltaSeconds)
				}
				avgSeconds := now.Sub(r.StartedAt).Seconds()
				if avgSeconds > 0 {
					r.AverageSpeedBps = int64(float64(written) / avgSeconds)
				}
				r.TransferredBytes = written
				r.UpdatedAt = now
				r.lastBytes = written
				r.lastSpeedAt = now
			})
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = tmp.Close()
			return "", written, readErr
		}
	}
	if expectedSize >= 0 && written != expectedSize {
		_ = tmp.Close()
		return "", written, fiber.NewError(fiber.StatusBadRequest, "上传内容大小与票据不一致。")
	}
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return "", written, err
	}
	if err := tmp.Close(); err != nil {
		return "", written, err
	}
	for i := 0; i < maxUploadNameAttempts; i++ {
		candidateName := name
		if i > 0 {
			candidateName = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		dst := filepath.Join(dir, candidateName)
		done, err := s.commitUploadCandidate(ctx, permitID, dirID, expectedFingerprint, tmpName, dst)
		if err != nil {
			return "", written, err
		}
		if done {
			return dst, written, nil
		}
	}
	return "", written, fiber.NewError(fiber.StatusConflict, "同名文件过多，请更换文件名后重试。")
}

func (s *Server) saveRawUniqueAtomic(c *fiber.Ctx, dir, rel, name string, reader io.Reader, source, ownerType, ownerID, permitID, transferID, dirID string, expectedSize int64, expectedFingerprint string) (string, int64, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if err := s.checkUploadDiskReserve(c, dir, expectedSize); err != nil {
		return "", 0, err
	}
	tmp, err := os.CreateTemp(dir, ".upload-*.tmp")
	if err != nil {
		return "", 0, err
	}
	tmpName := tmp.Name()
	ctx, cancel := context.WithCancel(context.Background())
	id := transferID
	if id == "" {
		id, _, _ = security.NewToken()
	}
	conn := c.Context().Conn()
	rec := &transferRecord{ID: id, Type: "upload", Status: transferActive, Source: source, OwnerType: ownerType, OwnerID: ownerID, PermitID: permitID, DirID: dirID, Path: rel, FileName: name, TotalBytes: expectedSize, ClientIP: s.clientIP(c), Cancelable: true, TempPath: tmpName, cancel: func() {
		cancel()
		_ = tmp.Close()
		if conn != nil {
			_ = conn.SetReadDeadline(time.Now())
			_ = conn.Close()
		}
	}}
	if err := s.registerUploadTransfer(c, rec, expectedFingerprint); err != nil {
		cancel()
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", 0, err
	}
	defer func() {
		cancel()
		s.transfers.remove(id)
		_ = os.Remove(tmpName)
	}()

	buf := make([]byte, 256*1024)
	var written int64
	for {
		select {
		case <-ctx.Done():
			_ = tmp.Close()
			return "", written, fiber.NewError(fiber.StatusRequestTimeout, "上传已取消。")
		case <-c.Context().Done():
			_ = tmp.Close()
			return "", written, fiber.NewError(fiber.StatusRequestTimeout, "上传连接已断开。")
		default:
		}
		n, readErr := reader.Read(buf)
		if n > 0 {
			written += int64(n)
			if expectedSize >= 0 && written > expectedSize {
				_ = tmp.Close()
				return "", written, fiber.NewError(fiber.StatusBadRequest, "上传内容大小与票据不一致。")
			}
			if written > mbToBytes(s.cfg().Storage.UploadMaxFileMB) {
				_ = tmp.Close()
				return "", written, fiber.NewError(fiber.StatusRequestEntityTooLarge, fmt.Sprintf("单个文件不能超过 %d MB", s.cfg().Storage.UploadMaxFileMB))
			}
			if _, err := tmp.Write(buf[:n]); err != nil {
				_ = tmp.Close()
				return "", written, err
			}
			now := time.Now()
			s.transfers.update(id, func(r *transferRecord) {
				deltaBytes := written - r.lastBytes
				deltaSeconds := now.Sub(r.lastSpeedAt).Seconds()
				if deltaSeconds > 0 {
					r.CurrentSpeedBps = int64(float64(deltaBytes) / deltaSeconds)
				}
				avgSeconds := now.Sub(r.StartedAt).Seconds()
				if avgSeconds > 0 {
					r.AverageSpeedBps = int64(float64(written) / avgSeconds)
				}
				r.TransferredBytes = written
				r.UpdatedAt = now
				r.lastBytes = written
				r.lastSpeedAt = now
			})
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = tmp.Close()
			return "", written, readErr
		}
	}
	if expectedSize >= 0 && written != expectedSize {
		_ = tmp.Close()
		return "", written, fiber.NewError(fiber.StatusBadRequest, "上传内容大小与票据不一致。")
	}
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return "", written, err
	}
	if err := tmp.Close(); err != nil {
		return "", written, err
	}
	for i := 0; i < maxUploadNameAttempts; i++ {
		candidateName := name
		if i > 0 {
			candidateName = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		dst := filepath.Join(dir, candidateName)
		done, err := s.commitUploadCandidate(ctx, permitID, dirID, expectedFingerprint, tmpName, dst)
		if err != nil {
			return "", written, err
		}
		if done {
			return dst, written, nil
		}
	}
	return "", written, fiber.NewError(fiber.StatusConflict, "同名文件过多，请更换文件名后重试。")
}

func cleanupPaths(paths []string) {
	for _, p := range paths {
		_ = os.Remove(p)
	}
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
			if t.ResourceFingerprint == "" {
				dto.Valid = false
				dto.Reason = "resource_binding_invalid"
			} else {
				dir, ok := s.cfg().Dir(t.DirID)
				if !ok {
					dto.Valid = false
					dto.Reason = "resource_unavailable"
				} else if !resourceAuthorizationFingerprintMatches(t.ResourceFingerprint, dir) {
					dto.Valid = false
					dto.Reason = "resource_binding_invalid"
				} else if t.Type == "download" && !dir.AllowDownload || t.Type == "upload" && !dir.AllowUpload {
					dto.Valid = false
					dto.Reason = "permission_disabled"
				}
			}
		}
		out = append(out, dto)
	}
	return c.JSON(out)
}

func (s *Server) createToken(c *fiber.Ctx) error {
	if err := s.checkTokenCreationRate(c, fmt.Sprint(c.Locals("sessionID"))); err != nil {
		return err
	}
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
	if in.Type == "download" {
		// 下载令牌必须提前确认普通文件存在，避免对外发出不可用链接或特殊设备路径。
		fullPath, safePath, _, err = s.resolveDownloadFile(dir, in.Path)
	} else {
		// 上传令牌允许未来创建子目录，但必须确保最近存在父目录没有符号链接逃逸。
		fullPath, safePath, err = fsutil.ResolveForCreate(dir.Path, in.Path)
	}
	if err != nil {
		if in.Type == "download" {
			return fiber.NewError(fiber.StatusNotFound, "下载文件不存在，请先在文件浏览页确认文件路径。")
		}
		return friendlyPathError(err, "路径不存在，请先在文件浏览页确认后再创建令牌。")
	}
	if in.Type != "download" {
		if err := ensureUploadTokenTarget(fullPath); err != nil {
			return err
		}
	}
	plain, hash, err := security.NewToken()
	if err != nil {
		return err
	}
	t := &store.Token{Hash: hash, Type: in.Type, DirID: dirID, Path: safePath, ResourceFingerprint: resourceAuthorizationFingerprint(dir), MaxUses: maxInt(in.MaxUses, in.MaxUsesOld), ExpiresAt: tokenExpiry(s.cfg(), in)}
	if err := s.store.CreateTokenLimited(t, s.cfg().Abuse.Creation.MaxActiveTokens); err != nil {
		return s.creationStoreError(c, err)
	}
	base := "/t/" + url.PathEscape(plain)
	publicURL := base + "/download"
	if in.Type == "upload" {
		publicURL = base + "/upload"
	}
	s.criticalAudit("token_create", s.clientIP(c), fmt.Sprintf("创建%s令牌 #%d", tokenTypeLabel(t.Type), t.ID))
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
	s.criticalAudit("token_revoke", s.clientIP(c), "撤销令牌 #"+c.Params("id"))
	return c.JSON(fiber.Map{"ok": true})
}

func (s *Server) deleteToken(c *fiber.Ctx) error {
	if err := s.store.DeleteTokenAndLeases(c.Params("id")); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fiber.ErrNotFound
		}
		return err
	}
	s.criticalAudit("token_delete", s.clientIP(c), "删除令牌 #"+c.Params("id"))
	return c.JSON(fiber.Map{"ok": true})
}

func (s *Server) auditLogs(c *fiber.Ctx) error {
	keyword := strings.TrimSpace(c.Query("keyword"))
	status := strings.ToLower(strings.TrimSpace(c.Query("status", "all")))
	if utf8.RuneCountInString(keyword) > 200 || status != "all" && status != "ok" && status != "failed" {
		return newCodedAPIError(fiber.StatusBadRequest, "audit_filter_invalid", "审计筛选参数无效。")
	}
	filter := store.AuditLogFilter{Keyword: keyword, Status: status}
	if c.Query("page") != "" || c.Query("pageSize") != "" {
		pageValue, err := strconv.ParseInt(c.Query("page", "1"), 10, 64)
		if err != nil || pageValue < 1 {
			return newCodedAPIError(fiber.StatusBadRequest, "audit_page_out_of_range", "审计页码超出允许范围。")
		}
		pageSize, err := strconv.Atoi(c.Query("pageSize", "50"))
		if err != nil || pageSize < 1 {
			pageSize = 50
		}
		if pageSize > 200 {
			pageSize = 200
		}
		maxInt := int64(^uint(0) >> 1)
		if pageValue-1 > maxInt/int64(pageSize) {
			return newCodedAPIError(fiber.StatusBadRequest, "audit_page_out_of_range", "审计页码超出允许范围。")
		}
		offset := (pageValue - 1) * int64(pageSize)
		page := int(pageValue)
		logs, total, err := s.store.AuditLogsPageFiltered(pageSize, int(offset), filter)
		if err != nil {
			return err
		}
		out := make([]auditDTO, 0, len(logs))
		for _, l := range logs {
			out = append(out, auditDTOFromStore(l))
		}
		totalPages := 0
		if total > 0 {
			totalPages = (total-1)/pageSize + 1
		}
		return c.JSON(auditPageDTO{Logs: out, Page: page, PageSize: pageSize, Total: total, TotalPages: totalPages})
	}
	limit, err := strconv.Atoi(c.Query("limit", "100"))
	if err != nil || limit < 1 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	logs, _, err := s.store.AuditLogsPageFiltered(limit, 0, filter)
	if err != nil {
		return err
	}
	out := make([]auditDTO, 0, len(logs))
	for _, l := range logs {
		out = append(out, auditDTOFromStore(l))
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
	if valid && t.ResourceFingerprint == "" {
		valid = false
		reason = "resource_binding_invalid"
	}
	if valid {
		dir, ok := s.cfg().Dir(t.DirID)
		if !ok {
			valid = false
			reason = "resource_unavailable"
		} else if !resourceAuthorizationFingerprintMatches(t.ResourceFingerprint, dir) {
			valid = false
			reason = "resource_binding_invalid"
		} else if t.Type == "download" && !dir.AllowDownload || t.Type == "upload" && !dir.AllowUpload {
			valid = false
			reason = "permission_disabled"
		}
	}
	if !valid {
		return c.JSON(fiber.Map{"valid": false, "reason": reason})
	}
	cfg := s.cfg()
	out := fiber.Map{"valid": true, "type": t.Type, "path": t.Path, "expiresAt": nil, "maxUses": t.MaxUses, "uses": t.Uses, "uploadedBytes": t.UploadedBytes, "uploadMaxBytes": s.tokenUploadMaxBytes(), "uploadMaxFileBytes": mbToBytes(cfg.Storage.UploadMaxFileMB), "uploadRequestMaxBytes": mbToBytes(cfg.Storage.UploadMaxMB), "allowedExtensions": cfg.Storage.AllowedExtensions, "blockedExtensions": cfg.Storage.BlockedExtensions}
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

func (s *Server) createPublicUploadLease(c *fiber.Ctx) error {
	tokenHash := security.HashToken(c.Params("token"))
	if err := s.checkLeaseCreationRate(c, "public:"+tokenHash, true); err != nil {
		return err
	}
	var in uploadLeaseRequest
	if err := c.BodyParser(&in); err != nil {
		return fiber.ErrBadRequest
	}
	t, dir, err := s.lookupPublicToken(c, "upload")
	if err != nil {
		return err
	}
	if in.FileSize < 0 || in.FileSize > mbToBytes(s.cfg().Storage.UploadMaxFileMB) || in.FileSize > mbToBytes(s.cfg().Storage.UploadMaxMB) {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, fmt.Sprintf("单个文件不能超过 %d MB", s.cfg().Storage.UploadMaxFileMB))
	}
	if maxBytes := s.tokenUploadMaxBytes(); maxBytes > 0 && t.UploadedBytes+in.FileSize > maxBytes {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, "公开上传令牌剩余容量不足。")
	}
	name := fsutil.SafeName(in.FileName)
	if name == "" || !s.extensionAllowed(name) {
		return fiber.NewError(fiber.StatusForbidden, "该文件扩展名不允许上传")
	}
	if err := os.MkdirAll(dir.Path, 0755); err != nil {
		return friendlyPathError(err, "上传目录不存在或不可访问。")
	}
	_, safeRel, err := fsutil.ResolveForCreate(dir.Path, t.Path)
	if err != nil {
		return friendlyPathError(err, "路径不存在，请先在文件浏览页确认后再创建令牌。")
	}
	if err := s.checkUploadDiskReserve(c, dir.Path, in.FileSize); err != nil {
		return err
	}
	plain, hash, err := security.NewToken()
	if err != nil {
		return err
	}
	currentDir, ok := s.cfg().Dir(t.DirID)
	if !ok || !currentDir.AllowUpload || !resourceAuthorizationFingerprintMatches(t.ResourceFingerprint, currentDir) {
		return fiber.ErrForbidden
	}
	now := time.Now()
	expiresAt := publicLeaseExpiry(now, time.Duration(s.cfg().Auth.UploadLeaseTTLSeconds)*time.Second, t.ExpiresAt)
	lease := store.UploadLease{Hash: hash, Source: "public_token", TokenID: sql.NullInt64{Int64: t.ID, Valid: true}, Role: "public", DirID: currentDir.ID, Path: safeRel, FileName: name, FileSize: in.FileSize, ResourceFingerprint: resourceAuthorizationFingerprint(currentDir), ExpiresAt: expiresAt, CreatedAt: now, ClientIP: s.clientIP(c)}
	limits := s.cfg().Abuse.Creation
	if err := s.store.CreateUploadLeaseLimited(&lease, limits.MaxOutstandingLeasesTotal, limits.MaxOutstandingLeasesOwner); err != nil {
		return s.creationStoreError(c, err)
	}
	s.criticalAudit("upload_lease_create", s.clientIP(c), fmt.Sprintf("公开令牌 #%d，目录 %s，路径 %s，文件 %s", t.ID, dir.ID, displayPath(safeRel), name))
	return c.JSON(uploadLeaseResponse{Lease: plain, UploadURL: "/t/" + url.PathEscape(c.Params("token")) + "/upload", RawUploadURL: "/t/upload-raw-by-lease", ExpiresAt: expiresAt})
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
	fileSHA256, checkedInfo, err := s.downloadLeaseFileHash(c, full, info)
	if err != nil {
		return store.DownloadLease{}, "", err
	}
	reserved, reservedDir, err := s.reservePublicToken(c, "download", 0)
	if err != nil {
		return store.DownloadLease{}, "", err
	}
	if resourceAuthorizationFingerprint(dir) != resourceAuthorizationFingerprint(reservedDir) {
		s.releaseTokenUse(reserved.ID, 0, "public download resource recheck")
		return store.DownloadLease{}, "", fiber.ErrForbidden
	}
	full, info, err = s.revalidateDownloadFile(reservedDir, safePath, checkedInfo)
	if err != nil {
		s.releaseTokenUse(reserved.ID, 0, "public download final file recheck")
		return store.DownloadLease{}, "", err
	}
	_ = full
	lease, plain, err := s.createLeaseRecord(store.DownloadLease{
		Source:              "public_token",
		TokenID:             sql.NullInt64{Int64: reserved.ID, Valid: true},
		DirID:               dir.ID,
		Path:                safePath,
		FileSize:            info.Size(),
		FileMtime:           normalizedFileMtime(info),
		FileSHA256:          fileSHA256,
		ResourceFingerprint: resourceAuthorizationFingerprint(reservedDir),
		ExpiresAt:           publicLeaseExpiry(time.Now(), s.downloadLeaseTTL(), reserved.ExpiresAt),
	})
	if err != nil {
		s.releaseTokenUse(reserved.ID, 0, "public download lease create")
		return lease, "", err
	}
	// 公开下载令牌在兑换下载票据时消耗一次次数；后续 Range 续传只校验票据，不重复扣次数。
	s.criticalAudit("public_download_lease_create", s.clientIP(c), fmt.Sprintf("令牌 #%d，文件 %s", reserved.ID, displayPath(safePath)))
	return lease, plain, nil
}

func (s *Server) publicUpload(c *fiber.Ctx) error {
	// 公开上传先做轻量令牌校验，再解析 multipart，避免无效 token 也能迫使服务端处理大请求体。
	initialToken, initialDir, err := s.lookupPublicToken(c, "upload")
	if err != nil {
		return err
	}
	contentLength := int64(c.Request().Header.ContentLength())
	if contentLength <= 0 {
		return fiber.NewError(fiber.StatusLengthRequired, "公开上传需要请求头 Content-Length，请重新选择文件后再试。")
	}
	if contentLength > mbToBytes(s.cfg().Storage.UploadMaxMB) {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, fmt.Sprintf("单次上传总量不能超过 %d MB", s.cfg().Storage.UploadMaxMB))
	}
	permitID, err := s.acquireUploadPermit(c, initialDir.ID, initialToken.ResourceFingerprint, "token", strconv.FormatInt(initialToken.ID, 10))
	if err != nil {
		return err
	}
	defer s.transfers.releaseUploadPermit(permitID)
	t, dir, err := s.reservePublicToken(c, "upload", contentLength)
	if err != nil {
		return err
	}
	resp, err := s.saveStreamingMultipart(c, streamUploadOptions{dir: dir, rel: t.Path, source: "public_token", ownerType: "token", ownerID: strconv.FormatInt(t.ID, 10), permitID: permitID, expectedSize: -1, expectedFingerprint: t.ResourceFingerprint})
	if err != nil {
		s.releaseTokenUse(t.ID, contentLength, "public upload failure")
		s.criticalAudit("token_upload_failed", s.clientIP(c), fmt.Sprint(t.ID))
		return err
	}
	actualBytes := uploadResponseBytes(resp)
	if err := s.store.AdjustTokenUploadedBytes(t.ID, actualBytes-contentLength, s.tokenUploadMaxBytes()); err != nil {
		cleanupUploadedResponse(dir, resp)
		s.releaseTokenUse(t.ID, contentLength, "public upload byte adjustment")
		s.criticalAudit("token_upload_failed", s.clientIP(c), fmt.Sprint(t.ID))
		if errors.Is(err, store.ErrTokenUploadLimitExceeded) {
			s.criticalAudit("token_upload_denied", s.clientIP(c), fmt.Sprintf("公开令牌 #%d 容量不足", t.ID))
			return fiber.NewError(fiber.StatusRequestEntityTooLarge, "公开上传令牌剩余容量不足。")
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

func (s *Server) releaseTokenUse(id int64, uploadBytes int64, context string) {
	if err := s.store.ReleaseTokenUse(id, uploadBytes); err != nil {
		// 只记录内部 ID 和固定上下文，不记录公开凭据或资源路径。
		log.Printf("[CRITICAL] token reservation rollback failed: context=%s token_id=%d err=%v", context, id, err)
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
		s.criticalAudit("token_denied", s.clientIP(c), "公开令牌不可用或已超出限制")
		if errors.Is(err, store.ErrTokenUploadLimitExceeded) {
			s.criticalAudit("token_upload_denied", s.clientIP(c), fmt.Sprintf("公开令牌 #%d 容量不足", t.ID))
			return t, config.Dir{}, fiber.NewError(fiber.StatusRequestEntityTooLarge, "公开上传令牌剩余容量不足。")
		}
		if errors.Is(err, store.ErrTokenNotUsable) {
			return t, config.Dir{}, fiber.ErrForbidden
		}
		return t, config.Dir{}, err
	}
	dir, ok := s.cfg().Dir(t.DirID)
	if !ok {
		s.releaseTokenUse(t.ID, uploadBytes, "public token missing resource")
		return t, dir, fiber.ErrForbidden
	}
	if !resourceAuthorizationFingerprintMatches(t.ResourceFingerprint, dir) {
		s.releaseTokenUse(t.ID, uploadBytes, "public token resource fingerprint")
		return t, dir, fiber.ErrForbidden
	}
	if tokenType == "download" && !dir.AllowDownload {
		s.releaseTokenUse(t.ID, uploadBytes, "public download permission")
		return t, dir, fiber.ErrForbidden
	}
	if tokenType == "upload" && !dir.AllowUpload {
		s.releaseTokenUse(t.ID, uploadBytes, "public upload permission")
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
	if !resourceAuthorizationFingerprintMatches(t.ResourceFingerprint, dir) {
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
		"file_list":                             "文件列表",
		"dirs":                                  "查看目录",
		"download":                              "文件下载",
		"upload":                                "文件上传",
		"login_success":                         "登录成功",
		"login_failed":                          "登录失败",
		"login_rate_limited":                    "登录限速",
		"logout":                                "退出登录",
		"unauthorized":                          "未认证访问",
		"forbidden":                             "权限不足",
		"illegal_access":                        "非法访问",
		"token_create":                          "创建令牌",
		"token_revoke":                          "撤销令牌",
		"token_delete":                          "删除令牌",
		"token_use":                             "使用令牌",
		"token_denied":                          "令牌拒绝",
		"csrf_denied":                           "跨站请求拦截",
		"token_download_failed":                 "令牌下载失败",
		"token_upload_failed":                   "令牌上传失败",
		"download_lease_create":                 "创建下载票据",
		"public_download_lease_create":          "创建公开下载票据",
		"download_lease_use":                    "使用下载票据",
		"download_lease_file_changed":           "下载票据文件变化",
		"download_lease_resource_changed":       "下载票据资源变化",
		"upload_lease_create":                   "创建上传票据",
		"upload_lease_use":                      "使用上传票据",
		"upload_lease_failed":                   "上传票据失败",
		"upload_lease_resource_changed":         "上传票据资源变化",
		"config_view":                           "查看配置",
		"config_resource_create":                "新增共享资源",
		"config_resource_update":                "修改共享资源",
		"config_resource_delete":                "删除共享资源",
		"config_resource_published_sync_failed": "资源配置发布同步失败",
		"config_upload_policy_update":           "修改上传策略",
		"file_picker_select":                    "选择服务端路径",
		"file_picker_denied":                    "文件选择拒绝",
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

func bearerToken(header string) string {
	fields := strings.Fields(strings.TrimSpace(header))
	if len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") {
		return strings.TrimSpace(fields[1])
	}
	return ""
}

func uploadLeaseTransferID(id int64) string {
	return fmt.Sprintf("upload-lease-%d", id)
}

func uploadLeaseOwner(lease store.UploadLease) (string, string) {
	if lease.Source == "public_token" && lease.TokenID.Valid {
		return "token", strconv.FormatInt(lease.TokenID.Int64, 10)
	}
	return "session", lease.SessionID
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
