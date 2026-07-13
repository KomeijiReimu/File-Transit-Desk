package server

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filetrans-backend/internal/config"
	"filetrans-backend/internal/fsutil"
	"filetrans-backend/internal/security"
	"filetrans-backend/internal/store"

	"github.com/gofiber/fiber/v2"
	"github.com/pquerna/otp/totp"
)

func TestAdminOnlyTokenRoutes(t *testing.T) {
	// 管理接口必须走 adminOnly，中途不能被普通 TOTP 用户访问。
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()

	cfg := config.Default()
	cfg.Auth.TOTPSecret = "JBSWY3DPEHPK3PXP"
	cfg.Auth.DevAllowFixedCode = false
	cfg.Auth.Admin.Username = "admin"
	setTestAdminPassword(cfg)
	cfg.Tokens.UploadMaxMB = 1
	root := t.TempDir()
	setTestStorageAndPickerRoot(cfg, root)
	app := New(cfg, st)

	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create user session: %v", err)
	}
	userReq := httptest.NewRequest(http.MethodGet, "/api/tokens", nil)
	userReq.AddCookie(&http.Cookie{Name: "sid", Value: "user-sid"})
	userResp, err := app.Test(userReq)
	if err != nil {
		t.Fatalf("user request: %v", err)
	}
	if userResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected user to be forbidden, got %d", userResp.StatusCode)
	}

	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	adminReq := httptest.NewRequest(http.MethodGet, "/api/tokens", nil)
	adminReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	adminResp, err := app.Test(adminReq)
	if err != nil {
		t.Fatalf("admin request: %v", err)
	}
	if adminResp.StatusCode != http.StatusOK {
		t.Fatalf("expected admin to access tokens, got %d", adminResp.StatusCode)
	}
}

func TestAdminPasswordPHCPriorityLegacyAndWrongUsernameWork(t *testing.T) {
	phc, err := security.Hash([]byte("new-password"))
	if err != nil {
		t.Fatalf("hash admin password: %v", err)
	}
	cfg := testConfig(t.TempDir())
	cfg.Auth.Admin.PasswordHash = phc
	cfg.Abuse.Login.IPMaxFailures = 2
	cfg.Abuse.Login.IPMaxFailures = 2
	cfg.Auth.Admin.PasswordSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte("legacy-password")))
	s := &Server{config: cfg, adminVerifySlots: newAdminVerifySlots(2)}
	if valid, exhausted := s.validateAdminLogin("admin", "new-password"); !valid || exhausted {
		t.Fatalf("expected PHC password login, valid=%v exhausted=%v", valid, exhausted)
	}
	if valid, _ := s.validateAdminLogin("admin", "legacy-password"); valid {
		t.Fatalf("PHC presence must prevent SHA fallback")
	}

	verifyCalls := 0
	s.verifyAdminPHC = func(string, []byte) (bool, error) {
		verifyCalls++
		return false, nil
	}
	if valid, exhausted := s.validateAdminLogin("wrong-admin", "anything"); valid || exhausted || verifyCalls != 1 {
		t.Fatalf("wrong username must still execute Argon verification, valid=%v exhausted=%v calls=%d", valid, exhausted, verifyCalls)
	}
	if valid, _ := s.validateAdminLogin(strings.Repeat("u", 129), "anything"); valid || verifyCalls != 2 {
		t.Fatalf("oversized username must fail after password verification, valid=%v calls=%d", valid, verifyCalls)
	}
	if valid, exhausted := s.validateAdminLogin("admin", strings.Repeat("p", 1025)); valid || exhausted || verifyCalls != 2 {
		t.Fatalf("oversized password must fail before Argon work, valid=%v exhausted=%v calls=%d", valid, exhausted, verifyCalls)
	}

	legacyCfg := testConfig(t.TempDir())
	legacyCfg.Auth.Admin.PasswordHash = ""
	legacyCfg.Auth.Admin.PasswordSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte("legacy-password")))
	legacyServer := &Server{config: legacyCfg}
	if valid, exhausted := legacyServer.validateAdminLogin("admin", "legacy-password"); !valid || exhausted {
		t.Fatalf("expected deprecated SHA login to remain available")
	}
}

func TestAdminArgonCapacityReturnsStable503(t *testing.T) {
	if newAdminVerifySlots(0) != nil {
		t.Fatalf("expected explicit zero verification limit to disable slots")
	}
	phc, err := security.Hash([]byte("password"))
	if err != nil {
		t.Fatalf("hash admin password: %v", err)
	}
	cfg := testConfig(t.TempDir())
	cfg.Auth.Admin.PasswordHash = phc
	cfg.Abuse.Login.IPMaxFailures = 2
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry(), adminVerifySlots: newAdminVerifySlots(1)}
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.verifyAdminPHC = func(string, []byte) (bool, error) {
		once.Do(func() { close(started) })
		<-release
		return false, nil
	}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	s.routes(app)
	firstDone := make(chan *http.Response, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/admin-login", strings.NewReader(`{"username":"admin","password":"password"}`))
		req.Header.Set("Content-Type", "application/json")
		resp, _ := app.Test(req, 5000)
		firstDone <- resp
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatalf("first Argon verification did not start")
	}
	secondReq := httptest.NewRequest(http.MethodPost, "/api/auth/admin-login", strings.NewReader(`{"username":"admin","password":"password"}`))
	secondReq.Header.Set("Content-Type", "application/json")
	secondResp, err := app.Test(secondReq)
	if err != nil {
		t.Fatalf("second admin login: %v", err)
	}
	var payload map[string]any
	decodeJSON(t, secondResp, &payload)
	if secondResp.StatusCode != http.StatusServiceUnavailable || payload["code"] != "auth_capacity_exhausted" || secondResp.Header.Get("Retry-After") != "1" {
		t.Fatalf("unexpected capacity response: status=%d headers=%v payload=%+v", secondResp.StatusCode, secondResp.Header, payload)
	}
	close(release)
	firstResp := <-firstDone
	if firstResp == nil {
		t.Fatalf("first admin login returned no response")
	}
	firstResp.Body.Close()
	if firstResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected first credential failure, got %d", firstResp.StatusCode)
	}
	thirdReq := httptest.NewRequest(http.MethodPost, "/api/auth/admin-login", strings.NewReader(`{"username":"admin","password":"password"}`))
	thirdReq.Header.Set("Content-Type", "application/json")
	thirdResp, err := app.Test(thirdReq)
	if err != nil {
		t.Fatalf("third admin login: %v", err)
	}
	thirdResp.Body.Close()
	if thirdResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Argon capacity response must not count as credential failure, got %d", thirdResp.StatusCode)
	}
}

func TestAdminLoginRejectsOversizedBodyBeforePasswordVerification(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.Auth.Admin.PasswordHash = "$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHQ$MTIzNDU2Nzg5MDEyMzQ1Ng"
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	verifyCalls := 0
	s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry(), adminVerifySlots: newAdminVerifySlots(1), verifyAdminPHC: func(string, []byte) (bool, error) {
		verifyCalls++
		return true, nil
	}}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler, StreamRequestBody: true})
	s.routes(app)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/admin-login", strings.NewReader(strings.Repeat("x", maxAdminLoginBodyBytes+1)))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = maxAdminLoginBodyBytes + 1
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("oversized admin login: %v", err)
	}
	var payload map[string]any
	decodeJSON(t, resp, &payload)
	if resp.StatusCode != http.StatusRequestEntityTooLarge || payload["code"] != "auth_request_too_large" || verifyCalls != 0 {
		t.Fatalf("unexpected oversized login response: status=%d payload=%+v verifyCalls=%d", resp.StatusCode, payload, verifyCalls)
	}
}

func TestSharedTOTPCodeCanCreateMultipleSessionsInSameWindow(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.Auth.TOTPSecret = "JBSWY3DPEHPK3PXP"
	cfg.Auth.DevAllowFixedCode = false
	code, err := totp.GenerateCode(cfg.Auth.TOTPSecret, time.Now())
	if err != nil {
		t.Fatalf("generate TOTP code: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app := New(cfg, st)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(fmt.Sprintf(`{"code":%q}`, code)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("shared TOTP login %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected shared same-window code login %d success, got %d", i, resp.StatusCode)
		}
	}
}

func TestLegacyAdminHashWarningDoesNotExposeHash(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.Auth.Admin.PasswordHash = ""
	cfg.Auth.Admin.PasswordSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte("legacy-password")))
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	output, restoreLog := captureTestLog(t)
	_ = New(cfg, st)
	restoreLog()
	if strings.Count(output.String(), "legacy_admin_password_sha256") != 1 || strings.Contains(output.String(), cfg.Auth.Admin.PasswordSHA256) {
		t.Fatalf("expected hash-free structured legacy warning, log=%q", output.String())
	}
}

func TestLoginGlobalBucketsAreSeparateForUserAndAdmin(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.Abuse.Login.GlobalPerMinute = 1
	cfg.Abuse.Login.IPMaxFailures = 100
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app := New(cfg, st)
	post := func(path, body string) (*http.Response, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, requestErr := app.Test(req)
		if requestErr != nil {
			t.Fatalf("login request: %v", requestErr)
		}
		var payload map[string]any
		decodeJSON(t, resp, &payload)
		return resp, payload
	}
	if resp, _ := post("/api/auth/login", `{"code":"111111"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected first user attempt processed, got %d", resp.StatusCode)
	}
	resp, payload := post("/api/auth/login", `{"code":"111111"}`)
	if resp.StatusCode != http.StatusTooManyRequests || payload["code"] != "login_rate_limited" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("unexpected user global limit response: status=%d payload=%+v", resp.StatusCode, payload)
	}
	if resp, _ := post("/api/auth/admin-login", `{"username":"admin","password":"wrong"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("admin global bucket must be separate, got %d", resp.StatusCode)
	}
}

func TestLoginFailureBucketsUseTrustedProxyIPAndSuccessResets(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.Server.TrustProxyHeaders = true
	cfg.Server.TrustedProxyCIDRs = []string{"0.0.0.0/32"}
	cfg.Abuse.Login.GlobalPerMinute = 0
	cfg.Abuse.Login.IPMaxFailures = 2
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app := New(cfg, st)
	code, err := totp.GenerateCode(cfg.Auth.TOTPSecret, time.Now())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	post := func(ip, code string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(fmt.Sprintf(`{"code":%q}`, code)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", ip)
		resp, requestErr := app.Test(req)
		if requestErr != nil {
			t.Fatalf("login request: %v", requestErr)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := post("198.51.100.10", "111111"); got != http.StatusUnauthorized {
		t.Fatalf("first failure status=%d", got)
	}
	if got := post("198.51.100.11", "111111"); got != http.StatusUnauthorized {
		t.Fatalf("different trusted-proxy client must use separate bucket, status=%d", got)
	}
	if got := post("198.51.100.10", code); got != http.StatusOK {
		t.Fatalf("successful login status=%d", got)
	}
	if got := post("198.51.100.10", "111111"); got != http.StatusUnauthorized {
		t.Fatalf("first post-reset failure status=%d", got)
	}
	if got := post("198.51.100.10", "111111"); got != http.StatusUnauthorized {
		t.Fatalf("second post-reset failure should still be processed, status=%d", got)
	}
	if got := post("198.51.100.10", "111111"); got != http.StatusTooManyRequests {
		t.Fatalf("expected block only after post-reset failures, status=%d", got)
	}
}

func TestCreationRateLimitCodesAndStoreLimitResponses(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.Abuse.Creation.TokenGlobalPerMinute = 100
	cfg.Abuse.Creation.TokenPerSessionPerMinute = 1
	cfg.Abuse.Creation.LeaseGlobalPerMinute = 100
	cfg.Abuse.Creation.LeasePerOwnerPerMinute = 1
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app := New(cfg, st)
	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	request := func(path string) (*http.Response, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
		resp, requestErr := app.Test(req)
		if requestErr != nil {
			t.Fatalf("creation request: %v", requestErr)
		}
		var payload map[string]any
		decodeJSON(t, resp, &payload)
		return resp, payload
	}
	_, _ = request("/api/tokens")
	resp, payload := request("/api/tokens")
	if resp.StatusCode != http.StatusTooManyRequests || payload["code"] != "token_create_rate_limited" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("unexpected token rate response: status=%d payload=%+v", resp.StatusCode, payload)
	}
	_, _ = request("/api/files/download-lease")
	resp, payload = request("/api/files/download-lease")
	if resp.StatusCode != http.StatusTooManyRequests || payload["code"] != "lease_create_rate_limited" {
		t.Fatalf("unexpected lease rate response: status=%d payload=%+v", resp.StatusCode, payload)
	}

	s := &Server{}
	errorApp := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	errorApp.Get("/outstanding", func(c *fiber.Ctx) error { return s.creationStoreError(c, store.ErrOutstandingLeaseLimit) })
	errorApp.Get("/tokens", func(c *fiber.Ctx) error { return s.creationStoreError(c, store.ErrActiveTokenLimitReached) })
	for _, tc := range []struct {
		path   string
		status int
		code   string
		retry  string
	}{{"/outstanding", http.StatusTooManyRequests, "outstanding_lease_limit", "30"}, {"/tokens", http.StatusForbidden, "active_token_limit_reached", ""}} {
		result, requestErr := errorApp.Test(httptest.NewRequest(http.MethodGet, tc.path, nil))
		if requestErr != nil {
			t.Fatalf("store error response: %v", requestErr)
		}
		var body map[string]any
		decodeJSON(t, result, &body)
		if result.StatusCode != tc.status || body["code"] != tc.code || result.Header.Get("Retry-After") != tc.retry {
			t.Fatalf("unexpected store limit response: status=%d body=%+v retry=%q", result.StatusCode, body, result.Header.Get("Retry-After"))
		}
	}
}

func TestPublicDownloadLeaseRateLimitRunsBeforeTokenLookup(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.Abuse.Creation.LeaseGlobalPerMinute = 1
	cfg.Abuse.Creation.LeasePerOwnerPerMinute = 100
	cfg.Abuse.Creation.PublicLeasePerIPPerMinute = 100
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app := New(cfg, st)
	first, err := app.Test(httptest.NewRequest(http.MethodPost, "/t/missing-token/download-lease", nil))
	if err != nil {
		t.Fatalf("first public lease: %v", err)
	}
	first.Body.Close()
	if first.StatusCode == http.StatusTooManyRequests {
		t.Fatalf("expected first request to reach token lookup, got %d", first.StatusCode)
	}
	second, err := app.Test(httptest.NewRequest(http.MethodPost, "/t/missing-token/download-lease", nil))
	if err != nil {
		t.Fatalf("second public lease: %v", err)
	}
	var payload map[string]any
	decodeJSON(t, second, &payload)
	if second.StatusCode != http.StatusTooManyRequests || payload["code"] != "lease_create_rate_limited" {
		t.Fatalf("expected limiter before repeated token lookup, status=%d payload=%+v", second.StatusCode, payload)
	}
}

func TestPublicLeasePerIPUsesTrustedProxyClientIP(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.Server.TrustProxyHeaders = true
	cfg.Server.TrustedProxyCIDRs = []string{"0.0.0.0/32"}
	cfg.Abuse.Creation.LeaseGlobalPerMinute = 0
	cfg.Abuse.Creation.LeasePerOwnerPerMinute = 100
	cfg.Abuse.Creation.PublicLeasePerIPPerMinute = 1
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app := New(cfg, st)
	request := func(token, ip string) (*http.Response, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/t/"+token+"/download-lease", nil)
		req.Header.Set("X-Forwarded-For", ip)
		resp, requestErr := app.Test(req)
		if requestErr != nil {
			t.Fatalf("public lease request: %v", requestErr)
		}
		var payload map[string]any
		decodeJSON(t, resp, &payload)
		return resp, payload
	}
	first, _ := request("missing-one", "198.51.100.30")
	if first.StatusCode == http.StatusTooManyRequests {
		t.Fatalf("first client request should reach token lookup")
	}
	second, payload := request("missing-two", "198.51.100.30")
	if second.StatusCode != http.StatusTooManyRequests || payload["code"] != "lease_create_rate_limited" {
		t.Fatalf("expected per-IP public lease limit, status=%d payload=%+v", second.StatusCode, payload)
	}
	third, _ := request("missing-three", "198.51.100.31")
	if third.StatusCode == http.StatusTooManyRequests {
		t.Fatalf("different trusted-proxy client IP must use a separate public lease bucket")
	}
}

func TestDirsHidePathForUserAndShowRootForAdmin(t *testing.T) {
	// 普通用户不能看到服务端真实路径；管理员配置页需要 root 做排障展示。
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()

	root := t.TempDir()
	cfg := config.Default()
	cfg.Auth.Admin.Username = "admin"
	setTestAdminPassword(cfg)
	setTestStorageAndPickerRoot(cfg, root)
	app := New(cfg, st)

	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create user session: %v", err)
	}
	userReq := httptest.NewRequest(http.MethodGet, "/api/dirs", nil)
	userReq.AddCookie(&http.Cookie{Name: "sid", Value: "user-sid"})
	userResp, err := app.Test(userReq)
	if err != nil {
		t.Fatalf("user dirs request: %v", err)
	}
	defer userResp.Body.Close()
	var userDirs []map[string]any
	if err := json.NewDecoder(userResp.Body).Decode(&userDirs); err != nil {
		t.Fatalf("decode user dirs: %v", err)
	}
	if userResp.StatusCode != http.StatusOK || len(userDirs) != 1 {
		t.Fatalf("expected one user dir, status=%d dirs=%d", userResp.StatusCode, len(userDirs))
	}
	if _, ok := userDirs[0]["path"]; ok {
		t.Fatalf("user dirs response leaked path field")
	}
	if _, ok := userDirs[0]["root"]; ok {
		t.Fatalf("user dirs response leaked root field")
	}

	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	adminReq := httptest.NewRequest(http.MethodGet, "/api/dirs", nil)
	adminReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	adminResp, err := app.Test(adminReq)
	if err != nil {
		t.Fatalf("admin dirs request: %v", err)
	}
	defer adminResp.Body.Close()
	var adminDirs []map[string]any
	if err := json.NewDecoder(adminResp.Body).Decode(&adminDirs); err != nil {
		t.Fatalf("decode admin dirs: %v", err)
	}
	if adminResp.StatusCode != http.StatusOK || adminDirs[0]["root"] != root {
		t.Fatalf("expected admin root %q, status=%d dirs=%v", root, adminResp.StatusCode, adminDirs)
	}
}

func TestAdminConfigCanManageDirectoryAndFileResources(t *testing.T) {
	// 管理员可视化配置只暴露安全字段，目录和单文件资源写回配置后要立即对新请求生效。
	base := t.TempDir()
	dirRoot := filepath.Join(base, "shared")
	filePath := filepath.Join(base, "manual.pdf")
	if err := os.MkdirAll(dirRoot, 0755); err != nil {
		t.Fatalf("mkdir shared: %v", err)
	}
	if err := osWriteFile(filePath, []byte("manual")); err != nil {
		t.Fatalf("write manual: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(base)
	cfg.Auth.DevAllowFixedCode = true
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	app := NewWithConfigPath(cfg, st, cfgPath)
	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}

	adminReq := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	adminReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	adminResp, err := app.Test(adminReq)
	if err != nil {
		t.Fatalf("safe config request: %v", err)
	}
	var safe map[string]any
	decodeJSON(t, adminResp, &safe)
	if adminResp.StatusCode != http.StatusOK || strings.Contains(fmt.Sprint(safe), "password_sha256") || strings.Contains(fmt.Sprint(safe), "password_hash") || strings.Contains(fmt.Sprint(safe), "totp_secret") {
		t.Fatalf("safe config leaked sensitive fields or failed: status=%d body=%v", adminResp.StatusCode, safe)
	}

	createDir := httptest.NewRequest(http.MethodPost, "/api/config/resources", strings.NewReader(fmt.Sprintf(`{"id":"photos","name":"照片","type":"directory","path":%q,"allowDownload":true,"allowUpload":true}`, dirRoot)))
	createDir.Header.Set("Content-Type", "application/json")
	createDir.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	createDirResp, err := app.Test(createDir)
	if err != nil {
		t.Fatalf("create dir resource: %v", err)
	}
	if createDirResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(createDirResp.Body)
		t.Fatalf("create dir status=%d body=%s", createDirResp.StatusCode, body)
	}
	createDirResp.Body.Close()

	createFile := httptest.NewRequest(http.MethodPost, "/api/config/resources", strings.NewReader(fmt.Sprintf(`{"id":"manual","name":"说明文档","type":"file","path":%q,"allowDownload":true,"allowUpload":true}`, filePath)))
	createFile.Header.Set("Content-Type", "application/json")
	createFile.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	createFileResp, err := app.Test(createFile)
	if err != nil {
		t.Fatalf("create file resource: %v", err)
	}
	if createFileResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(createFileResp.Body)
		t.Fatalf("create file status=%d body=%s", createFileResp.StatusCode, body)
	}
	createFileResp.Body.Close()

	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create user session: %v", err)
	}
	userDirsReq := httptest.NewRequest(http.MethodGet, "/api/dirs", nil)
	userDirsReq.AddCookie(&http.Cookie{Name: "sid", Value: "user-sid"})
	userDirsResp, err := app.Test(userDirsReq)
	if err != nil {
		t.Fatalf("user dirs request: %v", err)
	}
	var dirs []map[string]any
	decodeJSON(t, userDirsResp, &dirs)
	if len(dirs) != 3 || fmt.Sprint(dirs[2]["type"]) != "file" {
		t.Fatalf("expected hot-updated file resource without root leak, got %+v", dirs)
	}
	if _, ok := dirs[2]["root"]; ok {
		t.Fatalf("user resource leaked root: %+v", dirs[2])
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/files/list?dirId=manual", nil)
	listReq.AddCookie(&http.Cookie{Name: "sid", Value: "user-sid"})
	listResp, err := app.Test(listReq)
	if err != nil {
		t.Fatalf("file resource list: %v", err)
	}
	var list fileListResponse
	decodeJSON(t, listResp, &list)
	if listResp.StatusCode != http.StatusOK || len(list.Entries) != 1 || list.Entries[0].Name != "manual.pdf" || list.CanUpload {
		t.Fatalf("unexpected file resource list: status=%d list=%+v", listResp.StatusCode, list)
	}

	leaseReq := httptest.NewRequest(http.MethodPost, "/api/files/download-lease", strings.NewReader(`{"dirId":"manual","path":"manual.pdf"}`))
	leaseReq.Header.Set("Content-Type", "application/json")
	leaseReq.AddCookie(&http.Cookie{Name: "sid", Value: "user-sid"})
	leaseResp, err := app.Test(leaseReq)
	if err != nil {
		t.Fatalf("file resource lease: %v", err)
	}
	var lease downloadLeaseResponse
	decodeJSON(t, leaseResp, &lease)
	if leaseResp.StatusCode != http.StatusOK || lease.URL == "" {
		t.Fatalf("expected lease for file resource, status=%d lease=%+v", leaseResp.StatusCode, lease)
	}

	manualFingerprint := resourceAuthorizationFingerprint(config.Dir{ID: "manual", Type: config.ResourceFile, Path: filePath, AllowDownload: true})
	manualToken := &store.Token{Hash: "manual-token", Type: "download", DirID: "manual", Path: "", ResourceFingerprint: manualFingerprint, MaxUses: 10}
	if err := st.CreateToken(manualToken); err != nil {
		t.Fatalf("create manual token: %v", err)
	}
	manualLease := &store.DownloadLease{Hash: "manual-lease", Source: "public_token", TokenID: sql.NullInt64{Int64: manualToken.ID, Valid: true}, DirID: "manual", Path: "", ResourceFingerprint: manualFingerprint, FileSize: 6, FileMtime: time.Now(), FileSHA256: sql.NullString{String: "", Valid: true}, ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.CreateDownloadLease(manualLease); err != nil {
		t.Fatalf("create manual lease: %v", err)
	}
	newFilePath := filepath.Join(base, "manual-v2.pdf")
	if err := osWriteFile(newFilePath, []byte("manual-v2")); err != nil {
		t.Fatalf("write new manual: %v", err)
	}
	updateFile := httptest.NewRequest(http.MethodPut, "/api/config/resources/manual", strings.NewReader(fmt.Sprintf(`{"id":"manual","name":"说明文档","type":"file","path":%q,"allowDownload":true,"allowUpload":false}`, newFilePath)))
	updateFile.Header.Set("Content-Type", "application/json")
	updateFile.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	updateFileResp, err := app.Test(updateFile)
	if err != nil {
		t.Fatalf("update file resource: %v", err)
	}
	if updateFileResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(updateFileResp.Body)
		t.Fatalf("update file status=%d body=%s", updateFileResp.StatusCode, body)
	}
	updateFileResp.Body.Close()
	revoked, err := st.TokenByHash("manual-token")
	if err != nil || !revoked.Revoked {
		t.Fatalf("expected token for changed resource to be revoked, token=%+v err=%v", revoked, err)
	}
	if _, err := st.DownloadLeaseByHash("manual-lease"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected lease for changed resource to be deleted, got %v", err)
	}
}

func TestTokenListReturnsValidityAndDeleteAudit(t *testing.T) {
	// 令牌列表需要返回失效原因，删除操作也必须留下审计记录。
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()

	cfg := config.Default()
	cfg.Auth.Admin.Username = "admin"
	setTestAdminPassword(cfg)
	cfg.Tokens.UploadMaxMB = 1
	root := t.TempDir()
	setTestStorageAndPickerRoot(cfg, root)
	app := New(cfg, st)
	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	expiredAt := time.Now().Add(-time.Minute)
	tok := &store.Token{Hash: "expired", Type: "download", DirID: "default", Path: "a.txt", ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), MaxUses: 1, ExpiresAt: sqlNullTime(expiredAt)}
	if err := st.CreateToken(tok); err != nil {
		t.Fatalf("create token: %v", err)
	}
	quotaTok := &store.Token{Hash: "quota", Type: "upload", DirID: "default", Path: "", ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), MaxUses: 0}
	if err := st.CreateToken(quotaTok); err != nil {
		t.Fatalf("create quota token: %v", err)
	}
	if _, err := st.ReserveTokenUse("quota", "upload", time.Now(), 1024*1024, 1024*1024); err != nil {
		t.Fatalf("reserve quota token: %v", err)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/tokens", nil)
	listReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	listResp, err := app.Test(listReq)
	if err != nil {
		t.Fatalf("list tokens request: %v", err)
	}
	defer listResp.Body.Close()
	var tokens []map[string]any
	if err := json.NewDecoder(listResp.Body).Decode(&tokens); err != nil {
		t.Fatalf("decode tokens: %v", err)
	}
	if listResp.StatusCode != http.StatusOK || len(tokens) != 2 {
		t.Fatalf("expected two tokens, status=%d tokens=%d", listResp.StatusCode, len(tokens))
	}
	seenExpired := false
	seenQuota := false
	for _, token := range tokens {
		switch token["reason"] {
		case "expired":
			seenExpired = token["valid"] == false
		case "upload_quota_exhausted":
			seenQuota = token["valid"] == false
		}
	}
	if !seenExpired || !seenQuota {
		t.Fatalf("expected expired and quota-exhausted token validity, got %v", tokens)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/tokens/"+strconv.FormatInt(tok.ID, 10), nil)
	deleteReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	deleteResp, err := app.Test(deleteReq)
	if err != nil {
		t.Fatalf("delete token request: %v", err)
	}
	defer deleteResp.Body.Close()
	if deleteResp.StatusCode != http.StatusOK {
		t.Fatalf("expected delete ok, got %d", deleteResp.StatusCode)
	}
	logs, err := st.AuditLogs(10)
	if err != nil {
		t.Fatalf("audit logs: %v", err)
	}
	found := false
	for _, log := range logs {
		if log.Action == "token_delete" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected token_delete audit log")
	}
}

func TestCreateTokenStoresResourceFingerprintForDownloadAndUpload(t *testing.T) {
	root := t.TempDir()
	if err := osWriteFile(filepath.Join(root, "download.txt"), []byte("download")); err != nil {
		t.Fatalf("write download file: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(root)
	app := New(cfg, st)
	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	for _, body := range []string{
		`{"type":"download","dirId":"default","path":"download.txt","maxUses":1}`,
		`{"type":"upload","dirId":"default","path":"incoming","maxUses":1}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/tokens", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("create token: %v", err)
		}
		var created struct {
			ID int64 `json:"id"`
		}
		decodeJSON(t, resp, &created)
		if resp.StatusCode != http.StatusOK || created.ID == 0 {
			t.Fatalf("unexpected create token response: status=%d id=%d", resp.StatusCode, created.ID)
		}
		stored, err := st.TokenByID(created.ID)
		if err != nil || stored.ResourceFingerprint != testResourceFingerprint(t, cfg, "default") {
			t.Fatalf("expected token resource fingerprint, token=%+v err=%v", stored, err)
		}
	}
}

func TestUploadPolicyRejectsBlockedExtension(t *testing.T) {
	// 上传扩展名策略必须在文件落盘前拒绝危险类型。
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()

	cfg := config.Default()
	cfg.Auth.Admin.Username = "admin"
	setTestAdminPassword(cfg)
	cfg.Storage.BlockedExtensions = []string{".exe"}
	root := t.TempDir()
	setTestStorageAndPickerRoot(cfg, root)
	app := New(cfg, st)

	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create user session: %v", err)
	}
	for _, name := range []string{"bad.exe", "bad.exe ", "bad.exe.\t"} {
		body, contentType := multipartUploadBody(t, name, []byte("x"))
		req := httptest.NewRequest(http.MethodPost, "/api/files/upload", body)
		req.Header.Set("Content-Type", contentType)
		req.AddCookie(&http.Cookie{Name: "sid", Value: "user-sid"})
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("upload request %q: %v", name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected blocked extension %q to be forbidden, got %d", name, resp.StatusCode)
		}
	}
}

func TestAdminCanUpdateUploadPolicyWithEmptyBlacklist(t *testing.T) {
	// 默认黑名单允许被清空；Web 保存后配置热更新，新的上传立即按最新策略判断。
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	root := t.TempDir()
	cfg := testConfig(root)
	cfg.Auth.DevAllowFixedCode = true
	cfg.Storage.BlockedExtensions = []string{".exe"}
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	app := NewWithConfigPath(cfg, st, cfgPath)
	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create user session: %v", err)
	}

	policyReq := httptest.NewRequest(http.MethodPut, "/api/config/upload-policy", strings.NewReader(`{"allowedExtensions":[],"blockedExtensions":[]}`))
	policyReq.Header.Set("Content-Type", "application/json")
	policyReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	policyResp, err := app.Test(policyReq)
	if err != nil {
		t.Fatalf("update upload policy: %v", err)
	}
	var policy uploadPolicyResponse
	decodeJSON(t, policyResp, &policy)
	if policyResp.StatusCode != http.StatusOK || len(policy.BlockedExtensions) != 0 {
		t.Fatalf("expected empty blacklist policy, status=%d policy=%+v", policyResp.StatusCode, policy)
	}

	body, contentType := multipartUploadBody(t, "tool.exe", []byte("ok"))
	uploadReq := httptest.NewRequest(http.MethodPost, "/api/files/upload", body)
	uploadReq.Header.Set("Content-Type", contentType)
	uploadReq.AddCookie(&http.Cookie{Name: "sid", Value: "user-sid"})
	uploadResp, err := app.Test(uploadReq)
	if err != nil {
		t.Fatalf("upload request: %v", err)
	}
	uploadResp.Body.Close()
	if uploadResp.StatusCode != http.StatusOK {
		t.Fatalf("expected exe upload to follow cleared blacklist, got %d", uploadResp.StatusCode)
	}

	badReq := httptest.NewRequest(http.MethodPut, "/api/config/upload-policy", strings.NewReader(`{"allowedExtensions":["*"],"blockedExtensions":[]}`))
	badReq.Header.Set("Content-Type", "application/json")
	badReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	restoreLog := discardTestLog(t)
	badResp, err := app.Test(badReq)
	restoreLog()
	if err != nil {
		t.Fatalf("bad policy request: %v", err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected wildcard policy to be rejected, got %d", badResp.StatusCode)
	}
}

func TestAuditLogsPagination(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app := New(testConfig(t.TempDir()), st)
	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := st.Audit("test", "127.0.0.1", fmt.Sprintf("log-%d", i)); err != nil {
			t.Fatalf("audit: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/audit/logs?page=2&pageSize=2", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("audit page request: %v", err)
	}
	var page auditPageDTO
	decodeJSON(t, resp, &page)
	if resp.StatusCode != http.StatusOK || page.Page != 2 || page.PageSize != 2 || page.Total != 3 || page.TotalPages != 2 || len(page.Logs) != 1 {
		t.Fatalf("unexpected audit page: status=%d page=%+v", resp.StatusCode, page)
	}
}

func TestAuditLogsGlobalFiltersAndValidation(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app := New(testConfig(t.TempDir()), st)
	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	for _, entry := range []struct {
		action string
		detail string
	}{
		{"token_create", "global-filter-needle"},
		{"login_success", "ok event"},
		{"login_failed", "failed event"},
		{"file_picker_denied", "policy event"},
		{"download", "literal 100%_value"},
		{"config_view", "newest event"},
	} {
		if err := st.Audit(entry.action, "192.0.2.10", entry.detail); err != nil {
			t.Fatalf("seed audit: %v", err)
		}
	}
	requestPage := func(query string) (*http.Response, auditPageDTO) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/admin/audit?"+query, nil)
		req.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("audit request: %v", err)
		}
		var page auditPageDTO
		decodeJSON(t, resp, &page)
		return resp, page
	}
	resp, page := requestPage("page=1&pageSize=2&keyword=global-filter-needle&status=all")
	if resp.StatusCode != http.StatusOK || page.Total != 1 || len(page.Logs) != 1 || page.Logs[0].Action != "token_create" {
		t.Fatalf("global keyword result status=%d page=%+v", resp.StatusCode, page)
	}
	resp, failed := requestPage("page=1&pageSize=1&status=failed")
	if resp.StatusCode != http.StatusOK || failed.Total != 2 || failed.TotalPages != 2 || len(failed.Logs) != 1 || !store.IsAuditFailureAction(failed.Logs[0].Action) {
		t.Fatalf("failed status page=%+v status=%d", failed, resp.StatusCode)
	}
	_, failedPage2 := requestPage("page=2&pageSize=1&status=failed")
	if failedPage2.Total != failed.Total || len(failedPage2.Logs) != 1 || !store.IsAuditFailureAction(failedPage2.Logs[0].Action) {
		t.Fatalf("failed page2 count/list mismatch: %+v", failedPage2)
	}
	_, literal := requestPage("page=1&pageSize=10&keyword=" + url.QueryEscape("%_") + "&status=all")
	if literal.Total != 1 || len(literal.Logs) != 1 || literal.Logs[0].Action != "download" {
		t.Fatalf("literal LIKE filter mismatch: %+v", literal)
	}
	_, labelOnly := requestPage("page=1&pageSize=10&keyword=" + url.QueryEscape("登录成功") + "&status=all")
	if labelOnly.Total != 0 || len(labelOnly.Logs) != 0 {
		t.Fatalf("actionLabel unexpectedly participated in filtering: %+v", labelOnly)
	}
	_, empty := requestPage("page=1&pageSize=10&keyword=does-not-exist&status=all")
	if empty.Total != 0 || empty.TotalPages != 0 || len(empty.Logs) != 0 {
		t.Fatalf("unexpected empty filter response: %+v", empty)
	}
	for _, query := range []string{
		"page=1&pageSize=10&status=unknown",
		"page=1&pageSize=10&keyword=" + url.QueryEscape(strings.Repeat("SENSITIVE", 26)),
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/audit?"+query, nil)
		req.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
		invalidResp, err := app.Test(req)
		if err != nil {
			t.Fatalf("invalid filter request: %v", err)
		}
		body, _ := io.ReadAll(invalidResp.Body)
		invalidResp.Body.Close()
		if invalidResp.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte(`"code":"audit_filter_invalid"`)) || bytes.Contains(body, []byte("SENSITIVE")) {
			t.Fatalf("invalid filter response leaked input or wrong code: status=%d body=%s", invalidResp.StatusCode, body)
		}
	}
	_, beyond := requestPage("page=999&pageSize=10&status=all")
	if beyond.Total != 6 || len(beyond.Logs) != 0 {
		t.Fatalf("normal page beyond total should be empty: %+v", beyond)
	}
	for _, query := range []string{
		"page=0&pageSize=10&status=all",
		"page=9223372036854775807&pageSize=200&status=all",
		"page=999999999999999999999999&pageSize=10&status=all",
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/audit?"+query, nil)
		req.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
		pageResp, err := app.Test(req)
		if err != nil {
			t.Fatalf("invalid page request: %v", err)
		}
		body, _ := io.ReadAll(pageResp.Body)
		pageResp.Body.Close()
		if pageResp.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte(`"code":"audit_page_out_of_range"`)) || bytes.Contains(body, []byte(query)) {
			t.Fatalf("invalid page response status=%d body=%s", pageResp.StatusCode, body)
		}
	}
}

func TestAuditDTOStatusMatchesExactFailureClassificationAcrossRoutes(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app := New(testConfig(t.TempDir()), st)
	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	wantStatus := map[string]string{
		"download_lease_file_changed":     "failed",
		"download_lease_resource_changed": "failed",
		"token_download_failed":           "failed",
		"config_changed":                  "ok",
		"unknown_action":                  "ok",
	}
	for action := range wantStatus {
		if err := st.Audit(action, "192.0.2.20", "stable detail"); err != nil {
			t.Fatalf("seed audit: %v", err)
		}
	}
	request := func(path string) auditPageDTO {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path+"?page=1&pageSize=20&status=all", nil)
		req.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("audit request %s: %v", path, err)
		}
		var page auditPageDTO
		decodeJSON(t, resp, &page)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("audit request %s status=%d", path, resp.StatusCode)
		}
		return page
	}
	legacy := request("/api/audit/logs")
	admin := request("/api/admin/audit")
	if !reflect.DeepEqual(legacy, admin) {
		t.Fatalf("compatibility routes returned different DTOs: legacy=%+v admin=%+v", legacy, admin)
	}
	if legacy.Total != len(wantStatus) || len(legacy.Logs) != len(wantStatus) {
		t.Fatalf("unexpected audit DTO count: %+v", legacy)
	}
	for _, entry := range legacy.Logs {
		if entry.Status != wantStatus[entry.Action] {
			t.Fatalf("action %s status=%s want=%s", entry.Action, entry.Status, wantStatus[entry.Action])
		}
		if entry.Action == "token_download_failed" && entry.ActionLabel != "令牌下载失败" {
			t.Fatalf("historical action label changed: %+v", entry)
		}
	}
	for _, tc := range []struct {
		status string
		want   int
	}{
		{"failed", 3},
		{"ok", 2},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/audit?page=1&pageSize=20&status="+tc.status, nil)
		req.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("status request: %v", err)
		}
		var page auditPageDTO
		decodeJSON(t, resp, &page)
		if page.Total != tc.want || len(page.Logs) != tc.want {
			t.Fatalf("status=%s page=%+v", tc.status, page)
		}
		for _, entry := range page.Logs {
			if entry.Status != tc.status {
				t.Fatalf("filter status and DTO diverged: filter=%s entry=%+v", tc.status, entry)
			}
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/admin/audit?page=1&pageSize=1&status=all", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("raw DTO request: %v", err)
	}
	var raw struct {
		Logs []map[string]any `json:"logs"`
	}
	decodeJSON(t, resp, &raw)
	allowed := map[string]bool{"id": true, "action": true, "actionLabel": true, "status": true, "ip": true, "detail": true, "createdAt": true}
	for key := range raw.Logs[0] {
		if !allowed[key] {
			t.Fatalf("audit DTO leaked unexpected field %q", key)
		}
	}
}

func TestOriginFromIPUsesFrontendPort(t *testing.T) {
	if got := originFromIP("http", net.ParseIP("192.168.124.9"), "5173"); got != "http://192.168.124.9:5173" {
		t.Fatalf("unexpected IPv4 origin: %s", got)
	}
	if got := originFromIP("https", net.ParseIP("2001:db8::1"), "8443"); got != "https://[2001:db8::1]:8443" {
		t.Fatalf("unexpected IPv6 origin: %s", got)
	}
}

func TestDevelopmentFrontendOriginPolicy(t *testing.T) {
	cases := []struct {
		origin string
		want   bool
	}{
		{"http://localhost:5173", true},
		{"http://127.0.0.1:5173", true},
		{"http://192.168.124.9:5173", true},
		{"http://8.8.8.8:5173", false},
		{"http://example.com:5173", false},
		{"http://192.168.124.9:5174", false},
	}
	for _, tc := range cases {
		if got := developmentFrontendOrigin(tc.origin, 5173); got != tc.want {
			t.Fatalf("developmentFrontendOrigin(%q)=%v, want %v", tc.origin, got, tc.want)
		}
	}
}

func TestDevelopmentFrontendOriginUsesConfiguredPort(t *testing.T) {
	if !developmentFrontendOrigin("http://192.168.124.9:5174", 5174) {
		t.Fatalf("expected configured frontend port 5174 to be allowed")
	}
	if developmentFrontendOrigin("http://192.168.124.9:5173", 5174) {
		t.Fatalf("expected default port 5173 to be rejected when configured port is 5174")
	}
}

func TestDevelopmentFrontendPortRejectsInvalidRange(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	for _, value := range []int{-1, 0, 65536} {
		if _, err := NewWithOptions(testConfig(t.TempDir()), st, "", Options{DevMode: true, DevFrontendPort: value}); err == nil {
			t.Fatalf("expected invalid dev frontend port %d rejected", value)
		}
	}
}

func TestDevelopmentTransferOriginAllowedForSameHost(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app, err := NewWithOptions(testConfig(t.TempDir()), st, "", Options{DevMode: true, DevFrontendPort: 5173})
	if err != nil {
		t.Fatalf("new dev server: %v", err)
	}
	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create session: %v", err)
	}

	preflight := httptest.NewRequest(http.MethodOptions, "/api/files/upload-by-lease", nil)
	preflight.Host = "192.168.124.9:17878"
	preflight.Header.Set("Origin", "http://192.168.124.9:5173")
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	preflight.Header.Set("Access-Control-Request-Headers", "Authorization")
	preflightResp, err := app.Test(preflight)
	if err != nil {
		t.Fatalf("preflight request: %v", err)
	}
	preflightResp.Body.Close()
	if got := preflightResp.Header.Get("Access-Control-Allow-Origin"); got != "http://192.168.124.9:5173" {
		t.Fatalf("expected dynamic CORS allow origin, got %q status=%d", got, preflightResp.StatusCode)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.Host = "192.168.124.9:17878"
	req.Header.Set("Origin", "http://192.168.124.9:5173")
	req.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("same-host dev origin request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Fatalf("same-host development frontend origin should not be blocked by csrf guard")
	}
}

func TestDevelopmentTransferOriginAllowedForConfiguredPort(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app, err := NewWithOptions(testConfig(t.TempDir()), st, "", Options{DevMode: true, DevFrontendPort: 5174})
	if err != nil {
		t.Fatalf("new dev server: %v", err)
	}
	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.Host = "192.168.124.9:17878"
	req.Header.Set("Origin", "http://192.168.124.9:5174")
	req.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("configured dev origin request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Fatalf("configured same-host development frontend origin should not be blocked")
	}
}

func TestDevelopmentTransferOriginRejectsDifferentHost(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app, err := NewWithOptions(testConfig(t.TempDir()), st, "", Options{DevMode: true, DevFrontendPort: 5173})
	if err != nil {
		t.Fatalf("new dev server: %v", err)
	}
	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.Host = "192.168.124.9:17878"
	req.Header.Set("Origin", "http://192.168.124.10:5173")
	req.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("different-host dev origin request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected different-host development origin to be blocked, got %d", resp.StatusCode)
	}
}

func TestProductionOriginsRequireSameOriginOrExplicitAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name          string
		allowOrigins  []string
		origin        string
		wantForbidden bool
	}{
		{"empty list rejects dev origin", nil, "http://192.168.124.9:5173", true},
		{"explicit origin allowed", []string{"https://admin.example"}, "https://admin.example", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			defer st.DB.Close()
			cfg := testConfig(t.TempDir())
			cfg.CORS.AllowOrigins = tc.allowOrigins
			app := New(cfg, st)
			if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
				t.Fatalf("create session: %v", err)
			}
			preflight := httptest.NewRequest(http.MethodOptions, "/api/auth/logout", nil)
			preflight.Header.Set("Origin", tc.origin)
			preflight.Header.Set("Access-Control-Request-Method", "POST")
			preflightResp, err := app.Test(preflight)
			if err != nil {
				t.Fatalf("preflight: %v", err)
			}
			preflightResp.Body.Close()
			corsAllowed := preflightResp.Header.Get("Access-Control-Allow-Origin") == tc.origin
			if corsAllowed == tc.wantForbidden {
				t.Fatalf("CORS allowed=%v wantForbidden=%v", corsAllowed, tc.wantForbidden)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
			req.Host = "192.168.124.9:17878"
			req.Header.Set("Origin", tc.origin)
			req.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("origin request: %v", err)
			}
			resp.Body.Close()
			if (resp.StatusCode == http.StatusForbidden) != tc.wantForbidden {
				t.Fatalf("status=%d wantForbidden=%v", resp.StatusCode, tc.wantForbidden)
			}
		})
	}
}

func TestExplicitDevModeControlsFixedCode(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.Auth.TOTPSecret = ""
	cfg.Auth.DevAllowFixedCode = true
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	if _, err := NewWithOptions(cfg, st, "", Options{DevFrontendPort: 5173}); err == nil {
		t.Fatalf("expected production mode to reject empty TOTP secret with fixed code enabled")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatalf("existing New constructor must default to production mode")
			}
		}()
		_ = New(cfg, st)
	}()
	app, err := NewWithOptions(cfg, st, "", Options{DevMode: true, DevFrontendPort: 5173})
	if err != nil {
		t.Fatalf("new explicit dev server: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"code":"000000"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("fixed-code dev login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected fixed code in explicit dev mode, got %d", resp.StatusCode)
	}

	realSecretCfg := testConfig(t.TempDir())
	realSecretCfg.Auth.DevAllowFixedCode = true
	realSecretApp, err := NewWithOptions(realSecretCfg, st, "", Options{DevMode: true, DevFrontendPort: 5173})
	if err != nil {
		t.Fatalf("new dev server with real secret: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"code":"000000"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = realSecretApp.Test(req)
	if err != nil {
		t.Fatalf("fixed-code login with real secret: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("real TOTP secret must disable fixed code even in dev mode, got %d", resp.StatusCode)
	}
}

func TestAdminFilePickerListsAndValidatesWithinRoot(t *testing.T) {
	// 文件选择器是管理员路径输入辅助：只列允许根内条目，最终选择仍由后端校验。
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "alpha"), 0755); err != nil {
		t.Fatalf("mkdir alpha: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "docs"), 0755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "zeta"), 0755); err != nil {
		t.Fatalf("mkdir zeta: %v", err)
	}
	if err := osWriteFile(filepath.Join(root, "a-small.txt"), []byte("a")); err != nil {
		t.Fatalf("write small: %v", err)
	}
	if err := osWriteFile(filepath.Join(root, "b-large.txt"), []byte("large-file")); err != nil {
		t.Fatalf("write large: %v", err)
	}
	manualPath := filepath.Join(root, "manual.pdf")
	if err := osWriteFile(manualPath, []byte("manual")); err != nil {
		t.Fatalf("write manual: %v", err)
	}
	if err := osWriteFile(filepath.Join(root, ".env"), []byte("secret")); err != nil {
		t.Fatalf("write env: %v", err)
	}
	symlinkCreated := os.Symlink(outside, filepath.Join(root, "outside-link")) == nil
	cfg := testConfig(root)
	cfg.FilePicker.Roots = []config.FilePickerRoot{{ID: "pick", Name: "可选根", Path: root, AllowSelectFiles: true, AllowSelectDirs: true}}
	app := New(cfg, st)
	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	rootsReq := httptest.NewRequest(http.MethodGet, "/api/config/file-picker/roots", nil)
	rootsReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	rootsResp, err := app.Test(rootsReq)
	if err != nil {
		t.Fatalf("picker roots: %v", err)
	}
	var roots []filePickerRootDTO
	decodeJSON(t, rootsResp, &roots)
	foundRootPath := false
	for _, pickerRoot := range roots {
		if pickerRoot.ID == "pick" && pickerRoot.Path == root {
			foundRootPath = true
		}
	}
	if rootsResp.StatusCode != http.StatusOK || !foundRootPath {
		t.Fatalf("expected picker roots to expose path for address bar, status=%d roots=%+v", rootsResp.StatusCode, roots)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/config/file-picker/list?rootId=pick&pageSize=10", nil)
	listReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	listResp, err := app.Test(listReq)
	if err != nil {
		t.Fatalf("list picker: %v", err)
	}
	var list filePickerListResponse
	decodeJSON(t, listResp, &list)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("picker list status=%d list=%+v", listResp.StatusCode, list)
	}
	seenManual := false
	seenEnv := false
	seenLinkBlocked := !symlinkCreated
	for _, item := range list.Items {
		switch item.Name {
		case "manual.pdf":
			seenManual = item.Selectable && item.Type == config.ResourceFile
		case ".env":
			seenEnv = true
		case "outside-link":
			seenLinkBlocked = item.Symlink && !item.Selectable
		}
	}
	if !seenManual || seenEnv || !seenLinkBlocked {
		t.Fatalf("unexpected picker items: %+v", list.Items)
	}
	if len(list.Items) < 6 || list.Items[0].Name != "alpha" || list.Items[1].Name != "docs" || list.Items[2].Name != "zeta" || list.Items[3].Type != config.ResourceFile {
		t.Fatalf("expected directories first then files by name, got %+v", list.Items)
	}

	sizeSortReq := httptest.NewRequest(http.MethodGet, "/api/config/file-picker/list?rootId=pick&pageSize=10&sort=size&order=desc", nil)
	sizeSortReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	sizeSortResp, err := app.Test(sizeSortReq)
	if err != nil {
		t.Fatalf("size sort picker: %v", err)
	}
	var sizeList filePickerListResponse
	decodeJSON(t, sizeSortResp, &sizeList)
	if sizeSortResp.StatusCode != http.StatusOK || len(sizeList.Items) < 6 || sizeList.Items[0].Type != config.ResourceDirectory || sizeList.Items[3].Name != "b-large.txt" {
		t.Fatalf("expected directories first and files sorted by size desc, status=%d items=%+v", sizeSortResp.StatusCode, sizeList.Items)
	}
	for _, sortBy := range []string{"type", "modifiedAt"} {
		sortReq := httptest.NewRequest(http.MethodGet, "/api/config/file-picker/list?rootId=pick&pageSize=10&sort="+sortBy+"&order=desc", nil)
		sortReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
		sortResp, err := app.Test(sortReq)
		if err != nil {
			t.Fatalf("%s sort picker: %v", sortBy, err)
		}
		var sorted filePickerListResponse
		decodeJSON(t, sortResp, &sorted)
		if sortResp.StatusCode != http.StatusOK || len(sorted.Items) < 6 || sorted.Items[0].Type != config.ResourceDirectory {
			t.Fatalf("expected directories first for %s sort, status=%d items=%+v", sortBy, sortResp.StatusCode, sorted.Items)
		}
	}

	validateReq := httptest.NewRequest(http.MethodPost, "/api/config/file-picker/validate", strings.NewReader(`{"rootId":"pick","path":"/manual.pdf","expectedType":"file"}`))
	validateReq.Header.Set("Content-Type", "application/json")
	validateReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	validateResp, err := app.Test(validateReq)
	if err != nil {
		t.Fatalf("validate picker: %v", err)
	}
	var picked filePickerValidateResponse
	decodeJSON(t, validateResp, &picked)
	if validateResp.StatusCode != http.StatusOK || picked.AbsolutePath != manualPath || picked.RelativePath != "manual.pdf" {
		t.Fatalf("unexpected picker validation: status=%d picked=%+v", validateResp.StatusCode, picked)
	}

	traversalReq := httptest.NewRequest(http.MethodGet, "/api/config/file-picker/list?rootId=pick&path=../", nil)
	traversalReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	traversalResp, err := app.Test(traversalReq)
	if err != nil {
		t.Fatalf("traversal picker: %v", err)
	}
	traversalResp.Body.Close()
	if traversalResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected traversal to be rejected, got %d", traversalResp.StatusCode)
	}
}

func TestAdminFilePickerDoesNotInjectSystemRoot(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(t.TempDir())
	cfg.Storage.Dirs = nil
	cfg.FilePicker.Roots = nil
	app := New(cfg, st)
	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}

	rootsReq := httptest.NewRequest(http.MethodGet, "/api/config/file-picker/roots", nil)
	rootsReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	rootsResp, err := app.Test(rootsReq)
	if err != nil {
		t.Fatalf("roots request: %v", err)
	}
	var roots []filePickerRootDTO
	decodeJSON(t, rootsResp, &roots)
	if rootsResp.StatusCode != http.StatusOK || len(roots) != 0 {
		t.Fatalf("expected only configured picker roots, status=%d roots=%+v", rootsResp.StatusCode, roots)
	}
}

func TestResourceAPIEnforcesCanonicalPickerRootAllowlist(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	insideDir := filepath.Join(root, "inside-dir")
	insideFile := filepath.Join(root, "inside.txt")
	if err := os.Mkdir(insideDir, 0755); err != nil {
		t.Fatalf("mkdir inside: %v", err)
	}
	if err := osWriteFile(insideFile, []byte("inside")); err != nil {
		t.Fatalf("write inside file: %v", err)
	}
	app, _ := newResourcePolicyTestApp(t, []config.FilePickerRoot{{ID: "allowed", Path: root, AllowSelectFiles: true, AllowSelectDirs: true}}, nil, "")
	for _, tc := range []struct {
		id, typ, path string
	}{
		{"inside-dir", "directory", insideDir},
		{"inside-file", "file", insideFile},
	} {
		status, payload := requestResourceChange(t, app, http.MethodPost, "/api/config/resources", fmt.Sprintf(`{"id":%q,"name":"Allowed","type":%q,"path":%q,"allowDownload":true,"allowUpload":false}`, tc.id, tc.typ, tc.path))
		if status != http.StatusOK {
			t.Fatalf("expected root-contained %s resource allowed, status=%d payload=%+v", tc.typ, status, payload)
		}
	}
	outsideDir := filepath.Join(outside, "outside-dir")
	if err := os.Mkdir(outsideDir, 0755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	status, payload := requestResourceChange(t, app, http.MethodPost, "/api/config/resources", fmt.Sprintf(`{"id":"outside","name":"Outside","type":"directory","path":%q,"allowDownload":true,"allowUpload":false}`, outsideDir))
	assertResourcePolicyError(t, status, payload, "resource_path_outside_allowlist", outsideDir)
	status, payload = requestResourceChange(t, app, http.MethodPut, "/api/config/resources/inside-dir", fmt.Sprintf(`{"id":"inside-dir","name":"Moved Outside","type":"directory","path":%q,"allowDownload":true,"allowUpload":false}`, outsideDir))
	assertResourcePolicyError(t, status, payload, "resource_path_outside_allowlist", outsideDir)

	emptyApp, _ := newResourcePolicyTestApp(t, nil, nil, "")
	status, payload = requestResourceChange(t, emptyApp, http.MethodPost, "/api/config/resources", fmt.Sprintf(`{"id":"empty-roots","name":"Empty","type":"directory","path":%q,"allowDownload":true,"allowUpload":false}`, insideDir))
	assertResourcePolicyError(t, status, payload, "resource_path_outside_allowlist", insideDir)

	dirsOnlyApp, _ := newResourcePolicyTestApp(t, []config.FilePickerRoot{{ID: "dirs", Path: root, AllowSelectDirs: true}}, nil, "")
	status, payload = requestResourceChange(t, dirsOnlyApp, http.MethodPost, "/api/config/resources", fmt.Sprintf(`{"id":"file-not-selectable","name":"File","type":"file","path":%q,"allowDownload":true,"allowUpload":false}`, insideFile))
	assertResourcePolicyError(t, status, payload, "resource_path_outside_allowlist", insideFile)
}

func TestResourceAPIUsesCanonicalSymlinkContainment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	root := t.TempDir()
	inside := filepath.Join(root, "inside")
	outside := t.TempDir()
	if err := os.Mkdir(inside, 0755); err != nil {
		t.Fatalf("mkdir inside: %v", err)
	}
	internalLink := filepath.Join(root, "internal-link")
	externalLink := filepath.Join(root, "external-link")
	if err := os.Symlink(inside, internalLink); err != nil {
		t.Skipf("create internal symlink: %v", err)
	}
	if err := os.Symlink(outside, externalLink); err != nil {
		t.Skipf("create external symlink: %v", err)
	}
	app, _ := newResourcePolicyTestApp(t, []config.FilePickerRoot{{ID: "allowed", Path: root, AllowSelectDirs: true, FollowSymlinks: false}}, nil, "")
	status, payload := requestResourceChange(t, app, http.MethodPost, "/api/config/resources", fmt.Sprintf(`{"id":"internal","name":"Internal","type":"directory","path":%q,"allowDownload":true,"allowUpload":false}`, internalLink))
	if status != http.StatusOK {
		t.Fatalf("expected canonical internal symlink allowed, status=%d payload=%+v", status, payload)
	}
	canonicalInside, err := fsutil.Canonical(inside)
	if err != nil || payload["root"] != canonicalInside {
		t.Fatalf("expected stored resource path canonicalized, root=%v want=%q err=%v", payload["root"], canonicalInside, err)
	}
	status, payload = requestResourceChange(t, app, http.MethodPost, "/api/config/resources", fmt.Sprintf(`{"id":"external","name":"External","type":"directory","path":%q,"allowDownload":true,"allowUpload":false}`, externalLink))
	assertResourcePolicyError(t, status, payload, "resource_path_outside_allowlist", externalLink)

	pickerApp, _ := newResourcePolicyTestApp(t, []config.FilePickerRoot{{ID: "allowed", Path: root, AllowSelectDirs: true, FollowSymlinks: true}}, nil, "")
	for _, tc := range []struct {
		path       string
		wantStatus int
	}{
		{"/internal-link", http.StatusOK},
		{"/external-link", http.StatusForbidden},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/config/file-picker/validate", strings.NewReader(fmt.Sprintf(`{"rootId":"allowed","path":%q,"expectedType":"directory"}`, tc.path)))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
		resp, err := pickerApp.Test(req)
		if err != nil {
			t.Fatalf("picker symlink validation: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.wantStatus {
			t.Fatalf("picker path %s status=%d want=%d", tc.path, resp.StatusCode, tc.wantStatus)
		}
	}
}

func TestLegacyOutsideAllowlistResourceCanOnlyTightenOrDelete(t *testing.T) {
	allowed := t.TempDir()
	legacy := t.TempDir()
	existing := config.Dir{ID: "legacy", Name: "Legacy", Type: config.ResourceDirectory, Path: legacy, AllowDownload: true, AllowUpload: true}
	warning, restoreLog := captureTestLog(t)
	app, _ := newResourcePolicyTestApp(t, []config.FilePickerRoot{{ID: "allowed", Path: allowed, AllowSelectDirs: true}}, []config.Dir{existing}, "")
	restoreLog()
	if !strings.Contains(warning.String(), "legacy") || strings.Contains(warning.String(), legacy) {
		t.Fatalf("expected ID-only legacy warning, log=%q", warning.String())
	}
	dirsReq := httptest.NewRequest(http.MethodGet, "/api/dirs", nil)
	dirsReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	dirsResp, err := app.Test(dirsReq)
	if err != nil {
		t.Fatalf("list legacy resource: %v", err)
	}
	var dirs []dirDTO
	decodeJSON(t, dirsResp, &dirs)
	foundLegacy := false
	for _, dir := range dirs {
		foundLegacy = foundLegacy || dir.ID == "legacy"
	}
	if !foundLegacy {
		t.Fatalf("expected legacy resource to remain available, dirs=%+v", dirs)
	}

	status, payload := requestResourceChange(t, app, http.MethodPut, "/api/config/resources/legacy", fmt.Sprintf(`{"id":"legacy","name":"Renamed","type":"directory","path":%q,"allowDownload":true,"allowUpload":false}`, legacy))
	if status != http.StatusOK {
		t.Fatalf("expected legacy rename/tighten allowed, status=%d payload=%+v", status, payload)
	}
	status, payload = requestResourceChange(t, app, http.MethodPut, "/api/config/resources/legacy", fmt.Sprintf(`{"id":"legacy","name":"Disabled","type":"directory","path":%q,"allowDownload":false,"allowUpload":false}`, legacy))
	if status != http.StatusOK {
		t.Fatalf("expected legacy resource to allow full permission tightening, status=%d payload=%+v", status, payload)
	}
	status, payload = requestResourceChange(t, app, http.MethodPut, "/api/config/resources/legacy", fmt.Sprintf(`{"id":"legacy","name":"Renamed","type":"directory","path":%q,"allowDownload":true,"allowUpload":false}`, legacy))
	assertResourcePolicyError(t, status, payload, "resource_path_outside_allowlist", legacy)
	status, payload = requestResourceChange(t, app, http.MethodPut, "/api/config/resources/legacy", fmt.Sprintf(`{"id":"legacy","name":"Moved","type":"directory","path":%q,"allowDownload":false,"allowUpload":false}`, allowed))
	assertResourcePolicyError(t, status, payload, "resource_path_outside_allowlist", allowed)
	status, payload = requestResourceChange(t, app, http.MethodPost, "/api/config/resources", fmt.Sprintf(`{"id":"legacy-copy","name":"Copy","type":"directory","path":%q,"allowDownload":true,"allowUpload":false}`, legacy))
	assertResourcePolicyError(t, status, payload, "resource_path_outside_allowlist", legacy)

	req := httptest.NewRequest(http.MethodDelete, "/api/config/resources/legacy", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("delete legacy resource: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected legacy delete allowed, got %d", resp.StatusCode)
	}
}

func TestResourceAPIProtectsServicePathsAndStaticOverlap(t *testing.T) {
	base := t.TempDir()
	staticDir := filepath.Join(base, "static")
	staticChild := filepath.Join(staticDir, "child")
	uploadInsideStatic := filepath.Join(staticDir, "uploads")
	for _, dir := range []string{staticChild, uploadInsideStatic} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir protected test dir: %v", err)
		}
	}
	app, protected := newResourcePolicyTestApp(t, []config.FilePickerRoot{{ID: "host", Path: string(os.PathSeparator), AllowSelectFiles: true, AllowSelectDirs: true}}, nil, staticDir)
	for i, protectedFile := range protected {
		status, payload := requestResourceChange(t, app, http.MethodPost, "/api/config/resources", fmt.Sprintf(`{"id":"protected-%d","name":"Protected","type":"file","path":%q,"allowDownload":true,"allowUpload":false}`, i, protectedFile))
		assertResourcePolicyError(t, status, payload, "resource_path_protected", protectedFile)
	}
	pickerReq := httptest.NewRequest(http.MethodPost, "/api/config/file-picker/validate", strings.NewReader(fmt.Sprintf(`{"rootId":"host","path":%q,"expectedType":"file"}`, protected[0])))
	pickerReq.Header.Set("Content-Type", "application/json")
	pickerReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	pickerResp, err := app.Test(pickerReq)
	if err != nil {
		t.Fatalf("validate protected picker path: %v", err)
	}
	var pickerPayload map[string]any
	decodeJSON(t, pickerResp, &pickerPayload)
	assertResourcePolicyError(t, pickerResp.StatusCode, pickerPayload, "resource_path_protected", protected[0])
	protectedParent := filepath.Dir(protected[0])
	status, payload := requestResourceChange(t, app, http.MethodPost, "/api/config/resources", fmt.Sprintf(`{"id":"protected-parent","name":"Protected Parent","type":"directory","path":%q,"allowDownload":true,"allowUpload":false}`, protectedParent))
	assertResourcePolicyError(t, status, payload, "resource_path_protected", protectedParent)
	for i, dir := range []string{staticDir, staticChild, base} {
		status, payload = requestResourceChange(t, app, http.MethodPost, "/api/config/resources", fmt.Sprintf(`{"id":"static-overlap-%d","name":"Overlap","type":"directory","path":%q,"allowDownload":true,"allowUpload":true}`, i, dir))
		assertResourcePolicyError(t, status, payload, "resource_path_protected", dir)
	}
	safeDir := t.TempDir()
	status, payload = requestResourceChange(t, app, http.MethodPost, "/api/config/resources", fmt.Sprintf(`{"id":"explicit-root-safe","name":"Safe","type":"directory","path":%q,"allowDownload":true,"allowUpload":false}`, safeDir))
	if status != http.StatusOK {
		t.Fatalf("expected explicit host root to allow safe path, status=%d payload=%+v", status, payload)
	}
}

func TestProtectedResourceValidationFailsClosedOnCanonicalError(t *testing.T) {
	cfg := testConfig(t.TempDir())
	s := &Server{config: cfg, configPath: filepath.Join(t.TempDir(), "config.yaml")}
	err := s.validateProtectedResourcePath(cfg, config.Dir{ID: "invalid", Type: config.ResourceDirectory, Path: "bad\x00path", AllowDownload: true})
	var coded *codedAPIError
	if !errors.As(err, &coded) || coded.code != "resource_path_protected" {
		t.Fatalf("expected canonical failure to fail closed as protected, err=%v", err)
	}
}

func TestUploadCommitsFinalFileWithOwnerOnlyPermission(t *testing.T) {
	// 上传文件默认只允许服务端运行用户读取，避免共享主机上其他系统用户直接读到内容。
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(root)
	app := New(cfg, st)
	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create user session: %v", err)
	}
	body, contentType := multipartUploadBody(t, "readable.txt", []byte("ok"))
	req := httptest.NewRequest(http.MethodPost, "/api/files/upload", body)
	req.Header.Set("Content-Type", contentType)
	req.AddCookie(&http.Cookie{Name: "sid", Value: "user-sid"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("upload request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected upload ok, got %d", resp.StatusCode)
	}
	info, err := os.Stat(filepath.Join(root, "readable.txt"))
	if err != nil {
		t.Fatalf("stat uploaded file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("expected uploaded file mode 0600, got %v", got)
	}
}

func TestUploadDuplicateNamesDoNotOverwrite(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(root)
	app := New(cfg, st)
	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create user session: %v", err)
	}
	for _, content := range []string{"first", "second"} {
		body, contentType := multipartUploadBody(t, "same.txt", []byte(content))
		req := httptest.NewRequest(http.MethodPost, "/api/files/upload", body)
		req.Header.Set("Content-Type", contentType)
		req.AddCookie(&http.Cookie{Name: "sid", Value: "user-sid"})
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("upload request: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected upload ok, got %d", resp.StatusCode)
		}
	}
	first, err := os.ReadFile(filepath.Join(root, "same.txt"))
	if err != nil {
		t.Fatalf("read first file: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(root, "same-1.txt"))
	if err != nil {
		t.Fatalf("read renamed file: %v", err)
	}
	if string(first) != "first" || string(second) != "second" {
		t.Fatalf("duplicate upload overwrote content, first=%q second=%q", first, second)
	}
}

func TestUploadRejectsActualFileSizeOverLimit(t *testing.T) {
	// 单文件大小限制在实际写入时再次执行，不能只依赖 multipart 头里的声明值。
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(root)
	cfg.Storage.UploadMaxMB = 2
	cfg.Storage.UploadMaxFileMB = 1
	app := New(cfg, st)
	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create user session: %v", err)
	}
	body, contentType := multipartUploadBody(t, "too-large.bin", bytes.Repeat([]byte("a"), 1024*1024+1))
	req := httptest.NewRequest(http.MethodPost, "/api/files/upload", body)
	req.Header.Set("Content-Type", contentType)
	req.AddCookie(&http.Cookie{Name: "sid", Value: "user-sid"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("upload request: %v", err)
	}
	assertErrorContains(t, resp, http.StatusRequestEntityTooLarge, "单个文件")
	if _, err := os.Stat(filepath.Join(root, "too-large.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected rejected upload not to leave final file, stat=%v", err)
	}
}

func TestFriendlyMissingPathErrors(t *testing.T) {
	// 不存在路径应返回前端可展示的中文提示，而不是笼统 Bad Request。
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()

	cfg := testConfig(root)
	app := New(cfg, st)
	if err := st.CreateSessionWithIdle("user-sid", time.Now().Add(time.Hour), time.Now().Add(time.Minute), "user", ""); err != nil {
		t.Fatalf("create user session: %v", err)
	}
	listReq := httptest.NewRequest(http.MethodGet, "/api/files/list?dirId=default&path=missing", nil)
	listReq.AddCookie(&http.Cookie{Name: "sid", Value: "user-sid"})
	listResp, err := app.Test(listReq)
	if err != nil {
		t.Fatalf("missing list request: %v", err)
	}
	assertErrorContains(t, listResp, http.StatusNotFound, "路径不存在")

	if err := st.CreateSessionWithIdle("admin-sid", time.Now().Add(time.Hour), time.Now().Add(time.Minute), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	tokenReq := httptest.NewRequest(http.MethodPost, "/api/tokens", strings.NewReader(`{"type":"download","dirId":"default","path":"missing.zip","ttlMinutes":30,"maxUses":1}`))
	tokenReq.Header.Set("Content-Type", "application/json")
	tokenReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	tokenResp, err := app.Test(tokenReq)
	if err != nil {
		t.Fatalf("missing token path request: %v", err)
	}
	assertErrorContains(t, tokenResp, http.StatusNotFound, "下载文件不存在")
}

func TestUploadTokenRequiresDirectoryWhenPathExists(t *testing.T) {
	// 上传令牌可以指向未来目录，但如果路径已经是文件，应在创建时友好拒绝，而不是等公开上传时失败。
	root := t.TempDir()
	if err := osWriteFile(filepath.Join(root, "file.txt"), []byte("x")); err != nil {
		t.Fatalf("write file: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(root)
	app := New(cfg, st)
	if err := st.CreateSessionWithIdle("admin-sid", time.Now().Add(time.Hour), time.Now().Add(time.Minute), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tokens", strings.NewReader(`{"type":"upload","dirId":"default","path":"file.txt","ttlMinutes":30,"maxUses":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("upload token request: %v", err)
	}
	assertErrorContains(t, resp, http.StatusBadRequest, "上传令牌需要指向目录")
}

func TestIdleSessionHeartbeatAndExpiry(t *testing.T) {
	// 空闲过期、心跳续期和宽限期共同决定页面会话是否仍可访问 API。
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()

	cfg := testConfig(t.TempDir())
	cfg.Auth.IdleTimeoutSeconds = 60
	app := New(cfg, st)
	now := time.Now()
	if err := st.CreateSessionWithIdle("idle-sid", now.Add(time.Hour), now.Add(-time.Minute), "user", ""); err != nil {
		t.Fatalf("create idle session: %v", err)
	}
	idleReq := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	idleReq.AddCookie(&http.Cookie{Name: "sid", Value: "idle-sid"})
	idleResp, err := app.Test(idleReq)
	if err != nil {
		t.Fatalf("idle me request: %v", err)
	}
	if idleResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected idle session to be unauthorized, got %d", idleResp.StatusCode)
	}

	if err := st.CreateSessionWithIdle("active-sid", now.Add(time.Hour), now.Add(time.Minute), "user", ""); err != nil {
		t.Fatalf("create active session: %v", err)
	}
	before, err := st.Session("active-sid")
	if err != nil {
		t.Fatalf("load active session: %v", err)
	}
	heartbeatReq := httptest.NewRequest(http.MethodPost, "/api/auth/heartbeat", nil)
	heartbeatReq.AddCookie(&http.Cookie{Name: "sid", Value: "active-sid"})
	heartbeatResp, err := app.Test(heartbeatReq)
	if err != nil {
		t.Fatalf("heartbeat request: %v", err)
	}
	if heartbeatResp.StatusCode != http.StatusOK {
		t.Fatalf("expected heartbeat ok, got %d", heartbeatResp.StatusCode)
	}
	after, err := st.Session("active-sid")
	if err != nil {
		t.Fatalf("reload active session: %v", err)
	}
	if !after.IdleExpiresAt.After(before.IdleExpiresAt) {
		t.Fatalf("expected heartbeat to extend idle expiry: before=%s after=%s", before.IdleExpiresAt, after.IdleExpiresAt)
	}

	if err := st.CreateSessionWithIdle("grace-sid", now.Add(time.Hour), time.Now().Add(-10*time.Second), "user", ""); err != nil {
		t.Fatalf("create grace session: %v", err)
	}
	strictReq := httptest.NewRequest(http.MethodGet, "/api/dirs", nil)
	strictReq.AddCookie(&http.Cookie{Name: "sid", Value: "grace-sid"})
	strictResp, err := app.Test(strictReq)
	if err != nil {
		t.Fatalf("strict idle request: %v", err)
	}
	strictResp.Body.Close()
	if strictResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected ordinary API to reject idle session, got %d", strictResp.StatusCode)
	}
	if _, err := st.Session("grace-sid"); err != nil {
		t.Fatalf("expected grace session to remain for heartbeat recovery: %v", err)
	}
	graceHeartbeatReq := httptest.NewRequest(http.MethodPost, "/api/auth/heartbeat", nil)
	graceHeartbeatReq.AddCookie(&http.Cookie{Name: "sid", Value: "grace-sid"})
	graceHeartbeatResp, err := app.Test(graceHeartbeatReq)
	if err != nil {
		t.Fatalf("grace heartbeat request: %v", err)
	}
	graceHeartbeatResp.Body.Close()
	if graceHeartbeatResp.StatusCode != http.StatusOK {
		t.Fatalf("expected heartbeat within grace to refresh session, got %d", graceHeartbeatResp.StatusCode)
	}
}

func TestDownloadRangeAndLeaseSurviveSessionExpiry(t *testing.T) {
	// 下载票据应独立于页面会话，保证长下载和 Range 续传不被空闲过期打断。
	root := t.TempDir()
	if err := osWriteFile(filepath.Join(root, "test.txt"), []byte("0123456789abcdef")); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	fixedMtime := time.Unix(1700000000, 123456789)
	if err := os.Chtimes(filepath.Join(root, "test.txt"), fixedMtime, fixedMtime); err != nil {
		t.Fatalf("set fixed mtime: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()

	cfg := testConfig(root)
	cfg.Downloads.VerifyHashOnEveryRequest = true
	app := New(cfg, st)
	if err := st.CreateSessionWithIdle("sid", time.Now().Add(time.Hour), time.Now().Add(time.Minute), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	rangeReq := httptest.NewRequest(http.MethodGet, "/api/files/download?dirId=default&path=test.txt", nil)
	rangeReq.Header.Set("Range", "bytes=4-9")
	rangeReq.AddCookie(&http.Cookie{Name: "sid", Value: "sid"})
	rangeResp, err := app.Test(rangeReq)
	if err != nil {
		t.Fatalf("range request: %v", err)
	}
	assertPartialBody(t, rangeResp, "456789")

	leaseReq := httptest.NewRequest(http.MethodPost, "/api/files/download-lease", strings.NewReader(`{"dirId":"default","path":"test.txt"}`))
	leaseReq.Header.Set("Content-Type", "application/json")
	leaseReq.AddCookie(&http.Cookie{Name: "sid", Value: "sid"})
	leaseResp, err := app.Test(leaseReq)
	if err != nil {
		t.Fatalf("create lease request: %v", err)
	}
	var lease downloadLeaseResponse
	decodeJSON(t, leaseResp, &lease)
	if leaseResp.StatusCode != http.StatusOK || lease.URL == "" {
		t.Fatalf("expected lease url, status=%d lease=%+v", leaseResp.StatusCode, lease)
	}
	loadedLease, err := st.DownloadLeaseByHash(leaseHashFromURL(t, lease.URL))
	if err != nil {
		t.Fatalf("load created lease: %v", err)
	}
	if !loadedLease.FileSHA256.Valid || loadedLease.FileSHA256.String == "" {
		t.Fatalf("expected small download lease to include content hash: %+v", loadedLease)
	}
	if err := st.DeleteSession("sid"); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	leaseRangeReq := httptest.NewRequest(http.MethodGet, lease.URL, nil)
	leaseRangeReq.Header.Set("Range", "bytes=10-15")
	leaseRangeResp, err := app.Test(leaseRangeReq)
	if err != nil {
		t.Fatalf("lease range request: %v", err)
	}
	assertPartialBody(t, leaseRangeResp, "abcdef")
	if err := osWriteFile(filepath.Join(root, "test.txt"), []byte("fedcba9876543210")); err != nil {
		t.Fatalf("replace test file: %v", err)
	}
	if err := os.Chtimes(filepath.Join(root, "test.txt"), fixedMtime, fixedMtime); err != nil {
		t.Fatalf("restore replaced file mtime: %v", err)
	}
	changedContentReq := httptest.NewRequest(http.MethodGet, lease.URL, nil)
	changedContentResp, err := app.Test(changedContentReq)
	if err != nil {
		t.Fatalf("changed content lease request: %v", err)
	}
	changedContentResp.Body.Close()
	if changedContentResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected same-size same-mtime content replacement to be rejected, got %d", changedContentResp.StatusCode)
	}
	logs, err := st.AuditLogs(20)
	if err != nil {
		t.Fatalf("audit logs after content mismatch: %v", err)
	}
	foundHashMismatch := false
	for _, log := range logs {
		if log.Action == "download_lease_file_changed" && strings.Contains(log.Detail, "内容哈希不匹配") {
			foundHashMismatch = true
		}
	}
	if !foundHashMismatch {
		t.Fatalf("expected content hash mismatch audit log, got %+v", logs)
	}

	staleReq := httptest.NewRequest(http.MethodGet, "/api/files/download?dirId=default&path=test.txt", nil)
	staleReq.Header.Set("Range", "bytes=10-15")
	staleReq.AddCookie(&http.Cookie{Name: "sid", Value: "sid"})
	staleResp, err := app.Test(staleReq)
	if err != nil {
		t.Fatalf("stale range request: %v", err)
	}
	if staleResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected stale session range to be unauthorized, got %d", staleResp.StatusCode)
	}
}

func TestLegacyEmptyResourceFingerprintsAreRejectedWithoutPathLeak(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "legacy.txt")
	if err := osWriteFile(filePath, []byte("legacy")); err != nil {
		t.Fatalf("write legacy file: %v", err)
	}
	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("stat legacy file: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(root)
	app := New(cfg, st)
	legacyToken := &store.Token{Hash: security.HashToken("legacy-token"), Type: "download", DirID: "default", Path: "legacy.txt", MaxUses: 2, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))}
	if err := st.CreateToken(legacyToken); err != nil {
		t.Fatalf("create legacy token: %v", err)
	}
	legacyLease := &store.DownloadLease{Hash: security.HashToken("legacy-lease"), Source: "session", DirID: "default", Path: "legacy.txt", FileSize: info.Size(), FileMtime: normalizedFileMtime(info), FileSHA256: sql.NullString{String: "", Valid: true}, ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.CreateDownloadLease(legacyLease); err != nil {
		t.Fatalf("create legacy lease: %v", err)
	}

	infoReq := httptest.NewRequest(http.MethodGet, "/t/legacy-token/info", nil)
	infoResp, err := app.Test(infoReq)
	if err != nil {
		t.Fatalf("legacy token info: %v", err)
	}
	var tokenInfo map[string]any
	decodeJSON(t, infoResp, &tokenInfo)
	if tokenInfo["valid"] != false || tokenInfo["reason"] != "resource_binding_invalid" {
		t.Fatalf("unexpected legacy token info: %+v", tokenInfo)
	}
	if _, leaked := tokenInfo["path"]; leaked || strings.Contains(fmt.Sprint(tokenInfo), root) {
		t.Fatalf("legacy token info leaked a path: %+v", tokenInfo)
	}

	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	listReq := httptest.NewRequest(http.MethodGet, "/api/tokens", nil)
	listReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	listResp, err := app.Test(listReq)
	if err != nil {
		t.Fatalf("list legacy token: %v", err)
	}
	var listed []tokenDTO
	decodeJSON(t, listResp, &listed)
	if len(listed) != 1 || listed[0].Valid || listed[0].Reason != "resource_binding_invalid" {
		t.Fatalf("unexpected listed legacy token: %+v", listed)
	}

	downloadReq := httptest.NewRequest(http.MethodGet, "/t/legacy-token/download", nil)
	downloadResp, err := app.Test(downloadReq)
	if err != nil {
		t.Fatalf("legacy token download: %v", err)
	}
	downloadResp.Body.Close()
	if downloadResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected legacy token to be forbidden, got %d", downloadResp.StatusCode)
	}
	leaseReq := httptest.NewRequest(http.MethodGet, "/api/files/download-by-lease?lease=legacy-lease", nil)
	leaseResp, err := app.Test(leaseReq)
	if err != nil {
		t.Fatalf("legacy lease download: %v", err)
	}
	assertErrorContains(t, leaseResp, http.StatusForbidden, "资源已变化")
	if _, err := st.TokenByID(legacyToken.ID); err != nil {
		t.Fatalf("expected rejected legacy token record to remain: %v", err)
	}
	if _, err := st.DownloadLeaseByHash(security.HashToken("legacy-lease")); err != nil {
		t.Fatalf("expected rejected legacy lease record to remain: %v", err)
	}
}

func TestTokenStatusReasonsTakePriorityOverEmptyResourceFingerprint(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(t.TempDir())
	app := New(cfg, st)
	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	tokens := []*store.Token{
		{Hash: security.HashToken("empty-revoked"), Type: "upload", DirID: "default", MaxUses: 0, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))},
		{Hash: security.HashToken("empty-expired"), Type: "upload", DirID: "default", MaxUses: 0, ExpiresAt: sqlNullTime(time.Now().Add(-time.Minute))},
		{Hash: security.HashToken("empty-exhausted"), Type: "upload", DirID: "default", MaxUses: 1, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))},
	}
	for _, token := range tokens {
		if err := st.CreateToken(token); err != nil {
			t.Fatalf("create token: %v", err)
		}
	}
	if err := st.Revoke(strconv.FormatInt(tokens[0].ID, 10)); err != nil {
		t.Fatalf("revoke token: %v", err)
	}
	if _, err := st.ReserveTokenUse(security.HashToken("empty-exhausted"), "upload", time.Now(), 0, 0); err != nil {
		t.Fatalf("exhaust token: %v", err)
	}

	for plain, want := range map[string]string{"empty-revoked": "revoked", "empty-expired": "expired", "empty-exhausted": "exhausted"} {
		req := httptest.NewRequest(http.MethodGet, "/t/"+plain+"/info", nil)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("token info %s: %v", plain, err)
		}
		var info map[string]any
		decodeJSON(t, resp, &info)
		if info["reason"] != want {
			t.Fatalf("expected %s reason for %s, got %+v", want, plain, info)
		}
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/tokens", nil)
	listReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	listResp, err := app.Test(listReq)
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	var listed []tokenDTO
	decodeJSON(t, listResp, &listed)
	seen := map[string]bool{}
	for _, token := range listed {
		seen[token.Reason] = true
	}
	for _, reason := range []string{"revoked", "expired", "exhausted"} {
		if !seen[reason] {
			t.Fatalf("expected list to preserve %s reason, got %+v", reason, listed)
		}
	}
}

func TestUploadTokensRejectEmptyFingerprintAndChangedRoot(t *testing.T) {
	base := t.TempDir()
	oldRoot := filepath.Join(base, "old")
	newRoot := filepath.Join(base, "new")
	for _, root := range []string{oldRoot, newRoot} {
		if err := os.MkdirAll(root, 0755); err != nil {
			t.Fatalf("mkdir root: %v", err)
		}
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	oldCfg := testConfig(oldRoot)
	empty := &store.Token{Hash: security.HashToken("empty-upload"), Type: "upload", DirID: "default", Path: "", MaxUses: 1, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))}
	changed := &store.Token{Hash: security.HashToken("changed-upload"), Type: "upload", DirID: "default", Path: "", ResourceFingerprint: testResourceFingerprint(t, oldCfg, "default"), MaxUses: 1, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))}
	for _, token := range []*store.Token{empty, changed} {
		if err := st.CreateToken(token); err != nil {
			t.Fatalf("create upload token: %v", err)
		}
	}
	oldApp := New(oldCfg, st)
	infoReq := httptest.NewRequest(http.MethodGet, "/t/empty-upload/info", nil)
	infoResp, err := oldApp.Test(infoReq)
	if err != nil {
		t.Fatalf("empty upload info: %v", err)
	}
	var info map[string]any
	decodeJSON(t, infoResp, &info)
	if info["reason"] != "resource_binding_invalid" {
		t.Fatalf("expected empty upload fingerprint failure, got %+v", info)
	}
	emptyLeaseReq := httptest.NewRequest(http.MethodPost, "/t/empty-upload/upload-lease", strings.NewReader(`{"fileName":"empty.txt","fileSize":1}`))
	emptyLeaseReq.Header.Set("Content-Type", "application/json")
	emptyLeaseResp, err := oldApp.Test(emptyLeaseReq)
	if err != nil {
		t.Fatalf("empty upload lease: %v", err)
	}
	emptyLeaseResp.Body.Close()
	if emptyLeaseResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected empty upload token forbidden, got %d", emptyLeaseResp.StatusCode)
	}

	changedApp := New(testConfig(newRoot), st)
	changedLeaseReq := httptest.NewRequest(http.MethodPost, "/t/changed-upload/upload-lease", strings.NewReader(`{"fileName":"changed.txt","fileSize":1}`))
	changedLeaseReq.Header.Set("Content-Type", "application/json")
	changedLeaseResp, err := changedApp.Test(changedLeaseReq)
	if err != nil {
		t.Fatalf("changed-root upload lease: %v", err)
	}
	changedLeaseResp.Body.Close()
	if changedLeaseResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected changed-root upload token forbidden, got %d", changedLeaseResp.StatusCode)
	}
}

func TestResourceFingerprintRejectsChangedRootAndPermissionAfterRestart(t *testing.T) {
	base := t.TempDir()
	oldRoot := filepath.Join(base, "old")
	newRoot := filepath.Join(base, "new")
	for _, root := range []string{oldRoot, newRoot} {
		if err := os.MkdirAll(root, 0755); err != nil {
			t.Fatalf("mkdir root: %v", err)
		}
		if err := osWriteFile(filepath.Join(root, "same.txt"), []byte("same")); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	oldCfg := testConfig(oldRoot)
	token := &store.Token{Hash: security.HashToken("restart-token"), Type: "download", DirID: "default", Path: "same.txt", ResourceFingerprint: testResourceFingerprint(t, oldCfg, "default"), MaxUses: 2, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))}
	if err := st.CreateToken(token); err != nil {
		t.Fatalf("create token: %v", err)
	}
	oldInfo, err := os.Stat(filepath.Join(oldRoot, "same.txt"))
	if err != nil {
		t.Fatalf("stat old file: %v", err)
	}
	oldLease := &store.DownloadLease{Hash: security.HashToken("restart-lease"), Source: "session", DirID: "default", Path: "same.txt", ResourceFingerprint: testResourceFingerprint(t, oldCfg, "default"), FileSize: oldInfo.Size(), FileMtime: normalizedFileMtime(oldInfo), FileSHA256: sql.NullString{String: "", Valid: true}, ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.CreateDownloadLease(oldLease); err != nil {
		t.Fatalf("create old-root lease: %v", err)
	}

	changedRootApp := New(testConfig(newRoot), st)
	infoReq := httptest.NewRequest(http.MethodGet, "/t/restart-token/info", nil)
	infoResp, err := changedRootApp.Test(infoReq)
	if err != nil {
		t.Fatalf("changed-root info: %v", err)
	}
	var changedInfo map[string]any
	decodeJSON(t, infoResp, &changedInfo)
	if changedInfo["valid"] != false || changedInfo["reason"] != "resource_binding_invalid" {
		t.Fatalf("expected changed root binding failure, got %+v", changedInfo)
	}
	reserveReq := httptest.NewRequest(http.MethodPost, "/t/restart-token/download-lease", nil)
	reserveResp, err := changedRootApp.Test(reserveReq)
	if err != nil {
		t.Fatalf("changed-root reserve: %v", err)
	}
	reserveResp.Body.Close()
	if reserveResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected changed-root token forbidden, got %d", reserveResp.StatusCode)
	}
	loaded, err := st.TokenByID(token.ID)
	if err != nil || loaded.Uses != 0 {
		t.Fatalf("expected rejected token reservation rolled back, token=%+v err=%v", loaded, err)
	}
	leaseReq := httptest.NewRequest(http.MethodGet, "/api/files/download-by-lease?lease=restart-lease", nil)
	leaseResp, err := changedRootApp.Test(leaseReq)
	if err != nil {
		t.Fatalf("changed-root lease: %v", err)
	}
	assertErrorContains(t, leaseResp, http.StatusForbidden, "资源已变化")

	permissionCfg := testConfig(oldRoot)
	permissionCfg.Storage.Dirs[0].AllowDownload = false
	permissionApp := New(permissionCfg, st)
	permissionInfoReq := httptest.NewRequest(http.MethodGet, "/t/restart-token/info", nil)
	permissionInfoResp, err := permissionApp.Test(permissionInfoReq)
	if err != nil {
		t.Fatalf("permission-change info: %v", err)
	}
	var permissionInfo map[string]any
	decodeJSON(t, permissionInfoResp, &permissionInfo)
	if permissionInfo["valid"] != false || permissionInfo["reason"] != "resource_binding_invalid" {
		t.Fatalf("expected permission binding failure, got %+v", permissionInfo)
	}
}

func TestUnchangedResourceFingerprintSurvivesRestartAndRange(t *testing.T) {
	root := t.TempDir()
	if err := osWriteFile(filepath.Join(root, "stable.txt"), []byte("0123456789")); err != nil {
		t.Fatalf("write stable file: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(root)
	app := New(cfg, st)
	if err := st.CreateSession("sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	leaseReq := httptest.NewRequest(http.MethodPost, "/api/files/download-lease", strings.NewReader(`{"dirId":"default","path":"stable.txt"}`))
	leaseReq.Header.Set("Content-Type", "application/json")
	leaseReq.AddCookie(&http.Cookie{Name: "sid", Value: "sid"})
	leaseResp, err := app.Test(leaseReq)
	if err != nil {
		t.Fatalf("create stable lease: %v", err)
	}
	var lease downloadLeaseResponse
	decodeJSON(t, leaseResp, &lease)
	stored, err := st.DownloadLeaseByHash(leaseHashFromURL(t, lease.URL))
	if err != nil || stored.ResourceFingerprint == "" {
		t.Fatalf("expected download lease fingerprint, lease=%+v err=%v", stored, err)
	}
	if err := st.DeleteSession("sid"); err != nil {
		t.Fatalf("expire page session: %v", err)
	}
	restarted := New(testConfig(root), st)
	token := &store.Token{Hash: security.HashToken("stable-token"), Type: "download", DirID: "default", Path: "stable.txt", ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), MaxUses: 1, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))}
	if err := st.CreateToken(token); err != nil {
		t.Fatalf("create stable token: %v", err)
	}
	infoReq := httptest.NewRequest(http.MethodGet, "/t/stable-token/info", nil)
	infoResp, err := restarted.Test(infoReq)
	if err != nil {
		t.Fatalf("stable token info after restart: %v", err)
	}
	var tokenInfo map[string]any
	decodeJSON(t, infoResp, &tokenInfo)
	if tokenInfo["valid"] != true {
		t.Fatalf("expected unchanged token to remain valid, got %+v", tokenInfo)
	}
	rangeReq := httptest.NewRequest(http.MethodGet, lease.URL, nil)
	rangeReq.Header.Set("Range", "bytes=3-6")
	rangeResp, err := restarted.Test(rangeReq)
	if err != nil {
		t.Fatalf("stable range after restart: %v", err)
	}
	assertPartialBody(t, rangeResp, "3456")
}

func TestDownloadLeaseSkipsHashAboveConfiguredThreshold(t *testing.T) {
	// 大文件默认跳过内容哈希，避免创建票据和续传校验时反复读取完整文件。
	root := t.TempDir()
	large := bytes.Repeat([]byte("a"), 1024*1024+1)
	if err := osWriteFile(filepath.Join(root, "large.bin"), large); err != nil {
		t.Fatalf("write large file: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()

	cfg := testConfig(root)
	cfg.Downloads.ContentHashMaxMB = 1
	app := New(cfg, st)
	if err := st.CreateSessionWithIdle("sid-large", time.Now().Add(time.Hour), time.Now().Add(time.Minute), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	leaseReq := httptest.NewRequest(http.MethodPost, "/api/files/download-lease", strings.NewReader(`{"dirId":"default","path":"large.bin"}`))
	leaseReq.Header.Set("Content-Type", "application/json")
	leaseReq.AddCookie(&http.Cookie{Name: "sid", Value: "sid-large"})
	leaseResp, err := app.Test(leaseReq)
	if err != nil {
		t.Fatalf("create large lease request: %v", err)
	}
	var lease downloadLeaseResponse
	decodeJSON(t, leaseResp, &lease)
	if leaseResp.StatusCode != http.StatusOK || lease.URL == "" {
		t.Fatalf("expected large lease url, status=%d lease=%+v", leaseResp.StatusCode, lease)
	}
	loadedLease, err := st.DownloadLeaseByHash(leaseHashFromURL(t, lease.URL))
	if err != nil {
		t.Fatalf("load large lease: %v", err)
	}
	if !loadedLease.FileSHA256.Valid || loadedLease.FileSHA256.String != "" {
		t.Fatalf("expected large lease to store an empty hash marker, got %+v", loadedLease.FileSHA256)
	}
	largeReq := httptest.NewRequest(http.MethodGet, lease.URL, nil)
	largeReq.Header.Set("Range", "bytes=0-3")
	largeResp, err := app.Test(largeReq)
	if err != nil {
		t.Fatalf("large lease range request: %v", err)
	}
	assertPartialBody(t, largeResp, "aaaa")

	tok := &store.Token{Hash: security.HashToken("large-token"), Type: "download", DirID: "default", Path: "large.bin", ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), MaxUses: 1, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))}
	if err := st.CreateToken(tok); err != nil {
		t.Fatalf("create large public token: %v", err)
	}
	publicLeaseReq := httptest.NewRequest(http.MethodPost, "/t/large-token/download-lease", nil)
	publicLeaseResp, err := app.Test(publicLeaseReq)
	if err != nil {
		t.Fatalf("create public large lease request: %v", err)
	}
	var publicLease downloadLeaseResponse
	decodeJSON(t, publicLeaseResp, &publicLease)
	if publicLeaseResp.StatusCode != http.StatusOK || publicLease.URL == "" {
		t.Fatalf("expected public large lease url, status=%d lease=%+v", publicLeaseResp.StatusCode, publicLease)
	}
	loadedPublicLease, err := st.DownloadLeaseByHash(leaseHashFromURL(t, publicLease.URL))
	if err != nil {
		t.Fatalf("load public large lease: %v", err)
	}
	if loadedPublicLease.ResourceFingerprint != testResourceFingerprint(t, cfg, "default") {
		t.Fatalf("expected public download lease resource fingerprint, got %q", loadedPublicLease.ResourceFingerprint)
	}
	if !loadedPublicLease.FileSHA256.Valid || loadedPublicLease.FileSHA256.String != "" {
		t.Fatalf("expected public large lease to skip content hash, got %+v", loadedPublicLease.FileSHA256)
	}
}

func TestPublicDownloadLeaseConsumesTokenOnceAndSupportsRange(t *testing.T) {
	// 公开下载只在兑换票据时扣一次 uses，同一票据的 Range 请求不重复扣次。
	root := t.TempDir()
	if err := osWriteFile(filepath.Join(root, "public.txt"), []byte("0123456789abcdef")); err != nil {
		t.Fatalf("write public file: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()

	cfg := testConfig(root)
	app := New(cfg, st)
	tok := &store.Token{Hash: security.HashToken("public-token"), Type: "download", DirID: "default", Path: "public.txt", ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), MaxUses: 1, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))}
	if err := st.CreateToken(tok); err != nil {
		t.Fatalf("create token: %v", err)
	}
	legacyReq := httptest.NewRequest(http.MethodGet, "/t/public-token/download", nil)
	legacyResp, err := app.Test(legacyReq)
	if err != nil {
		t.Fatalf("legacy public download page request: %v", err)
	}
	legacyResp.Body.Close()
	if legacyResp.StatusCode != http.StatusOK {
		t.Fatalf("expected legacy download page to be shown without consuming token, got %d", legacyResp.StatusCode)
	}
	unused, err := st.TokenByHash(security.HashToken("public-token"))
	if err != nil {
		t.Fatalf("reload token after legacy page: %v", err)
	}
	if unused.Uses != 0 {
		t.Fatalf("expected legacy confirmation page not to consume token, got uses=%d", unused.Uses)
	}
	leaseReq := httptest.NewRequest(http.MethodPost, "/t/public-token/download-lease", nil)
	leaseResp, err := app.Test(leaseReq)
	if err != nil {
		t.Fatalf("public lease request: %v", err)
	}
	var lease downloadLeaseResponse
	decodeJSON(t, leaseResp, &lease)
	if leaseResp.StatusCode != http.StatusOK || lease.URL == "" {
		t.Fatalf("expected public lease url, status=%d lease=%+v", leaseResp.StatusCode, lease)
	}
	used, err := st.TokenByHash(security.HashToken("public-token"))
	if err != nil {
		t.Fatalf("reload token: %v", err)
	}
	if used.Uses != 1 {
		t.Fatalf("expected token uses to be 1 after lease creation, got %d", used.Uses)
	}
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, lease.URL, nil)
		req.Header.Set("Range", "bytes=4-9")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("public lease range request %d: %v", i, err)
		}
		assertPartialBody(t, resp, "456789")
	}
	used, err = st.TokenByHash(security.HashToken("public-token"))
	if err != nil {
		t.Fatalf("reload token after range: %v", err)
	}
	if used.Uses != 1 {
		t.Fatalf("expected range requests not to consume token again, got uses=%d", used.Uses)
	}
	logs, err := st.AuditLogs(20)
	if err != nil {
		t.Fatalf("audit logs: %v", err)
	}
	leaseUseCount := 0
	for _, log := range logs {
		if log.Action == "download_lease_use" {
			leaseUseCount++
		}
	}
	if leaseUseCount != 1 {
		t.Fatalf("expected one first-use lease audit log, got %d", leaseUseCount)
	}
	secondReq := httptest.NewRequest(http.MethodPost, "/t/public-token/download-lease", nil)
	secondResp, err := app.Test(secondReq)
	if err != nil {
		t.Fatalf("second public lease request: %v", err)
	}
	if secondResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected exhausted token to be forbidden, got %d", secondResp.StatusCode)
	}
	if err := st.CreateSessionWithIdle("admin-sid", time.Now().Add(time.Hour), time.Now().Add(time.Minute), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	revokeReq := httptest.NewRequest(http.MethodPost, "/api/tokens/"+strconv.FormatInt(tok.ID, 10)+"/revoke", nil)
	revokeReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	revokeResp, err := app.Test(revokeReq)
	if err != nil {
		t.Fatalf("revoke token request: %v", err)
	}
	revokeResp.Body.Close()
	if revokeResp.StatusCode != http.StatusOK {
		t.Fatalf("expected revoke ok, got %d", revokeResp.StatusCode)
	}
	revokedLeaseReq := httptest.NewRequest(http.MethodGet, lease.URL, nil)
	revokedLeaseResp, err := app.Test(revokedLeaseReq)
	if err != nil {
		t.Fatalf("revoked lease request: %v", err)
	}
	revokedLeaseResp.Body.Close()
	if revokedLeaseResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected revoked token lease to be removed, got %d", revokedLeaseResp.StatusCode)
	}

	deleteTok := &store.Token{Hash: security.HashToken("delete-token"), Type: "download", DirID: "default", Path: "public.txt", ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), MaxUses: 0, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))}
	if err := st.CreateToken(deleteTok); err != nil {
		t.Fatalf("create delete token: %v", err)
	}
	deleteLeaseReq := httptest.NewRequest(http.MethodPost, "/t/delete-token/download-lease", nil)
	deleteLeaseResp, err := app.Test(deleteLeaseReq)
	if err != nil {
		t.Fatalf("delete token lease request: %v", err)
	}
	var deleteLease downloadLeaseResponse
	decodeJSON(t, deleteLeaseResp, &deleteLease)
	if deleteLeaseResp.StatusCode != http.StatusOK || deleteLease.URL == "" {
		t.Fatalf("expected delete token lease url, status=%d lease=%+v", deleteLeaseResp.StatusCode, deleteLease)
	}
	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/tokens/"+strconv.FormatInt(deleteTok.ID, 10), nil)
	deleteReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	deleteResp, err := app.Test(deleteReq)
	if err != nil {
		t.Fatalf("delete token request: %v", err)
	}
	deleteResp.Body.Close()
	if deleteResp.StatusCode != http.StatusOK {
		t.Fatalf("expected delete token ok, got %d", deleteResp.StatusCode)
	}
	deletedLeaseReq := httptest.NewRequest(http.MethodGet, deleteLease.URL, nil)
	deletedLeaseResp, err := app.Test(deletedLeaseReq)
	if err != nil {
		t.Fatalf("deleted lease request: %v", err)
	}
	deletedLeaseResp.Body.Close()
	if deletedLeaseResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected deleted token lease to be removed, got %d", deletedLeaseResp.StatusCode)
	}
}

func TestPublicDownloadLeaseExpiresNoLaterThanToken(t *testing.T) {
	// 公开 token 的过期时间是对外授权边界，兑换出的下载票据不能比 token 本身活得更久。
	root := t.TempDir()
	if err := osWriteFile(filepath.Join(root, "short.txt"), []byte("short")); err != nil {
		t.Fatalf("write public file: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(root)
	cfg.Downloads.LeaseTTLSeconds = 7200
	app := New(cfg, st)
	tokenExpiresAt := time.Now().Add(2 * time.Minute)
	tok := &store.Token{Hash: security.HashToken("short-token"), Type: "download", DirID: "default", Path: "short.txt", ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), MaxUses: 1, ExpiresAt: sqlNullTime(tokenExpiresAt)}
	if err := st.CreateToken(tok); err != nil {
		t.Fatalf("create token: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/t/short-token/download-lease", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("public lease request: %v", err)
	}
	var lease downloadLeaseResponse
	decodeJSON(t, resp, &lease)
	if resp.StatusCode != http.StatusOK || lease.URL == "" {
		t.Fatalf("expected public lease url, status=%d lease=%+v", resp.StatusCode, lease)
	}
	if lease.ExpiresAt.After(tokenExpiresAt.Add(time.Second)) {
		t.Fatalf("expected lease to expire no later than token: lease=%s token=%s", lease.ExpiresAt, tokenExpiresAt)
	}
}

func TestValidateLoginCodeAcceptsAdjacentTOTPWindow(t *testing.T) {
	// TOTP 允许相邻窗口，降低客户端和服务器时间轻微偏移导致的登录失败。
	cfg := config.Default()
	cfg.Auth.TOTPSecret = "JBSWY3DPEHPK3PXP"
	cfg.Auth.Admin.Username = "admin"
	setTestAdminPassword(cfg)
	s := &Server{config: cfg}

	code, err := totp.GenerateCode(cfg.Auth.TOTPSecret, time.Now().Add(-30*time.Second))
	if err != nil {
		t.Fatalf("generate previous-window totp: %v", err)
	}
	if !s.validateLoginCode(code) {
		t.Fatalf("expected previous-window totp to be accepted")
	}
	if !s.validateLoginCode(" " + code[:3] + " " + code[3:] + " ") {
		t.Fatalf("expected formatted totp to be normalized and accepted")
	}
}

func TestTokenExpiryClampsToConfiguredMaxTTL(t *testing.T) {
	// 管理员误填过长有效期时，后端仍把公开令牌夹紧到配置上限。
	cfg := testConfig(t.TempDir())
	cfg.Tokens.DefaultTTLSeconds = 3600
	cfg.Tokens.MaxTTLSeconds = 7200
	farFuture := time.Now().Add(365 * 24 * time.Hour)
	got := tokenExpiry(cfg, tokenRequest{ExpiresAt: &farFuture})
	if !got.Valid {
		t.Fatalf("expected valid expiry")
	}
	if time.Until(got.Time) > 7200*time.Second+time.Second {
		t.Fatalf("expected explicit expiry to be clamped to max ttl, got %s", got.Time)
	}
	got = tokenExpiry(cfg, tokenRequest{TTLSeconds: 999999})
	if time.Until(got.Time) > 7200*time.Second+time.Second {
		t.Fatalf("expected ttl seconds to be clamped to max ttl, got %s", got.Time)
	}
}

func TestLoginLimiterReservesAttemptsAtomically(t *testing.T) {
	// 限速检查和次数消耗在同一把锁内完成，避免窗口边界并发请求全部放行。
	limiter := newLoginLimiter()
	for i := 0; i < loginMaxFailures; i++ {
		if !limiter.reserve("ip") {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
	}
	if limiter.reserve("ip") {
		t.Fatalf("expected attempt after max failures to be rate limited")
	}
	limiter.reset("ip")
	if !limiter.reserve("ip") {
		t.Fatalf("expected reset limiter to allow new attempt")
	}
}

func TestUploadLeaseSurvivesSessionDeletion(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app := New(testConfig(root), st)
	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/files/upload-lease", strings.NewReader(`{"dirId":"default","path":"","fileName":"lease.txt","fileSize":5}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "sid", Value: "user-sid"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("create lease: %v", err)
	}
	var lease uploadLeaseResponse
	decodeJSON(t, resp, &lease)
	if lease.Lease == "" || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected upload lease, status=%d lease=%+v", resp.StatusCode, lease)
	}
	if strings.Contains(lease.UploadURL, "lease=") {
		t.Fatalf("upload lease URL must not expose bearer token in query: %s", lease.UploadURL)
	}
	if err := st.DeleteSession("user-sid"); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	part, err := w.CreateFormFile("files", "ignored.txt")
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	_, _ = part.Write([]byte("hello"))
	_ = w.Close()
	uploadReq := httptest.NewRequest(http.MethodPost, lease.UploadURL, body)
	uploadReq.Header.Set("Content-Type", w.FormDataContentType())
	uploadReq.Header.Set("Authorization", "Bearer "+lease.Lease)
	uploadResp, err := app.Test(uploadReq)
	if err != nil {
		t.Fatalf("upload by lease: %v", err)
	}
	uploadResp.Body.Close()
	if uploadResp.StatusCode != http.StatusOK {
		t.Fatalf("expected lease upload ok, got %d", uploadResp.StatusCode)
	}
	data, err := os.ReadFile(filepath.Join(root, "lease.txt"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("expected uploaded content, data=%q err=%v", data, err)
	}
	assertNoUploadTempFiles(t, root)
}

func TestUploadLeaseDoesNotRegisterTransferBeforeBodyUpload(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	s := &Server{config: testConfig(root), store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry()}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler, StreamRequestBody: true})
	s.routes(app)
	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/files/upload-lease", strings.NewReader(`{"dirId":"default","path":"","fileName":"visible.bin","fileSize":7}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "sid", Value: "user-sid"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("create lease: %v", err)
	}
	var lease uploadLeaseResponse
	decodeJSON(t, resp, &lease)
	if resp.StatusCode != http.StatusOK || lease.Lease == "" {
		t.Fatalf("expected upload lease, status=%d lease=%+v", resp.StatusCode, lease)
	}
	if items := s.transfers.list(); len(items) != 0 {
		t.Fatalf("expected lease creation not to pre-register active transfer, got %+v", items)
	}
}

func TestRawUploadLeaseSuccessAndSingleUse(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app := New(testConfig(root), st)
	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	lease := createTestUploadLease(t, app, "raw.txt", int64(len("hello raw")))
	if lease.RawUploadURL != "/api/files/upload-raw-by-lease" || lease.UploadURL != "/api/files/upload-by-lease" {
		t.Fatalf("unexpected upload urls: %+v", lease)
	}
	req := httptest.NewRequest(http.MethodPost, lease.RawUploadURL, strings.NewReader("hello raw"))
	req.Header.Set("Authorization", "Bearer "+lease.Lease)
	req.ContentLength = int64(len("hello raw"))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("raw upload: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected raw upload ok, got %d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()
	data, err := os.ReadFile(filepath.Join(root, "raw.txt"))
	if err != nil || string(data) != "hello raw" {
		t.Fatalf("expected uploaded raw content, data=%q err=%v", data, err)
	}
	assertNoUploadTempFiles(t, root)
	retry := httptest.NewRequest(http.MethodPost, lease.RawUploadURL, strings.NewReader("hello raw"))
	retry.Header.Set("Authorization", "Bearer "+lease.Lease)
	retry.ContentLength = int64(len("hello raw"))
	retryResp, err := app.Test(retry)
	if err != nil {
		t.Fatalf("raw retry: %v", err)
	}
	retryResp.Body.Close()
	if retryResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected used lease retry to be unauthorized, got %d", retryResp.StatusCode)
	}
}

func TestRawUploadLengthMismatchDoesNotConsumeLease(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app := New(testConfig(root), st)
	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	lease := createTestUploadLease(t, app, "bad-len.txt", 5)
	req := httptest.NewRequest(http.MethodPost, lease.RawUploadURL, strings.NewReader("hey"))
	req.Header.Set("Authorization", "Bearer "+lease.Lease)
	req.ContentLength = 3
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("raw length mismatch: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected length mismatch bad request, got %d", resp.StatusCode)
	}
	stored, err := st.UploadLeaseByHash(security.HashToken(lease.Lease))
	if err != nil || stored.UsedAt.Valid {
		t.Fatalf("expected length mismatch not to consume lease, lease=%+v err=%v", stored, err)
	}
	if _, err := os.Stat(filepath.Join(root, "bad-len.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected no final file, stat=%v", err)
	}
	zero := createTestUploadLease(t, app, "zero.txt", 0)
	zeroReq := httptest.NewRequest(http.MethodPost, zero.RawUploadURL, http.NoBody)
	zeroReq.Header.Set("Authorization", "Bearer "+zero.Lease)
	zeroReq.ContentLength = 0
	zeroResp, err := app.Test(zeroReq)
	if err != nil {
		t.Fatalf("zero raw upload: %v", err)
	}
	zeroResp.Body.Close()
	if zeroResp.StatusCode != http.StatusOK {
		t.Fatalf("expected zero-byte upload ok, got %d", zeroResp.StatusCode)
	}
}

func TestRawUploadVisibleAndCancelableDuringTransfer(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app := New(testConfig(root), st)
	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create user session: %v", err)
	}
	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	const slowTotal = int64(2 * 1024 * 1024)
	lease := createTestUploadLease(t, app, "slow-raw.bin", slowTotal)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serverErr := make(chan error, 1)
	go func() { serverErr <- app.Listener(ln) }()
	defer func() {
		_ = app.Shutdown()
		select {
		case err := <-serverErr:
			if err != nil && !strings.Contains(err.Error(), "Server closed") {
				t.Fatalf("fiber listener: %v", err)
			}
		case <-time.After(time.Second):
		}
	}()
	baseURL := "http://" + ln.Addr().String()
	reader, writer := io.Pipe()
	defer writer.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodPost, baseURL+lease.RawUploadURL, reader)
	if err != nil {
		t.Fatalf("new raw request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+lease.Lease)
	req.ContentLength = slowTotal
	done := make(chan error, 1)
	go func() {
		resp, err := client.Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		done <- err
	}()
	if _, err := writer.Write(bytes.Repeat([]byte("x"), 512*1024)); err != nil {
		t.Fatalf("write first chunk: %v", err)
	}
	transferID := waitForSlowUploadTransfer(t, client, baseURL)
	cancelReq, err := http.NewRequest(http.MethodPost, baseURL+"/api/transfers/"+url.PathEscape(transferID)+"/cancel", nil)
	if err != nil {
		t.Fatalf("new cancel request: %v", err)
	}
	cancelReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	cancelResp, err := client.Do(cancelReq)
	if err != nil {
		t.Fatalf("cancel request: %v", err)
	}
	_, _ = io.Copy(io.Discard, cancelResp.Body)
	_ = cancelResp.Body.Close()
	if cancelResp.StatusCode != http.StatusOK {
		t.Fatalf("expected cancel ok, got %d", cancelResp.StatusCode)
	}
	_ = writer.CloseWithError(io.ErrClosedPipe)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("raw upload request did not finish after admin cancel")
	}
	if _, err := os.Stat(filepath.Join(root, "slow-raw.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected canceled raw upload not to create final file, stat=%v", err)
	}
	assertNoUploadTempFiles(t, root)
}

func TestActiveRawUploadCanceledAtResourcePublishBoundary(t *testing.T) {
	s, app, st, oldRoot, newRoot := newTransferGateTestServer(t)
	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	const total = int64(2 * 1024 * 1024)
	lease := createTestUploadLease(t, app, "publish-cancel.bin", total)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serverErr := make(chan error, 1)
	go func() { serverErr <- app.Listener(ln) }()
	defer func() {
		_ = app.Shutdown()
		<-serverErr
	}()
	reader, writer := io.Pipe()
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+lease.RawUploadURL, reader)
	if err != nil {
		t.Fatalf("new raw request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+lease.Lease)
	req.ContentLength = total
	done := make(chan struct{}, 1)
	go func() {
		resp, _ := client.Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		done <- struct{}{}
	}()
	if _, err := writer.Write(bytes.Repeat([]byte("x"), 512*1024)); err != nil {
		t.Fatalf("write first chunk: %v", err)
	}
	waitForRegisteredUpload(t, s, "publish-cancel.bin")
	started := time.Now()
	if err := s.updateConfigResources(func(resources []config.Dir) ([]config.Dir, error) {
		resources[0].Path = newRoot
		return resources, nil
	}); err != nil {
		t.Fatalf("publish changed resource: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("resource publish waited for long upload: %v", elapsed)
	}
	_ = writer.CloseWithError(io.ErrClosedPipe)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("canceled upload did not finish")
	}
	for _, root := range []string{oldRoot, newRoot} {
		if _, err := os.Stat(filepath.Join(root, "publish-cancel.bin")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected no final file in %s, stat=%v", root, err)
		}
		assertNoUploadTempFiles(t, root)
	}
}

func TestActiveRawUploadContinuesAfterSessionRemoval(t *testing.T) {
	for _, mode := range []string{"deleted", "idle_expired"} {
		t.Run(mode, func(t *testing.T) {
			s, app, st, root, _ := newTransferGateTestServer(t)
			if err := st.CreateSessionWithIdle("user-sid", time.Now().Add(time.Hour), time.Now().Add(time.Hour), "user", ""); err != nil {
				t.Fatalf("create session: %v", err)
			}
			const total = int64(1024 * 1024)
			fileName := "session-" + mode + ".bin"
			lease := createTestUploadLease(t, app, fileName, total)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			serverErr := make(chan error, 1)
			go func() { serverErr <- app.Listener(ln) }()
			defer func() {
				_ = app.Shutdown()
				<-serverErr
			}()
			reader, writer := io.Pipe()
			client := &http.Client{Timeout: 5 * time.Second}
			req, err := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+lease.RawUploadURL, reader)
			if err != nil {
				t.Fatalf("new raw request: %v", err)
			}
			req.Header.Set("Authorization", "Bearer "+lease.Lease)
			req.ContentLength = total
			type uploadResult struct {
				status int
				err    error
			}
			done := make(chan uploadResult, 1)
			go func() {
				resp, err := client.Do(req)
				if err != nil {
					done <- uploadResult{err: err}
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				done <- uploadResult{status: resp.StatusCode}
			}()
			first := bytes.Repeat([]byte("s"), 256*1024)
			if _, err := writer.Write(first); err != nil {
				t.Fatalf("write first chunk: %v", err)
			}
			waitForRegisteredUpload(t, s, fileName)
			if mode == "deleted" {
				if err := st.DeleteSession("user-sid"); err != nil {
					t.Fatalf("delete session: %v", err)
				}
			} else {
				expired := time.Now().Add(-time.Minute)
				if err := st.TouchSession("user-sid", expired, expired); err != nil {
					t.Fatalf("expire idle session: %v", err)
				}
				if err := st.DeleteExpiredSessions(time.Now()); err != nil {
					t.Fatalf("clean idle session: %v", err)
				}
			}
			if _, err := writer.Write(bytes.Repeat([]byte("s"), int(total)-len(first))); err != nil {
				t.Fatalf("write remaining upload: %v", err)
			}
			_ = writer.Close()
			result := <-done
			if result.err != nil || result.status != http.StatusOK {
				t.Fatalf("expected upload to survive session removal, status=%d err=%v", result.status, result.err)
			}
			info, err := os.Stat(filepath.Join(root, fileName))
			if err != nil || info.Size() != total {
				t.Fatalf("expected complete uploaded file, info=%v err=%v", info, err)
			}
		})
	}
}

func TestActiveLegacyMultipartCanceledAtResourcePublishBoundary(t *testing.T) {
	s, app, st, oldRoot, newRoot := newTransferGateTestServer(t)
	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	const total = int64(2 * 1024 * 1024)
	lease := createTestUploadLease(t, app, "multipart-cancel.bin", total)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serverErr := make(chan error, 1)
	go func() { serverErr <- app.Listener(ln) }()
	defer func() {
		_ = app.Shutdown()
		<-serverErr
	}()
	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+lease.UploadURL, reader)
	if err != nil {
		t.Fatalf("new multipart request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+lease.Lease)
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	done := make(chan struct{}, 1)
	go func() {
		resp, _ := client.Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		done <- struct{}{}
	}()
	part, err := multipartWriter.CreateFormFile("files", "ignored.bin")
	if err != nil {
		t.Fatalf("create multipart part: %v", err)
	}
	if _, err := part.Write(bytes.Repeat([]byte("m"), 512*1024)); err != nil {
		t.Fatalf("write multipart chunk: %v", err)
	}
	waitForRegisteredUpload(t, s, "multipart-cancel.bin")
	if err := s.updateConfigResources(func(resources []config.Dir) ([]config.Dir, error) {
		resources[0].Path = newRoot
		return resources, nil
	}); err != nil {
		t.Fatalf("publish changed resource: %v", err)
	}
	_ = writer.CloseWithError(io.ErrClosedPipe)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("canceled multipart upload did not finish")
	}
	for _, root := range []string{oldRoot, newRoot} {
		if _, err := os.Stat(filepath.Join(root, "multipart-cancel.bin")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected no final multipart file in %s, stat=%v", root, err)
		}
		assertNoUploadTempFiles(t, root)
	}
}

func TestCompletedUploadFileSurvivesResourcePublish(t *testing.T) {
	s, app, st, oldRoot, newRoot := newTransferGateTestServer(t)
	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	lease := createTestUploadLease(t, app, "completed-before-publish.txt", 5)
	req := httptest.NewRequest(http.MethodPost, lease.RawUploadURL, strings.NewReader("hello"))
	req.Header.Set("Authorization", "Bearer "+lease.Lease)
	req.ContentLength = 5
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("complete upload: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected completed upload, got %d", resp.StatusCode)
	}
	if err := s.updateConfigResources(func(resources []config.Dir) ([]config.Dir, error) {
		resources[0].Path = newRoot
		return resources, nil
	}); err != nil {
		t.Fatalf("publish changed resource: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(oldRoot, "completed-before-publish.txt"))
	if err != nil || string(content) != "hello" {
		t.Fatalf("expected completed file retained, content=%q err=%v", content, err)
	}
}

func TestStartedDownloadCompletesAcrossResourcePublishAndOldLeaseCannotRestart(t *testing.T) {
	s, app, st, oldRoot, newRoot := newTransferGateTestServer(t)
	content := bytes.Repeat([]byte("download-boundary-"), 512*1024)
	filePath := filepath.Join(oldRoot, "slow-download.bin")
	if err := osWriteFile(filePath, content); err != nil {
		t.Fatalf("write download file: %v", err)
	}
	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("stat download file: %v", err)
	}
	lease := &store.DownloadLease{Hash: security.HashToken("started-download"), Source: "session", DirID: "default", Path: "slow-download.bin", ResourceFingerprint: testResourceFingerprint(t, s.cfg(), "default"), FileSize: info.Size(), FileMtime: normalizedFileMtime(info), FileSHA256: sql.NullString{String: "", Valid: true}, ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.CreateDownloadLease(lease); err != nil {
		t.Fatalf("create download lease: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serverErr := make(chan error, 1)
	go func() { serverErr <- app.Listener(ln) }()
	defer func() {
		_ = app.Shutdown()
		<-serverErr
	}()
	url := "http://" + ln.Addr().String() + "/api/files/download-by-lease?lease=started-download"
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("start download: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("expected started download 200, got %d", resp.StatusCode)
	}
	if err := s.updateConfigResources(func(resources []config.Dir) ([]config.Dir, error) {
		resources[0].Path = newRoot
		return resources, nil
	}); err != nil {
		resp.Body.Close()
		t.Fatalf("publish changed resource: %v", err)
	}
	received, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || !bytes.Equal(received, content) {
		t.Fatalf("expected started download to complete, bytes=%d/%d err=%v", len(received), len(content), err)
	}
	rangeReq, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new range request: %v", err)
	}
	rangeReq.Header.Set("Range", "bytes=0-15")
	rangeResp, err := client.Do(rangeReq)
	if err != nil {
		t.Fatalf("retry old lease range: %v", err)
	}
	rangeResp.Body.Close()
	if rangeResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected old lease range rejected after publish, got %d", rangeResp.StatusCode)
	}
}

func TestUploadLeaseFingerprintRejectsAfterRestartWithChangedResource(t *testing.T) {
	base := t.TempDir()
	oldRoot := filepath.Join(base, "old")
	newRoot := filepath.Join(base, "new")
	if err := os.MkdirAll(oldRoot, 0755); err != nil {
		t.Fatalf("mkdir old root: %v", err)
	}
	if err := os.MkdirAll(newRoot, 0755); err != nil {
		t.Fatalf("mkdir new root: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app := New(testConfig(oldRoot), st)
	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	leaseReq := httptest.NewRequest(http.MethodPost, "/api/files/upload-lease", strings.NewReader(`{"dirId":"default","path":"","fileName":"restart.txt","fileSize":5}`))
	leaseReq.Header.Set("Content-Type", "application/json")
	leaseReq.AddCookie(&http.Cookie{Name: "sid", Value: "user-sid"})
	leaseResp, err := app.Test(leaseReq)
	if err != nil {
		t.Fatalf("create upload lease: %v", err)
	}
	var lease uploadLeaseResponse
	decodeJSON(t, leaseResp, &lease)
	if leaseResp.StatusCode != http.StatusOK || lease.Lease == "" {
		t.Fatalf("expected upload lease, status=%d lease=%+v", leaseResp.StatusCode, lease)
	}
	restarted := New(testConfig(newRoot), st)
	body, contentType := multipartUploadBody(t, "ignored.txt", []byte("hello"))
	uploadReq := httptest.NewRequest(http.MethodPost, lease.UploadURL, body)
	uploadReq.Header.Set("Content-Type", contentType)
	uploadReq.Header.Set("Authorization", "Bearer "+lease.Lease)
	uploadResp, err := restarted.Test(uploadReq)
	if err != nil {
		t.Fatalf("upload stale lease after restart: %v", err)
	}
	assertErrorContains(t, uploadResp, http.StatusForbidden, "资源已变化")
	if _, err := os.Stat(filepath.Join(newRoot, "restart.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected stale lease not to write into new root, stat=%v", err)
	}
	stored, err := st.UploadLeaseByHash(security.HashToken(lease.Lease))
	if err != nil {
		t.Fatalf("reload upload lease: %v", err)
	}
	if stored.UsedAt.Valid {
		t.Fatalf("expected fingerprint mismatch not to consume upload lease")
	}
}

func TestUploadLeaseFingerprintCatchesOnlineResourceChangeWithoutCleanup(t *testing.T) {
	base := t.TempDir()
	oldRoot := filepath.Join(base, "old")
	newRoot := filepath.Join(base, "new")
	if err := os.MkdirAll(oldRoot, 0755); err != nil {
		t.Fatalf("mkdir old root: %v", err)
	}
	if err := os.MkdirAll(newRoot, 0755); err != nil {
		t.Fatalf("mkdir new root: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(oldRoot)
	s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry()}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	s.routes(app)
	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	leaseReq := httptest.NewRequest(http.MethodPost, "/api/files/upload-lease", strings.NewReader(`{"dirId":"default","path":"","fileName":"race.txt","fileSize":5}`))
	leaseReq.Header.Set("Content-Type", "application/json")
	leaseReq.AddCookie(&http.Cookie{Name: "sid", Value: "user-sid"})
	leaseResp, err := app.Test(leaseReq)
	if err != nil {
		t.Fatalf("create upload lease: %v", err)
	}
	var lease uploadLeaseResponse
	decodeJSON(t, leaseResp, &lease)
	if leaseResp.StatusCode != http.StatusOK || lease.Lease == "" {
		t.Fatalf("expected upload lease, status=%d lease=%+v", leaseResp.StatusCode, lease)
	}
	s.replaceConfig(testConfig(newRoot))
	body, contentType := multipartUploadBody(t, "ignored.txt", []byte("hello"))
	uploadReq := httptest.NewRequest(http.MethodPost, lease.UploadURL, body)
	uploadReq.Header.Set("Content-Type", contentType)
	uploadReq.Header.Set("Authorization", "Bearer "+lease.Lease)
	uploadResp, err := app.Test(uploadReq)
	if err != nil {
		t.Fatalf("upload stale lease after hot config change: %v", err)
	}
	assertErrorContains(t, uploadResp, http.StatusForbidden, "资源已变化")
	if _, err := os.Stat(filepath.Join(newRoot, "race.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected stale lease not to write into hot-updated root, stat=%v", err)
	}
	stored, err := st.UploadLeaseByHash(security.HashToken(lease.Lease))
	if err != nil {
		t.Fatalf("reload upload lease: %v", err)
	}
	if stored.UsedAt.Valid {
		t.Fatalf("expected hot-change fingerprint mismatch not to consume upload lease")
	}
}

func TestUploadLeaseRejectsQueryToken(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app := New(testConfig(root), st)
	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	leaseReq := httptest.NewRequest(http.MethodPost, "/api/files/upload-lease", strings.NewReader(`{"dirId":"default","path":"","fileName":"query.txt","fileSize":5}`))
	leaseReq.Header.Set("Content-Type", "application/json")
	leaseReq.AddCookie(&http.Cookie{Name: "sid", Value: "user-sid"})
	leaseResp, err := app.Test(leaseReq)
	if err != nil {
		t.Fatalf("create upload lease: %v", err)
	}
	var lease uploadLeaseResponse
	decodeJSON(t, leaseResp, &lease)
	if leaseResp.StatusCode != http.StatusOK || lease.Lease == "" {
		t.Fatalf("expected upload lease, status=%d lease=%+v", leaseResp.StatusCode, lease)
	}
	body, contentType := multipartUploadBody(t, "ignored.txt", []byte("hello"))
	uploadReq := httptest.NewRequest(http.MethodPost, lease.UploadURL+"?lease="+url.QueryEscape(lease.Lease), body)
	uploadReq.Header.Set("Content-Type", contentType)
	uploadResp, err := app.Test(uploadReq)
	if err != nil {
		t.Fatalf("upload by query lease: %v", err)
	}
	uploadResp.Body.Close()
	if uploadResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected query lease to be unauthorized, got %d", uploadResp.StatusCode)
	}
	stored, err := st.UploadLeaseByHash(security.HashToken(lease.Lease))
	if err != nil {
		t.Fatalf("reload upload lease: %v", err)
	}
	if stored.UsedAt.Valid {
		t.Fatalf("expected query lease rejection not to consume upload lease")
	}
}

func TestUploadLeaseInvalidatedWhenResourceChanges(t *testing.T) {
	base := t.TempDir()
	oldRoot := filepath.Join(base, "old")
	newRoot := filepath.Join(base, "new")
	if err := os.MkdirAll(oldRoot, 0755); err != nil {
		t.Fatalf("mkdir old root: %v", err)
	}
	if err := os.MkdirAll(newRoot, 0755); err != nil {
		t.Fatalf("mkdir new root: %v", err)
	}
	oldTmp := filepath.Join(oldRoot, ".upload-stale.tmp")
	if err := os.WriteFile(oldTmp, []byte("stale"), 0600); err != nil {
		t.Fatalf("write stale temp: %v", err)
	}
	staleTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldTmp, staleTime, staleTime); err != nil {
		t.Fatalf("mark temp stale: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(base)
	cfg.Storage.Dirs[0].Path = oldRoot
	cfg.Auth.DevAllowFixedCode = true
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	app := NewWithConfigPath(cfg, st, cfgPath)
	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create user session: %v", err)
	}
	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	leaseReq := httptest.NewRequest(http.MethodPost, "/api/files/upload-lease", strings.NewReader(`{"dirId":"default","path":"","fileName":"moved.txt","fileSize":5}`))
	leaseReq.Header.Set("Content-Type", "application/json")
	leaseReq.AddCookie(&http.Cookie{Name: "sid", Value: "user-sid"})
	leaseResp, err := app.Test(leaseReq)
	if err != nil {
		t.Fatalf("create upload lease: %v", err)
	}
	var lease uploadLeaseResponse
	decodeJSON(t, leaseResp, &lease)
	if leaseResp.StatusCode != http.StatusOK || lease.Lease == "" {
		t.Fatalf("expected upload lease, status=%d lease=%+v", leaseResp.StatusCode, lease)
	}
	updateReq := httptest.NewRequest(http.MethodPut, "/api/config/resources/default", strings.NewReader(fmt.Sprintf(`{"id":"default","name":"Default","type":"directory","path":%q,"allowDownload":true,"allowUpload":true}`, newRoot)))
	updateReq.Header.Set("Content-Type", "application/json")
	updateReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	updateResp, err := app.Test(updateReq)
	if err != nil {
		t.Fatalf("update resource: %v", err)
	}
	updateResp.Body.Close()
	if updateResp.StatusCode != http.StatusOK {
		t.Fatalf("expected resource update ok, got %d", updateResp.StatusCode)
	}
	cleanupDeadline := time.Now().Add(3 * time.Second)
	for {
		_, statErr := os.Stat(oldTmp)
		if errors.Is(statErr, os.ErrNotExist) {
			break
		}
		if time.Now().After(cleanupDeadline) {
			t.Fatalf("expected old resource temp to be removed asynchronously, stat=%v", statErr)
		}
		time.Sleep(time.Millisecond)
	}
	body, contentType := multipartUploadBody(t, "ignored.txt", []byte("hello"))
	uploadReq := httptest.NewRequest(http.MethodPost, lease.UploadURL, body)
	uploadReq.Header.Set("Content-Type", contentType)
	uploadReq.Header.Set("Authorization", "Bearer "+lease.Lease)
	uploadResp, err := app.Test(uploadReq)
	if err != nil {
		t.Fatalf("upload by stale lease: %v", err)
	}
	uploadResp.Body.Close()
	if uploadResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected stale upload lease to fail, got %d", uploadResp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(newRoot, "moved.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected stale upload not to create file in new root, stat=%v", err)
	}
}

func TestResourceConfigDBFailureDoesNotPublish(t *testing.T) {
	restoreLog := discardTestLog(t)
	defer restoreLog()
	oldRoot := t.TempDir()
	newRoot := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	baseConfig := testConfig(oldRoot)
	baseConfig.Auth.DevAllowFixedCode = true
	cfg, err := baseConfig.NormalizedClone()
	if err != nil {
		t.Fatalf("normalize config: %v", err)
	}
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	s := &Server{config: cfg, configPath: cfgPath, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry()}
	s.revokeResourceAccess = func([]string) error { return errors.New("forced db failure") }
	err = s.updateConfigResources(func(resources []config.Dir) ([]config.Dir, error) {
		resources[0].Path = newRoot
		return resources, nil
	})
	if err == nil {
		t.Fatalf("expected DB revocation failure")
	}
	if got, _ := s.cfg().Dir("default"); got.Path != oldRoot {
		t.Fatalf("expected memory config unchanged, got %q", got.Path)
	}
	loaded, err := config.Load(cfgPath)
	if err != nil || loaded.Storage.Dirs[0].Path != oldRoot {
		t.Fatalf("expected disk config unchanged, path=%q err=%v", loaded.Storage.Dirs[0].Path, err)
	}
	if matches, err := filepath.Glob(filepath.Join(filepath.Dir(cfgPath), ".config-*.yaml.tmp")); err != nil || len(matches) != 0 {
		t.Fatalf("expected prepared temp aborted, matches=%v err=%v", matches, err)
	}
}

func TestResourceConfigPublishFailureLeavesRevocationsApplied(t *testing.T) {
	restoreLog := discardTestLog(t)
	defer restoreLog()
	oldRoot := t.TempDir()
	newRoot := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	baseConfig := testConfig(oldRoot)
	baseConfig.Auth.DevAllowFixedCode = true
	cfg, err := baseConfig.NormalizedClone()
	if err != nil {
		t.Fatalf("normalize config: %v", err)
	}
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	token := &store.Token{Hash: "publish-fail-token", Type: "upload", DirID: "default", ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))}
	if err := st.CreateToken(token); err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := st.CreateUploadLease(&store.UploadLease{Hash: "publish-fail-upload", SessionID: "session", DirID: "default", FileName: "a.txt", ResourceFingerprint: token.ResourceFingerprint, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("create upload lease: %v", err)
	}
	if err := st.CreateDownloadLease(&store.DownloadLease{Hash: "publish-fail-download", Source: "session", DirID: "default", ResourceFingerprint: token.ResourceFingerprint, FileMtime: time.Now(), FileSHA256: sql.NullString{String: "", Valid: true}, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("create download lease: %v", err)
	}
	s := &Server{config: cfg, configPath: cfgPath, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry()}
	canceled := false
	s.transfers.add(&transferRecord{ID: "publish-fail-active", Type: "upload", Status: transferActive, DirID: "default", Cancelable: true, cancel: func() { canceled = true }})
	s.commitPreparedConfig = func(*config.PreparedSave) (bool, error) { return false, errors.New("forced rename failure") }
	err = s.updateConfigResources(func(resources []config.Dir) ([]config.Dir, error) {
		resources[0].Path = newRoot
		return resources, nil
	})
	if err == nil {
		t.Fatalf("expected publish failure")
	}
	if got, _ := s.cfg().Dir("default"); got.Path != oldRoot {
		t.Fatalf("expected memory config to stay old after rename failure, got %q", got.Path)
	}
	loadedConfig, err := config.Load(cfgPath)
	if err != nil || loadedConfig.Storage.Dirs[0].Path != oldRoot {
		t.Fatalf("expected disk config to stay old, path=%q err=%v", loadedConfig.Storage.Dirs[0].Path, err)
	}
	loadedToken, err := st.TokenByID(token.ID)
	if err != nil || !loadedToken.Revoked {
		t.Fatalf("expected authorization revocation to remain applied, token=%+v err=%v", loadedToken, err)
	}
	if _, err := st.UploadLeaseByHash("publish-fail-upload"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected upload lease revoked before failed publish, err=%v", err)
	}
	if _, err := st.DownloadLeaseByHash("publish-fail-download"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected download lease revoked before failed publish, err=%v", err)
	}
	if canceled {
		t.Fatalf("expected unpublished resource config not to cancel active upload")
	}
}

func TestResourceConfigNameOnlyDoesNotRevoke(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	baseConfig := testConfig(root)
	baseConfig.Auth.DevAllowFixedCode = true
	cfg, err := baseConfig.NormalizedClone()
	if err != nil {
		t.Fatalf("normalize config: %v", err)
	}
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	revokeCalls := 0
	canceled := false
	s := &Server{config: cfg, configPath: cfgPath, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry()}
	s.transfers.add(&transferRecord{ID: "name-only-active", Type: "upload", Status: transferActive, DirID: "default", Cancelable: true, cancel: func() { canceled = true }})
	s.revokeResourceAccess = func([]string) error {
		revokeCalls++
		return nil
	}
	if err := s.updateConfigResources(func(resources []config.Dir) ([]config.Dir, error) {
		resources[0].Name = "Renamed"
		return resources, nil
	}); err != nil {
		t.Fatalf("rename resource: %v", err)
	}
	if revokeCalls != 0 {
		t.Fatalf("expected name-only update not to revoke, calls=%d", revokeCalls)
	}
	if canceled {
		t.Fatalf("expected name-only update not to cancel active upload")
	}
	if got, _ := s.cfg().Dir("default"); got.Name != "Renamed" {
		t.Fatalf("expected renamed resource in memory, got %+v", got)
	}
}

func TestResourceAuthorizationChangesRevokeBeforePublish(t *testing.T) {
	cases := map[string]func(*config.Dir){
		"path":           func(dir *config.Dir) { dir.Path = t.TempDir() },
		"type":           func(dir *config.Dir) { dir.Type = config.ResourceFile; dir.AllowUpload = false },
		"allow_download": func(dir *config.Dir) { dir.AllowDownload = false },
		"allow_upload":   func(dir *config.Dir) { dir.AllowUpload = false },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			baseConfig := testConfig(root)
			baseConfig.Auth.DevAllowFixedCode = true
			cfg, err := baseConfig.NormalizedClone()
			if err != nil {
				t.Fatalf("normalize config: %v", err)
			}
			cfgPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := config.SaveAtomic(cfgPath, cfg); err != nil {
				t.Fatalf("save config: %v", err)
			}
			st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			defer st.DB.Close()
			var revoked []string
			s := &Server{config: cfg, configPath: cfgPath, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry()}
			s.revokeResourceAccess = func(ids []string) error {
				revoked = append(revoked, ids...)
				return nil
			}
			if err := s.updateConfigResources(func(resources []config.Dir) ([]config.Dir, error) {
				mutate(&resources[0])
				return resources, nil
			}); err != nil {
				t.Fatalf("update resource authorization: %v", err)
			}
			if len(revoked) != 1 || revoked[0] != "default" {
				t.Fatalf("expected default resource revocation, got %v", revoked)
			}
		})
	}
}

func TestPublishedResourceConfigSyncFailureKeepsRevocationAndNewConfig(t *testing.T) {
	restoreLog := discardTestLog(t)
	defer restoreLog()
	root := t.TempDir()
	newRoot := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	baseConfig := testConfig(root)
	baseConfig.Auth.DevAllowFixedCode = true
	cfg, err := baseConfig.NormalizedClone()
	if err != nil {
		t.Fatalf("normalize config: %v", err)
	}
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	token := &store.Token{Hash: "sync-failure-token", Type: "upload", DirID: "default", ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))}
	if err := st.CreateToken(token); err != nil {
		t.Fatalf("create token: %v", err)
	}
	s := &Server{config: cfg, configPath: cfgPath, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry()}
	canceled := false
	s.transfers.add(&transferRecord{ID: "sync-failure-active", Type: "upload", Status: transferActive, DirID: "default", Cancelable: true, cancel: func() { canceled = true }})
	s.commitPreparedConfig = func(prepared *config.PreparedSave) (bool, error) {
		published, err := prepared.Commit()
		if err != nil || !published {
			return published, err
		}
		return true, errors.New("forced directory sync failure")
	}
	err = s.updateConfigResources(func(resources []config.Dir) ([]config.Dir, error) {
		resources[0].Path = newRoot
		return resources, nil
	})
	if err == nil {
		t.Fatalf("expected reported sync failure")
	}
	if !canceled {
		t.Fatalf("expected published resource config to cancel active upload despite sync failure")
	}
	if current, _ := s.cfg().Dir("default"); current.Path != newRoot {
		t.Fatalf("expected memory config switched after published sync failure, got %q", current.Path)
	}
	loaded, err := config.Load(cfgPath)
	if err != nil || loaded.Storage.Dirs[0].Path != newRoot {
		t.Fatalf("expected disk config published, path=%q err=%v", loaded.Storage.Dirs[0].Path, err)
	}
	loadedToken, err := st.TokenByID(token.ID)
	if err != nil || !loadedToken.Revoked {
		t.Fatalf("expected DB revocation retained, token=%+v err=%v", loadedToken, err)
	}
	logs, err := st.AuditLogs(10)
	if err != nil {
		t.Fatalf("load audit logs: %v", err)
	}
	found := false
	for _, entry := range logs {
		if entry.Action == "config_resource_published_sync_failed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected published sync failure audit, logs=%+v", logs)
	}
}

func TestConfigPrepareErrorClassificationDoesNotLeakPath(t *testing.T) {
	restoreLog := discardTestLog(t)
	defer restoreLog()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(root)
	cfg.Auth.DevAllowFixedCode = true
	s := &Server{config: cfg, configPath: filepath.Join(root, "private", "config.yaml"), store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry()}

	err = s.updateConfig(func(next *config.Config) error {
		next.Storage.AllowedExtensions = []string{"*"}
		return nil
	})
	var fiberErr *fiber.Error
	if !errors.As(err, &fiberErr) || fiberErr.Code != http.StatusBadRequest {
		t.Fatalf("expected validation error 400, err=%v", err)
	}

	privatePath := s.configPath
	s.prepareConfig = func(string, *config.Config) (*config.PreparedSave, *config.Config, error) {
		return nil, nil, fmt.Errorf("cannot write %s: %w", privatePath, os.ErrPermission)
	}
	err = s.updateConfig(func(*config.Config) error { return nil })
	if !errors.As(err, &fiberErr) || fiberErr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected filesystem error 503, err=%v", err)
	}
	if strings.Contains(fiberErr.Message, privatePath) || strings.Contains(fiberErr.Message, "permission") {
		t.Fatalf("filesystem response leaked internal detail: %q", fiberErr.Message)
	}
}

func TestUploadLeaseInvalidatedAfterResourceDeleteAndRecreate(t *testing.T) {
	base := t.TempDir()
	oldRoot := filepath.Join(base, "old")
	newRoot := filepath.Join(base, "new")
	if err := os.MkdirAll(oldRoot, 0755); err != nil {
		t.Fatalf("mkdir old root: %v", err)
	}
	if err := os.MkdirAll(newRoot, 0755); err != nil {
		t.Fatalf("mkdir new root: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(base)
	cfg.Storage.Dirs[0].Path = oldRoot
	cfg.Auth.DevAllowFixedCode = true
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	app := NewWithConfigPath(cfg, st, cfgPath)
	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create user session: %v", err)
	}
	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	leaseReq := httptest.NewRequest(http.MethodPost, "/api/files/upload-lease", strings.NewReader(`{"dirId":"default","path":"","fileName":"recreated.txt","fileSize":5}`))
	leaseReq.Header.Set("Content-Type", "application/json")
	leaseReq.AddCookie(&http.Cookie{Name: "sid", Value: "user-sid"})
	leaseResp, err := app.Test(leaseReq)
	if err != nil {
		t.Fatalf("create upload lease: %v", err)
	}
	var lease uploadLeaseResponse
	decodeJSON(t, leaseResp, &lease)
	if leaseResp.StatusCode != http.StatusOK || lease.Lease == "" {
		t.Fatalf("expected upload lease, status=%d lease=%+v", leaseResp.StatusCode, lease)
	}
	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/config/resources/default", nil)
	deleteReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	deleteResp, err := app.Test(deleteReq)
	if err != nil {
		t.Fatalf("delete resource: %v", err)
	}
	deleteResp.Body.Close()
	if deleteResp.StatusCode != http.StatusOK {
		t.Fatalf("expected delete ok, got %d", deleteResp.StatusCode)
	}
	createReq := httptest.NewRequest(http.MethodPost, "/api/config/resources", strings.NewReader(fmt.Sprintf(`{"id":"default","name":"Default","type":"directory","path":%q,"allowDownload":true,"allowUpload":true}`, newRoot)))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	createResp, err := app.Test(createReq)
	if err != nil {
		t.Fatalf("recreate resource: %v", err)
	}
	createResp.Body.Close()
	if createResp.StatusCode != http.StatusOK {
		t.Fatalf("expected recreate ok, got %d", createResp.StatusCode)
	}
	body, contentType := multipartUploadBody(t, "ignored.txt", []byte("hello"))
	uploadReq := httptest.NewRequest(http.MethodPost, lease.UploadURL, body)
	uploadReq.Header.Set("Content-Type", contentType)
	uploadReq.Header.Set("Authorization", "Bearer "+lease.Lease)
	uploadResp, err := app.Test(uploadReq)
	if err != nil {
		t.Fatalf("upload by stale lease: %v", err)
	}
	uploadResp.Body.Close()
	if uploadResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected recreated resource stale lease to fail, got %d", uploadResp.StatusCode)
	}
}

func TestStreamingUploadLimitCleansTempFile(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(root)
	cfg.Storage.UploadMaxMB = 2
	cfg.Storage.UploadMaxFileMB = 1
	app := New(cfg, st)
	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	body, contentType := multipartUploadBody(t, "large.bin", bytes.Repeat([]byte("x"), 1024*1024+10))
	req := httptest.NewRequest(http.MethodPost, "/api/files/upload", body)
	req.Header.Set("Content-Type", contentType)
	req.AddCookie(&http.Cookie{Name: "sid", Value: "user-sid"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("upload request: %v", err)
	}
	assertErrorContains(t, resp, http.StatusRequestEntityTooLarge, "单个文件")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".upload-") {
			t.Fatalf("expected temp file cleanup, found %s", entry.Name())
		}
	}
}

func TestRealListenerMultipartUploadUsesBodyStreamAndCompletes(t *testing.T) {
	s, app, st, root, _ := newTransferGateTestServer(t)
	testCfg := s.cfg().Clone()
	testCfg.Storage.UploadMaxMB = 4
	testCfg.Storage.UploadMaxFileMB = 2
	testCfg.Storage.MinFreeMB = 0
	testCfg.Storage.MinFreePercent = 0
	s.replaceConfig(testCfg)
	if err := st.CreateSession("real-stream-session", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- app.Listener(listener) }()
	defer func() {
		_ = app.Shutdown()
		<-serverDone
	}()

	oldFallback := testMultipartBodyFallback
	var fallbackCalls atomic.Int32
	testMultipartBodyFallback = func(c *fiber.Ctx) io.Reader {
		fallbackCalls.Add(1)
		return bytes.NewReader(c.Body())
	}
	defer func() { testMultipartBodyFallback = oldFallback }()

	content := bytes.Repeat([]byte("real-listener-stream-"), 32*1024)
	multipartBody, contentType := multipartUploadBody(t, "real-stream.bin", content)
	payload := append([]byte(nil), multipartBody.Bytes()...)
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial listener: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatalf("set connection deadline: %v", err)
	}
	headers := fmt.Sprintf("POST /api/files/upload?dirId=default HTTP/1.1\r\nHost: %s\r\nContent-Type: %s\r\nTransfer-Encoding: chunked\r\nCookie: sid=real-stream-session\r\nConnection: close\r\n\r\n", listener.Addr().String(), contentType)
	if _, err := io.WriteString(conn, headers); err != nil {
		t.Fatalf("write request headers: %v", err)
	}
	const chunkSize = 32 * 1024
	writeChunk := func(chunk []byte) error {
		if _, err := fmt.Fprintf(conn, "%x\r\n", len(chunk)); err != nil {
			return err
		}
		if _, err := conn.Write(chunk); err != nil {
			return err
		}
		_, err := io.WriteString(conn, "\r\n")
		return err
	}
	if err := writeChunk(payload[:chunkSize]); err != nil {
		t.Fatalf("write first request chunk: %v", err)
	}
	registrationDeadline := time.Now().Add(3 * time.Second)
	for {
		registered := false
		for _, record := range s.transfers.list() {
			if record.Type == "upload" && record.FileName == "real-stream.bin" && record.Status == transferActive {
				registered = true
				break
			}
		}
		if registered {
			break
		}
		if time.Now().After(registrationDeadline) {
			t.Fatalf("expected active upload %q", "real-stream.bin")
		}
		time.Sleep(time.Millisecond)
	}
	if permits := s.transfers.uploadPermitCount(); permits != 1 {
		t.Fatalf("expected one request permit during multipart stream, got %d", permits)
	}
	for offset := chunkSize; offset < len(payload); offset += chunkSize {
		end := offset + chunkSize
		if end > len(payload) {
			end = len(payload)
		}
		if err := writeChunk(payload[offset:end]); err != nil {
			t.Fatalf("write multipart chunk: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := io.WriteString(conn, "0\r\n\r\n"); err != nil {
		t.Fatalf("finish chunked request: %v", err)
	}
	requestForResponse := &http.Request{Method: http.MethodPost}
	resp, err := http.ReadResponse(bufio.NewReader(conn), requestForResponse)
	if err != nil {
		t.Fatalf("read multipart response: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("multipart status=%d body=%s", resp.StatusCode, body)
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("real listener unexpectedly used buffered app.Test fallback %d times", fallbackCalls.Load())
	}
	wantPath := filepath.Join(root, "real-stream.bin")
	got, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("uploaded content mismatch: got=%d want=%d", len(got), len(content))
	}
	assertNoUploadTempFiles(t, root)
	deadline := time.Now().Add(3 * time.Second)
	for {
		permitCount := s.transfers.uploadPermitCount()
		completed := false
		for _, record := range s.transfers.list() {
			if record.Type == "upload" && record.FileName == "real-stream.bin" && record.Status == transferCompleted {
				completed = true
			}
		}
		if permitCount == 0 && completed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("upload state not released: permits=%d completed=%t", permitCount, completed)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStartupUploadTempCleanupHonorsRetention(t *testing.T) {
	root := t.TempDir()
	oldTmp := filepath.Join(root, ".upload-old.tmp")
	newTmp := filepath.Join(root, ".upload-new.tmp")
	if err := os.WriteFile(oldTmp, []byte("old"), 0600); err != nil {
		t.Fatalf("write old temp: %v", err)
	}
	if err := os.WriteFile(newTmp, []byte("new"), 0600); err != nil {
		t.Fatalf("write new temp: %v", err)
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldTmp, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(root)
	cfg.Storage.UploadTempRetentionSeconds = 60
	_ = New(cfg, st)
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, statErr := os.Stat(oldTmp)
		if errors.Is(statErr, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected old temp to be removed asynchronously, stat=%v", statErr)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := os.Stat(newTmp); err != nil {
		t.Fatalf("expected new temp to remain: %v", err)
	}
}

func TestActiveTransfersAPIShape(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app := New(testConfig(t.TempDir()), st)
	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/transfers/active", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("active transfers: %v", err)
	}
	var payload struct {
		Transfers []transferRecord `json:"transfers"`
	}
	decodeJSON(t, resp, &payload)
	if resp.StatusCode != http.StatusOK || payload.Transfers == nil {
		t.Fatalf("expected active transfers structure, status=%d payload=%+v", resp.StatusCode, payload)
	}
}

func TestCompletedTransferRemainsBrieflyVisibleWithoutProtectingTemp(t *testing.T) {
	registry := newTransferRegistry()
	tmpPath := filepath.Join(t.TempDir(), ".upload-live.tmp")
	registry.add(&transferRecord{ID: "upload-1", Type: "upload", Status: transferActive, TempPath: tmpPath, Cancelable: true})
	if _, ok := registry.activeTempPaths()[canonicalTempPath(tmpPath)]; !ok {
		t.Fatalf("expected active temp path to be protected")
	}
	registry.remove("upload-1")
	items := registry.list()
	if len(items) != 1 || items[0].Status != transferCompleted || items[0].Cancelable {
		t.Fatalf("expected completed transfer to remain briefly visible, got %+v", items)
	}
	if len(registry.activeTempPaths()) != 0 {
		t.Fatalf("completed transfer must not protect temp files")
	}
}

func TestAdminCancelUploadTransfer(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(t.TempDir())
	s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry()}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	s.routes(app)
	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	canceled := make(chan struct{})
	s.transfers.add(&transferRecord{ID: "upload-1", Type: "upload", Status: transferActive, Source: "session", DirID: "default", FileName: "slow.bin", Cancelable: true, cancel: func() { close(canceled) }})
	req := httptest.NewRequest(http.MethodPost, "/api/transfers/upload-1/cancel", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("cancel transfer: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected cancel ok, got %d", resp.StatusCode)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatalf("expected upload cancel function to be called")
	}
	items := s.transfers.list()
	if len(items) != 1 || items[0].Status != transferCanceling {
		t.Fatalf("expected transfer marked canceling, got %+v", items)
	}
}

func TestPublicUploadUsesRegistrySourceAndCancel(t *testing.T) {
	registry := newTransferRegistry()
	canceled := false
	registry.add(&transferRecord{ID: "public-1", Type: "upload", Status: transferActive, Source: "public_token", DirID: "default", Path: "", FileName: "public.bin", TotalBytes: 1024, Cancelable: true, TempPath: "./relative/.upload-live.tmp", cancel: func() { canceled = true }})
	items := registry.list()
	if len(items) != 1 || items[0].Source != "public_token" || !items[0].Cancelable {
		t.Fatalf("expected visible cancelable public upload transfer, got %+v", items)
	}
	active := registry.activeTempPaths()
	if len(active) != 1 {
		t.Fatalf("expected one normalized active temp path, got %+v", active)
	}
	for path := range active {
		if !filepath.IsAbs(path) || strings.Contains(path, "..") {
			t.Fatalf("expected absolute normalized active temp path, got %q", path)
		}
	}
	if !registry.cancel("public-1") || !canceled {
		t.Fatalf("expected public upload transfer cancel to call cancel hook")
	}
	items = registry.list()
	if len(items) != 1 || items[0].Status != transferCanceling {
		t.Fatalf("expected public upload marked canceling, got %+v", items)
	}
}

func TestPublicUploadStreamingLimitCleansTempAndRollsBackToken(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(root)
	cfg.Storage.UploadMaxMB = 2
	cfg.Storage.UploadMaxFileMB = 1
	cfg.Tokens.UploadMaxMB = 2
	app := New(cfg, st)
	tok := &store.Token{Hash: security.HashToken("public-upload"), Type: "upload", DirID: "default", Path: "", ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), MaxUses: 1, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))}
	if err := st.CreateToken(tok); err != nil {
		t.Fatalf("create upload token: %v", err)
	}
	body, contentType := multipartUploadBody(t, "too-large.bin", bytes.Repeat([]byte("x"), 1024*1024+10))
	req := httptest.NewRequest(http.MethodPost, "/t/public-upload/upload", body)
	req.Header.Set("Content-Type", contentType)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("public upload request: %v", err)
	}
	assertErrorContains(t, resp, http.StatusRequestEntityTooLarge, "单个文件")
	loaded, err := st.TokenByHash(security.HashToken("public-upload"))
	if err != nil {
		t.Fatalf("reload token: %v", err)
	}
	if loaded.Uses != 0 || loaded.UploadedBytes != 0 {
		t.Fatalf("expected failed public upload to roll back reservation, got uses=%d bytes=%d", loaded.Uses, loaded.UploadedBytes)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".upload-") || entry.Name() == "too-large.bin" {
			t.Fatalf("expected failed public upload cleanup, found %s", entry.Name())
		}
	}
}

func TestPublicUploadLeaseCannotUseSessionMultipartEndpoint(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(root)
	app := New(cfg, st)
	tok := &store.Token{Hash: security.HashToken("public-raw-token"), Type: "upload", DirID: "default", Path: "", ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), MaxUses: 1, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))}
	if err := st.CreateToken(tok); err != nil {
		t.Fatalf("create upload token: %v", err)
	}
	leaseReq := httptest.NewRequest(http.MethodPost, "/t/public-raw-token/upload-lease", strings.NewReader(`{"fileName":"bypass.txt","fileSize":6}`))
	leaseReq.Header.Set("Content-Type", "application/json")
	leaseResp, err := app.Test(leaseReq)
	if err != nil {
		t.Fatalf("create public upload lease: %v", err)
	}
	var lease uploadLeaseResponse
	decodeJSON(t, leaseResp, &lease)
	if leaseResp.StatusCode != http.StatusOK || lease.Lease == "" {
		t.Fatalf("expected public upload lease, status=%d lease=%+v", leaseResp.StatusCode, lease)
	}
	body, contentType := multipartUploadBody(t, "bypass.txt", []byte("bypass"))
	bypassReq := httptest.NewRequest(http.MethodPost, "/api/files/upload-by-lease", body)
	bypassReq.Header.Set("Content-Type", contentType)
	bypassReq.Header.Set("Authorization", "Bearer "+lease.Lease)
	bypassResp, err := app.Test(bypassReq)
	if err != nil {
		t.Fatalf("bypass multipart upload: %v", err)
	}
	bypassResp.Body.Close()
	if bypassResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected public lease rejected by session multipart endpoint, got %d", bypassResp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(root, "bypass.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected bypass file not to be written, stat=%v", err)
	}
	storedLease, err := st.UploadLeaseByHash(security.HashToken(lease.Lease))
	if err != nil {
		t.Fatalf("reload public upload lease: %v", err)
	}
	if storedLease.UsedAt.Valid {
		t.Fatalf("expected rejected public lease not to be consumed")
	}
	loadedToken, err := st.TokenByHash(security.HashToken("public-raw-token"))
	if err != nil {
		t.Fatalf("reload public token: %v", err)
	}
	if loadedToken.Uses != 0 || loadedToken.UploadedBytes != 0 {
		t.Fatalf("expected rejected bypass not to affect token, uses=%d bytes=%d", loadedToken.Uses, loadedToken.UploadedBytes)
	}
}

func TestPublicRawUploadRechecksResourceAfterTokenReservation(t *testing.T) {
	base := t.TempDir()
	oldRoot := filepath.Join(base, "old")
	newRoot := filepath.Join(base, "new")
	for _, root := range []string{oldRoot, newRoot} {
		if err := os.MkdirAll(root, 0755); err != nil {
			t.Fatalf("mkdir root: %v", err)
		}
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	oldCfg := testConfig(oldRoot)
	token := &store.Token{Hash: security.HashToken("raw-race-token"), Type: "upload", DirID: "default", Path: "", ResourceFingerprint: testResourceFingerprint(t, oldCfg, "default"), MaxUses: 1, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))}
	if err := st.CreateToken(token); err != nil {
		t.Fatalf("create upload token: %v", err)
	}
	s := &Server{config: oldCfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry()}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler, StreamRequestBody: true})
	s.routes(app)
	leaseReq := httptest.NewRequest(http.MethodPost, "/t/raw-race-token/upload-lease", strings.NewReader(`{"fileName":"race.txt","fileSize":5}`))
	leaseReq.Header.Set("Content-Type", "application/json")
	leaseResp, err := app.Test(leaseReq)
	if err != nil {
		t.Fatalf("create public upload lease: %v", err)
	}
	var lease uploadLeaseResponse
	decodeJSON(t, leaseResp, &lease)
	if leaseResp.StatusCode != http.StatusOK || lease.Lease == "" {
		t.Fatalf("expected public upload lease, status=%d lease=%+v", leaseResp.StatusCode, lease)
	}
	hookCalls := 0
	s.beforeUploadTransferRegister = func() {
		hookCalls++
		s.replaceConfig(testConfig(newRoot))
	}
	rawReq := httptest.NewRequest(http.MethodPost, lease.RawUploadURL, strings.NewReader("hello"))
	rawReq.Header.Set("Authorization", "Bearer "+lease.Lease)
	rawReq.ContentLength = 5
	rawResp, err := app.Test(rawReq)
	if err != nil {
		t.Fatalf("raw upload after resource race: %v", err)
	}
	rawResp.Body.Close()
	if rawResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected resource race to be forbidden, got %d", rawResp.StatusCode)
	}
	if hookCalls != 1 {
		t.Fatalf("expected one post-reservation hook call, got %d", hookCalls)
	}
	loaded, err := st.TokenByID(token.ID)
	if err != nil {
		t.Fatalf("reload upload token: %v", err)
	}
	if loaded.Uses != 0 || loaded.UploadedBytes != 0 {
		t.Fatalf("expected token reservation rollback, uses=%d bytes=%d", loaded.Uses, loaded.UploadedBytes)
	}
	for _, root := range []string{oldRoot, newRoot} {
		if _, err := os.Stat(filepath.Join(root, "race.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected no final file in %s, stat=%v", root, err)
		}
		assertNoUploadTempFiles(t, root)
	}
}

func TestUploadDiskReserveThresholdsAndErrors(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.Storage.MinFreeMB = 100
	cfg.Storage.MinFreePercent = 20
	s := &Server{config: cfg}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	app.Get("/check", func(c *fiber.Ctx) error { return s.checkUploadDiskReserve(c, cfg.Storage.Dirs[0].Path, 50*1024*1024) })
	tests := []struct {
		name             string
		available, total uint64
		err              error
		wantStatus       int
	}{
		{"megabyte reserve allows", 151 * 1024 * 1024, 500 * 1024 * 1024, nil, http.StatusOK},
		{"percentage reserve rejects", 249 * 1024 * 1024, 1000 * 1024 * 1024, nil, http.StatusServiceUnavailable},
		{"checker error fails closed", 0, 0, errors.New("disk unavailable"), http.StatusServiceUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s.availableDiskSpace = func(string) (uint64, uint64, error) { return tc.available, tc.total, tc.err }
			resp, requestErr := app.Test(httptest.NewRequest(http.MethodGet, "/check", nil))
			if requestErr != nil {
				t.Fatalf("disk check request: %v", requestErr)
			}
			if tc.wantStatus == http.StatusOK {
				resp.Body.Close()
				if resp.StatusCode != tc.wantStatus {
					t.Fatalf("status=%d want=%d", resp.StatusCode, tc.wantStatus)
				}
				return
			}
			var payload map[string]any
			decodeJSON(t, resp, &payload)
			if resp.StatusCode != tc.wantStatus || payload["code"] != "storage_reserve_unavailable" || resp.Header.Get("Retry-After") != "60" {
				t.Fatalf("unexpected disk rejection: status=%d payload=%+v retry=%q", resp.StatusCode, payload, resp.Header.Get("Retry-After"))
			}
		})
	}
	cfg.Storage.MinFreeMB = int(^uint(0) >> 1)
	s.availableDiskSpace = func(string) (uint64, uint64, error) { return ^uint64(0), ^uint64(0), nil }
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/check", nil))
	if err != nil {
		t.Fatalf("overflow disk check: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("overflowing reserve must fail closed, got %d", resp.StatusCode)
	}
	cfg.Storage.MinFreeMB = 100
	s.transfers = newTransferRegistry()
	dir, ok := cfg.Dir("default")
	if !ok {
		t.Fatalf("missing default dir")
	}
	s.availableDiskSpace = func(string) (uint64, uint64, error) { return 100 * 1024 * 1024, 1000 * 1024 * 1024, nil }
	if err := s.transfers.tryAcquireUploadPermit(&uploadPermit{ID: "disk-permit", DirID: dir.ID, ResourceFingerprint: resourceAuthorizationFingerprint(dir), OwnerType: "session", OwnerID: "disk-owner"}, uploadAdmissionLimits{Global: 1}); err != nil {
		t.Fatalf("acquire disk test permit: %v", err)
	}
	app.Post("/write", func(c *fiber.Ctx) error {
		_, _, err := s.saveRawUniqueAtomic(c, dir.Path, "", "disk.txt", strings.NewReader("hello"), "session", "session", "disk-owner", "disk-permit", "disk-test", dir.ID, 5, resourceAuthorizationFingerprint(dir))
		return err
	})
	writeResp, err := app.Test(httptest.NewRequest(http.MethodPost, "/write", nil))
	if err != nil {
		t.Fatalf("disk write rejection: %v", err)
	}
	writeResp.Body.Close()
	if writeResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected disk write rejection, got %d", writeResp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(dir.Path, "disk.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disk rejection created final file: %v", err)
	}
	assertNoUploadTempFiles(t, dir.Path)
}

func TestPublicRawUploadCapacityRollsBackTokenAndConsumesLease(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(root)
	cfg.Abuse.Uploads.Global = 1
	token := &store.Token{Hash: security.HashToken("capacity-token"), Type: "upload", DirID: "default", Path: "", ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), MaxUses: 1, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))}
	if err := st.CreateToken(token); err != nil {
		t.Fatalf("create token: %v", err)
	}
	s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry(), availableDiskSpace: func(string) (uint64, uint64, error) { return 1 << 40, 1 << 40, nil }}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler, StreamRequestBody: true})
	s.routes(app)
	leaseReq := httptest.NewRequest(http.MethodPost, "/t/capacity-token/upload-lease", strings.NewReader(`{"fileName":"capacity.txt","fileSize":5}`))
	leaseReq.Header.Set("Content-Type", "application/json")
	leaseResp, err := app.Test(leaseReq)
	if err != nil {
		t.Fatalf("create lease: %v", err)
	}
	var lease uploadLeaseResponse
	decodeJSON(t, leaseResp, &lease)
	if leaseResp.StatusCode != http.StatusOK {
		t.Fatalf("lease status=%d", leaseResp.StatusCode)
	}
	if err := s.transfers.tryAcquireUploadPermit(&uploadPermit{ID: "occupied", DirID: "other", ResourceFingerprint: "other-fingerprint", OwnerType: "session", OwnerID: "other"}, uploadAdmissionLimits{Global: 1}); err != nil {
		t.Fatalf("occupy upload capacity: %v", err)
	}
	rawReq := httptest.NewRequest(http.MethodPost, lease.RawUploadURL, strings.NewReader("hello"))
	rawReq.Header.Set("Authorization", "Bearer "+lease.Lease)
	rawReq.ContentLength = 5
	rawResp, err := app.Test(rawReq)
	if err != nil {
		t.Fatalf("raw capacity request: %v", err)
	}
	var payload map[string]any
	decodeJSON(t, rawResp, &payload)
	if rawResp.StatusCode != http.StatusServiceUnavailable || payload["code"] != "upload_capacity_exhausted" || rawResp.Header.Get("Retry-After") != "5" {
		t.Fatalf("unexpected capacity response: status=%d payload=%+v", rawResp.StatusCode, payload)
	}
	loadedToken, err := st.TokenByID(token.ID)
	if err != nil {
		t.Fatalf("load token: %v", err)
	}
	if loadedToken.Uses != 0 || loadedToken.UploadedBytes != 0 {
		t.Fatalf("public reservation must roll back, uses=%d bytes=%d", loadedToken.Uses, loadedToken.UploadedBytes)
	}
	storedLease, err := st.UploadLeaseByHash(security.HashToken(lease.Lease))
	if err != nil || !storedLease.UsedAt.Valid {
		t.Fatalf("upload lease must remain single-use after capacity rejection: lease=%+v err=%v", storedLease, err)
	}
	if _, err := os.Stat(filepath.Join(root, "capacity.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capacity rejection must not create final file: %v", err)
	}
	assertNoUploadTempFiles(t, root)
}

func TestRegisterUploadTransferReturnsScopedConcurrencyCode(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.Abuse.Uploads.Global = 10
	cfg.Abuse.Uploads.PerResource = 1
	s := &Server{config: cfg, transfers: newTransferRegistry()}
	dir, ok := cfg.Dir("default")
	if !ok {
		t.Fatalf("missing default dir")
	}
	fingerprint := resourceAuthorizationFingerprint(dir)
	if err := s.transfers.tryAcquireUploadPermit(&uploadPermit{ID: "occupied", DirID: dir.ID, ResourceFingerprint: fingerprint, OwnerType: "session", OwnerID: "one"}, uploadAdmissionLimits{Global: 10, PerResource: 1}); err != nil {
		t.Fatalf("acquire occupied permit: %v", err)
	}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	app.Get("/", func(c *fiber.Ctx) error {
		_, err := s.acquireUploadPermit(c, dir.ID, fingerprint, "session", "two")
		return err
	})
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("register transfer: %v", err)
	}
	var payload map[string]any
	decodeJSON(t, resp, &payload)
	if resp.StatusCode != http.StatusTooManyRequests || payload["code"] != "upload_concurrency_limited" || resp.Header.Get("Retry-After") != "5" {
		t.Fatalf("unexpected scoped capacity response: status=%d payload=%+v retry=%q", resp.StatusCode, payload, resp.Header.Get("Retry-After"))
	}
	if len(s.transfers.permits) != 1 {
		t.Fatalf("rejected request must not acquire a permit")
	}
}

func TestPublicRawUploadDiskFailureRollsBackToken(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(root)
	token := &store.Token{Hash: security.HashToken("disk-token"), Type: "upload", DirID: "default", Path: "", ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), MaxUses: 1, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))}
	if err := st.CreateToken(token); err != nil {
		t.Fatalf("create token: %v", err)
	}
	diskAvailable := uint64(1 << 40)
	s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry(), availableDiskSpace: func(string) (uint64, uint64, error) { return diskAvailable, 1 << 40, nil }}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler, StreamRequestBody: true})
	s.routes(app)
	leaseReq := httptest.NewRequest(http.MethodPost, "/t/disk-token/upload-lease", strings.NewReader(`{"fileName":"disk-fail.txt","fileSize":5}`))
	leaseReq.Header.Set("Content-Type", "application/json")
	leaseResp, err := app.Test(leaseReq)
	if err != nil {
		t.Fatalf("create lease: %v", err)
	}
	var lease uploadLeaseResponse
	decodeJSON(t, leaseResp, &lease)
	if leaseResp.StatusCode != http.StatusOK {
		t.Fatalf("lease status=%d", leaseResp.StatusCode)
	}
	diskAvailable = 1
	rawReq := httptest.NewRequest(http.MethodPost, lease.RawUploadURL, strings.NewReader("hello"))
	rawReq.Header.Set("Authorization", "Bearer "+lease.Lease)
	rawReq.ContentLength = 5
	rawResp, err := app.Test(rawReq)
	if err != nil {
		t.Fatalf("raw disk failure: %v", err)
	}
	var payload map[string]any
	decodeJSON(t, rawResp, &payload)
	if rawResp.StatusCode != http.StatusServiceUnavailable || payload["code"] != "storage_reserve_unavailable" || rawResp.Header.Get("Retry-After") != "60" {
		t.Fatalf("unexpected disk response: status=%d payload=%+v", rawResp.StatusCode, payload)
	}
	loaded, err := st.TokenByID(token.ID)
	if err != nil {
		t.Fatalf("load token: %v", err)
	}
	if loaded.Uses != 0 || loaded.UploadedBytes != 0 {
		t.Fatalf("disk rejection must roll back token, uses=%d bytes=%d", loaded.Uses, loaded.UploadedBytes)
	}
	assertNoUploadTempFiles(t, root)
	if _, err := os.Stat(filepath.Join(root, "disk-fail.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disk rejection created final file: %v", err)
	}
}

func TestMultipartRequestPermitRejectsBeforeFirstPartBody(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(root)
	cfg.Abuse.Uploads.Global = 1
	if err := st.CreateSession("permit-session", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry(), availableDiskSpace: func(string) (uint64, uint64, error) { return 1 << 40, 1 << 40, nil }}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler, StreamRequestBody: true})
	s.routes(app)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serverErr := make(chan error, 1)
	go func() { serverErr <- app.Listener(ln) }()
	defer func() {
		_ = app.Shutdown()
		<-serverErr
	}()
	client := &http.Client{Timeout: 5 * time.Second}
	reader, writer := io.Pipe()
	firstDone := make(chan *http.Response, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/api/files/upload?dirId=default", reader)
		req.Header.Set("Content-Type", "multipart/form-data; boundary=stalled")
		req.AddCookie(&http.Cookie{Name: "sid", Value: "permit-session"})
		resp, _ := client.Do(req)
		firstDone <- resp
	}()
	deadline := time.Now().Add(3 * time.Second)
	for s.transfers.uploadPermitCount() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.transfers.uploadPermitCount() != 1 {
		t.Fatalf("first multipart request did not acquire permit before first part")
	}
	secondReq, err := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/api/files/upload?dirId=default", strings.NewReader("--second--\r\n"))
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	secondReq.Header.Set("Content-Type", "multipart/form-data; boundary=second")
	secondReq.AddCookie(&http.Cookie{Name: "sid", Value: "permit-session"})
	secondResp, err := client.Do(secondReq)
	if err != nil {
		t.Fatalf("second multipart request: %v", err)
	}
	var payload map[string]any
	decodeJSON(t, secondResp, &payload)
	if secondResp.StatusCode != http.StatusServiceUnavailable || payload["code"] != "upload_capacity_exhausted" {
		t.Fatalf("expected immediate global permit rejection, status=%d payload=%+v", secondResp.StatusCode, payload)
	}
	_ = writer.CloseWithError(errors.New("test complete"))
	if firstResp := <-firstDone; firstResp != nil {
		firstResp.Body.Close()
	}
	assertNoUploadTempFiles(t, root)
}

func TestMultipartPermitOwnerLimitsPublicTokenAndUploadLease(t *testing.T) {
	t.Run("public legacy token not consumed", func(t *testing.T) {
		root := t.TempDir()
		st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		defer st.DB.Close()
		cfg := testConfig(root)
		cfg.Abuse.Uploads.Global = 10
		cfg.Abuse.Uploads.PerToken = 1
		token := &store.Token{Hash: security.HashToken("multipart-owner-token"), Type: "upload", DirID: "default", ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), MaxUses: 2, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))}
		if err := st.CreateToken(token); err != nil {
			t.Fatalf("create token: %v", err)
		}
		s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry(), availableDiskSpace: func(string) (uint64, uint64, error) { return 1 << 40, 1 << 40, nil }}
		if err := s.transfers.tryAcquireUploadPermit(&uploadPermit{ID: "occupied-token", DirID: "default", ResourceFingerprint: token.ResourceFingerprint, OwnerType: "token", OwnerID: strconv.FormatInt(token.ID, 10)}, uploadAdmissionLimits{Global: 10, PerToken: 1}); err != nil {
			t.Fatalf("occupy token permit: %v", err)
		}
		app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler, StreamRequestBody: true})
		s.routes(app)
		body, contentType := multipartUploadBody(t, "public.txt", []byte("hello"))
		req := httptest.NewRequest(http.MethodPost, "/t/multipart-owner-token/upload", body)
		req.Header.Set("Content-Type", contentType)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("public multipart: %v", err)
		}
		var payload map[string]any
		decodeJSON(t, resp, &payload)
		if resp.StatusCode != http.StatusTooManyRequests || payload["code"] != "upload_concurrency_limited" {
			t.Fatalf("unexpected public owner response: status=%d payload=%+v", resp.StatusCode, payload)
		}
		loaded, err := st.TokenByID(token.ID)
		if err != nil || loaded.Uses != 0 || loaded.UploadedBytes != 0 {
			t.Fatalf("permit failure must not consume public token: token=%+v err=%v", loaded, err)
		}
		assertNoUploadTempFiles(t, root)
	})

	t.Run("upload lease remains single use", func(t *testing.T) {
		root := t.TempDir()
		st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		defer st.DB.Close()
		cfg := testConfig(root)
		cfg.Abuse.Uploads.Global = 10
		cfg.Abuse.Uploads.PerSession = 1
		if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
			t.Fatalf("create session: %v", err)
		}
		s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry(), availableDiskSpace: func(string) (uint64, uint64, error) { return 1 << 40, 1 << 40, nil }}
		app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler, StreamRequestBody: true})
		s.routes(app)
		lease := createTestUploadLease(t, app, "lease-owner.txt", 5)
		storedBefore, err := st.UploadLeaseByHash(security.HashToken(lease.Lease))
		if err != nil {
			t.Fatalf("load upload lease: %v", err)
		}
		if err := s.transfers.tryAcquireUploadPermit(&uploadPermit{ID: "occupied-session", DirID: "default", ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), OwnerType: "session", OwnerID: storedBefore.SessionID}, uploadAdmissionLimits{Global: 10, PerSession: 1}); err != nil {
			t.Fatalf("occupy session permit: %v", err)
		}
		body, contentType := multipartUploadBody(t, "ignored.txt", []byte("hello"))
		req := httptest.NewRequest(http.MethodPost, lease.UploadURL, body)
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Authorization", "Bearer "+lease.Lease)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("lease multipart: %v", err)
		}
		var payload map[string]any
		decodeJSON(t, resp, &payload)
		if resp.StatusCode != http.StatusTooManyRequests || payload["code"] != "upload_concurrency_limited" {
			t.Fatalf("unexpected lease owner response: status=%d payload=%+v", resp.StatusCode, payload)
		}
		stored, err := st.UploadLeaseByHash(security.HashToken(lease.Lease))
		if err != nil || !stored.UsedAt.Valid {
			t.Fatalf("permit failure must keep lease fail-closed: lease=%+v err=%v", stored, err)
		}
		assertNoUploadTempFiles(t, root)
	})
}

func TestMultipartDirIDBindingEnforcesResourcePermitBeforeFile(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(root)
	cfg.Abuse.Uploads.Global = 10
	cfg.Abuse.Uploads.PerResource = 1
	if err := st.CreateSession("binding-session", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry(), availableDiskSpace: func(string) (uint64, uint64, error) { return 1 << 40, 1 << 40, nil }}
	fingerprint := testResourceFingerprint(t, cfg, "default")
	if err := s.transfers.tryAcquireUploadPermit(&uploadPermit{ID: "occupied-resource", DirID: "default", ResourceFingerprint: fingerprint, OwnerType: "session", OwnerID: "other-session"}, uploadAdmissionLimits{Global: 10, PerResource: 1}); err != nil {
		t.Fatalf("occupy resource permit: %v", err)
	}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler, StreamRequestBody: true})
	s.routes(app)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("dirId", "default"); err != nil {
		t.Fatalf("write dirId: %v", err)
	}
	part, err := writer.CreateFormFile("files", "binding.txt")
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	_, _ = part.Write([]byte("hello"))
	_ = writer.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/files/upload", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: "sid", Value: "binding-session"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("binding upload: %v", err)
	}
	var payload map[string]any
	decodeJSON(t, resp, &payload)
	if resp.StatusCode != http.StatusTooManyRequests || payload["code"] != "upload_concurrency_limited" {
		t.Fatalf("unexpected bind rejection: status=%d payload=%+v", resp.StatusCode, payload)
	}
	assertNoUploadTempFiles(t, root)
}

func TestUploadFinalCommitRejectsPublishedResourceChangeAfterTempClose(t *testing.T) {
	s, app, st, oldRoot, newRoot := newTransferGateTestServer(t)
	cfg := s.cfg()
	token := &store.Token{Hash: security.HashToken("final-race-token"), Type: "upload", DirID: "default", ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), MaxUses: 1, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))}
	if err := st.CreateToken(token); err != nil {
		t.Fatalf("create token: %v", err)
	}
	leaseReq := httptest.NewRequest(http.MethodPost, "/t/final-race-token/upload-lease", strings.NewReader(`{"fileName":"final-race.txt","fileSize":5}`))
	leaseReq.Header.Set("Content-Type", "application/json")
	leaseResp, err := app.Test(leaseReq)
	if err != nil {
		t.Fatalf("create lease: %v", err)
	}
	var lease uploadLeaseResponse
	decodeJSON(t, leaseResp, &lease)
	if leaseResp.StatusCode != http.StatusOK {
		t.Fatalf("lease status=%d", leaseResp.StatusCode)
	}
	reached := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.beforeUploadFinalCommit = func() {
		once.Do(func() { close(reached) })
		<-release
	}
	type requestResult struct {
		resp *http.Response
		err  error
	}
	done := make(chan requestResult, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, lease.RawUploadURL, strings.NewReader("hello"))
		req.Header.Set("Authorization", "Bearer "+lease.Lease)
		req.ContentLength = 5
		resp, requestErr := app.Test(req)
		done <- requestResult{resp: resp, err: requestErr}
	}()
	select {
	case <-reached:
	case <-time.After(3 * time.Second):
		t.Fatalf("upload did not pause before final commit")
	}
	storedLease, err := st.UploadLeaseByHash(security.HashToken(lease.Lease))
	if err != nil || !storedLease.UsedAt.Valid {
		t.Fatalf("lease must already be used before final commit: lease=%+v err=%v", storedLease, err)
	}
	var tempPath string
	s.transfers.mu.RLock()
	for _, rec := range s.transfers.records {
		if rec.Type == "upload" && rec.Status == transferActive {
			tempPath = rec.TempPath
			break
		}
	}
	s.transfers.mu.RUnlock()
	if tempPath == "" {
		t.Fatalf("expected active progress record before final commit")
	}
	if _, err := os.Stat(tempPath); err != nil {
		t.Fatalf("closed staging temp must still exist before commit: %v", err)
	}
	if err := s.updateConfigResources(func(resources []config.Dir) ([]config.Dir, error) {
		for i := range resources {
			if resources[i].ID == "default" {
				resources[i].Path = newRoot
			}
		}
		return resources, nil
	}); err != nil {
		t.Fatalf("publish changed resource: %v", err)
	}
	close(release)
	result := <-done
	if result.resp != nil {
		result.resp.Body.Close()
	}
	if result.err == nil && result.resp != nil && result.resp.StatusCode == http.StatusOK {
		t.Fatalf("publish-before-commit upload unexpectedly succeeded")
	}
	loaded, err := st.TokenByID(token.ID)
	if err != nil {
		t.Fatalf("load token: %v", err)
	}
	if loaded.Uses != 0 || loaded.UploadedBytes != 0 {
		t.Fatalf("failed final commit must roll back token, uses=%d bytes=%d", loaded.Uses, loaded.UploadedBytes)
	}
	if s.transfers.uploadPermitCount() != 0 {
		t.Fatalf("handler defer must release request permit")
	}
	recordReleased := false
	s.transfers.mu.RLock()
	for _, rec := range s.transfers.records {
		if rec.FileName == "final-race.txt" {
			recordReleased = rec.Status == transferCompleted && rec.TempPath == ""
		}
	}
	s.transfers.mu.RUnlock()
	if !recordReleased {
		t.Fatalf("handler defer must release progress record and clear temp path")
	}
	if _, err := os.Stat(filepath.Join(oldRoot, "final-race.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("publish-before-commit created final file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(newRoot, "final-race.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("publish-before-commit created file in new root: %v", err)
	}
	assertNoUploadTempFiles(t, oldRoot)
}

func TestUploadFinalCommitBeforePublishKeepsFile(t *testing.T) {
	s, app, st, oldRoot, newRoot := newTransferGateTestServer(t)
	if err := st.CreateSession("commit-first-session", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	lease := createTestUploadLeaseForSession(t, app, "commit-first-session", "commit-first.txt", 5)
	reached := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.beforeUploadFinalCommit = func() {
		once.Do(func() { close(reached) })
		<-release
	}
	done := make(chan *http.Response, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, lease.RawUploadURL, strings.NewReader("hello"))
		req.Header.Set("Authorization", "Bearer "+lease.Lease)
		req.ContentLength = 5
		resp, _ := app.Test(req)
		done <- resp
	}()
	<-reached
	close(release)
	resp := <-done
	if resp == nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatalf("commit-first upload failed")
	}
	resp.Body.Close()
	if err := s.updateConfigResources(func(resources []config.Dir) ([]config.Dir, error) {
		for i := range resources {
			if resources[i].ID == "default" {
				resources[i].Path = newRoot
			}
		}
		return resources, nil
	}); err != nil {
		t.Fatalf("publish after commit: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(oldRoot, "commit-first.txt"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("commit-first file was not retained: data=%q err=%v", data, err)
	}
}

func TestUploadFinalCommitChecksEachSameNameCandidate(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "same.txt"), []byte("existing"), 0600); err != nil {
		t.Fatalf("write existing file: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(root)
	if err := st.CreateSession("same-name-session", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry(), availableDiskSpace: func(string) (uint64, uint64, error) { return 1 << 40, 1 << 40, nil }}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler, StreamRequestBody: true})
	s.routes(app)
	lease := createTestUploadLeaseForSession(t, app, "same-name-session", "same.txt", 5)
	commitChecks := 0
	s.beforeUploadFinalCommit = func() { commitChecks++ }
	req := httptest.NewRequest(http.MethodPost, lease.RawUploadURL, strings.NewReader("hello"))
	req.Header.Set("Authorization", "Bearer "+lease.Lease)
	req.ContentLength = 5
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("same-name upload: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || commitChecks != 2 {
		t.Fatalf("expected two independently gated candidates, status=%d checks=%d", resp.StatusCode, commitChecks)
	}
	existing, _ := os.ReadFile(filepath.Join(root, "same.txt"))
	created, createErr := os.ReadFile(filepath.Join(root, "same-1.txt"))
	if string(existing) != "existing" || createErr != nil || string(created) != "hello" {
		t.Fatalf("same-name commit overwrote or lost files: existing=%q created=%q err=%v", existing, created, createErr)
	}
}

func TestCapabilityResponsesAlwaysSetSecurityHeadersIncludingRange(t *testing.T) {
	root := t.TempDir()
	content := []byte("0123456789abcdef")
	if err := os.WriteFile(filepath.Join(root, "capability.txt"), content, 0600); err != nil {
		t.Fatalf("write capability file: %v", err)
	}
	cfg := testConfig(root)
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	if err := st.CreateSession("capability-session", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	fingerprint := testResourceFingerprint(t, cfg, "default")
	for _, token := range []*store.Token{
		{Hash: security.HashToken("capability-download"), Type: "download", DirID: "default", Path: "capability.txt", ResourceFingerprint: fingerprint, MaxUses: 2, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))},
		{Hash: security.HashToken("capability-upload"), Type: "upload", DirID: "default", ResourceFingerprint: fingerprint, MaxUses: 2, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))},
	} {
		if err := st.CreateToken(token); err != nil {
			t.Fatalf("create token: %v", err)
		}
	}
	app := New(cfg, st)
	request := func(method, path, body, sessionID string, headers map[string]string) *http.Response {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if sessionID != "" {
			req.AddCookie(&http.Cookie{Name: "sid", Value: sessionID})
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		resp, requestErr := app.Test(req)
		if requestErr != nil {
			t.Fatalf("%s %s: %v", method, path, requestErr)
		}
		assertCapabilityHeaders(t, resp)
		return resp
	}

	for _, tc := range []struct {
		method, path, body, session string
	}{
		{http.MethodGet, "/t/capability-upload/upload", "", ""},
		{http.MethodGet, "/t/capability-download/info", "", ""},
		{http.MethodGet, "/t/missing-capability/info", "", ""},
		{http.MethodPost, "/api/tokens", `{}`, ""},
		{http.MethodPost, "/api/files/upload-by-lease", "", ""},
	} {
		resp := request(tc.method, tc.path, tc.body, tc.session, nil)
		resp.Body.Close()
	}

	publicLeaseResp := request(http.MethodPost, "/t/capability-download/download-lease", "", "", nil)
	var publicLease downloadLeaseResponse
	decodeJSON(t, publicLeaseResp, &publicLease)
	if publicLeaseResp.StatusCode != http.StatusOK || publicLease.URL == "" {
		t.Fatalf("public lease response status=%d lease=%+v", publicLeaseResp.StatusCode, publicLease)
	}
	publicDownload := request(http.MethodGet, publicLease.URL, "", "", nil)
	publicBody, err := io.ReadAll(publicDownload.Body)
	publicDownload.Body.Close()
	if err != nil || publicDownload.StatusCode != http.StatusOK || !bytes.Equal(publicBody, content) {
		t.Fatalf("public capability download status=%d body=%q err=%v", publicDownload.StatusCode, publicBody, err)
	}

	downloadLeaseResp := request(http.MethodPost, "/api/files/download-lease", `{"dirId":"default","path":"capability.txt"}`, "capability-session", nil)
	var lease downloadLeaseResponse
	decodeJSON(t, downloadLeaseResp, &lease)
	if downloadLeaseResp.StatusCode != http.StatusOK || lease.URL == "" {
		t.Fatalf("authenticated lease response status=%d lease=%+v", downloadLeaseResp.StatusCode, lease)
	}
	uploadLeaseResp := request(http.MethodPost, "/api/files/upload-lease", `{"dirId":"default","path":"","fileName":"capability-upload.txt","fileSize":1}`, "capability-session", nil)
	uploadLeaseResp.Body.Close()
	if uploadLeaseResp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated upload lease status=%d", uploadLeaseResp.StatusCode)
	}

	full := request(http.MethodGet, lease.URL, "", "", nil)
	fullBody, err := io.ReadAll(full.Body)
	full.Body.Close()
	if err != nil || full.StatusCode != http.StatusOK || !bytes.Equal(fullBody, content) {
		t.Fatalf("full capability download status=%d body=%q err=%v", full.StatusCode, fullBody, err)
	}
	for _, byteRange := range []string{"bytes=0-3", "bytes=4-7"} {
		partial := request(http.MethodGet, lease.URL, "", "", map[string]string{"Range": byteRange})
		_, _ = io.Copy(io.Discard, partial.Body)
		partial.Body.Close()
		if partial.StatusCode != http.StatusPartialContent {
			t.Fatalf("range %q status=%d", byteRange, partial.StatusCode)
		}
	}
}

func TestCapabilityHeadersPrecedeCORSForOptionsAndRejectedOrigins(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.CORS.AllowOrigins = []string{"https://allowed.example"}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app := New(cfg, st)
	for _, path := range []string{
		"/t/options-token/upload",
		"/api/tokens",
		"/api/files/download-lease",
		"/api/files/download-by-lease",
		"/api/files/upload-lease",
		"/api/files/upload-raw-by-lease",
		"/api/files/upload-by-lease",
	} {
		req := httptest.NewRequest(http.MethodOptions, path, nil)
		req.Header.Set("Origin", "https://rejected.example")
		req.Header.Set("Access-Control-Request-Method", http.MethodPost)
		resp, requestErr := app.Test(req)
		if requestErr != nil {
			t.Fatalf("OPTIONS %s: %v", path, requestErr)
		}
		assertCapabilityHeaders(t, resp)
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("rejected origin unexpectedly allowed for %s: %q", path, got)
		}
		resp.Body.Close()
	}
	ordinaryReq := httptest.NewRequest(http.MethodOptions, "/api/health", nil)
	ordinaryReq.Header.Set("Origin", "https://rejected.example")
	ordinaryReq.Header.Set("Access-Control-Request-Method", http.MethodGet)
	ordinaryResp, err := app.Test(ordinaryReq)
	if err != nil {
		t.Fatalf("ordinary OPTIONS: %v", err)
	}
	ordinaryResp.Body.Close()
	if ordinaryResp.Header.Get("X-Robots-Tag") != "" || ordinaryResp.Header.Get("Cache-Control") == "no-store" {
		t.Fatalf("ordinary non-capability API received capability headers")
	}
}

func TestCheckedFiberBodyLimitAndKeepaliveIdleTimeout(t *testing.T) {
	limit, err := checkedFiberBodyLimit(2047, math.MaxInt32)
	if err != nil || int64(limit) != int64(2047)*1024*1024 {
		t.Fatalf("expected representable simulated 32-bit body limit, limit=%d err=%v", limit, err)
	}
	if _, err := checkedFiberBodyLimit(2048, math.MaxInt32); err == nil {
		t.Fatalf("expected simulated 32-bit overflow rejected")
	}
	cfg := testConfig(t.TempDir())
	cfg.Server.KeepaliveIdleTimeoutSeconds = 321
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app := New(cfg, st)
	appCfg := app.Config()
	if appCfg.IdleTimeout != 321*time.Second {
		t.Fatalf("unexpected Fiber idle timeout: %v", appCfg.IdleTimeout)
	}
	if appCfg.ReadTimeout != 0 || appCfg.WriteTimeout != 0 {
		t.Fatalf("read/write timeout must remain disabled: read=%v write=%v", appCfg.ReadTimeout, appCfg.WriteTimeout)
	}
	s := &Server{config: cfg, store: st}
	safeApp := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	safeApp.Get("/", s.safeConfig)
	resp, err := safeApp.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("safe config: %v", err)
	}
	var payload map[string]any
	decodeJSON(t, resp, &payload)
	serverPayload, ok := payload["server"].(map[string]any)
	if !ok || serverPayload["keepaliveIdleTimeoutSeconds"] != float64(321) {
		t.Fatalf("safe config did not expose read-only idle timeout: %+v", payload)
	}
}

func TestDownloadLeaseCreationRejectsSameSizeMtimeReplacementAfterHash(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "replace.txt")
	fixedTime := time.Unix(1700000000, 0)
	if err := os.WriteFile(path, []byte("old!"), 0600); err != nil {
		t.Fatalf("write original: %v", err)
	}
	if err := os.Chtimes(path, fixedTime, fixedTime); err != nil {
		t.Fatalf("set original time: %v", err)
	}
	cfg := testConfig(root)
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	if err := st.CreateSession("hash-race-session", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry()}
	var once sync.Once
	s.duringDownloadFileHash = func() {
		once.Do(func() {
			replacement := filepath.Join(root, "replacement.tmp")
			if err := os.WriteFile(replacement, []byte("new!"), 0600); err != nil {
				t.Fatalf("write replacement: %v", err)
			}
			_ = os.Chtimes(replacement, fixedTime, fixedTime)
			if err := os.Rename(replacement, path); err != nil {
				t.Fatalf("replace file: %v", err)
			}
		})
	}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	s.routes(app)
	req := httptest.NewRequest(http.MethodPost, "/api/files/download-lease", strings.NewReader(`{"dirId":"default","path":"replace.txt"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "sid", Value: "hash-race-session"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("create lease: %v", err)
	}
	var payload map[string]any
	decodeJSON(t, resp, &payload)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("same-size/mtime replacement after hash must be rejected, got %d", resp.StatusCode)
	}
	if payload["code"] != "download_file_changed" {
		t.Fatalf("expected stable changed code, payload=%+v", payload)
	}
}

func TestDownloadFinalValidationRejectsSameFileIdentityChange(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "direct.txt")
	fixedTime := time.Unix(1700000100, 0)
	if err := os.WriteFile(path, []byte("old!"), 0600); err != nil {
		t.Fatalf("write original: %v", err)
	}
	_ = os.Chtimes(path, fixedTime, fixedTime)
	cfg := testConfig(root)
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	if err := st.CreateSession("download-race-session", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry()}
	var once sync.Once
	s.beforeDownloadFinalValidation = func() {
		once.Do(func() {
			replacement := filepath.Join(root, "direct-replacement.tmp")
			_ = os.WriteFile(replacement, []byte("new!"), 0600)
			_ = os.Chtimes(replacement, fixedTime, fixedTime)
			_ = os.Rename(replacement, path)
		})
	}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	s.routes(app)
	req := httptest.NewRequest(http.MethodGet, "/api/files/download?dirId=default&path=direct.txt", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: "download-race-session"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("direct download: %v", err)
	}
	var payload map[string]any
	decodeJSON(t, resp, &payload)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("download final identity change must be rejected, got %d", resp.StatusCode)
	}
	if payload["code"] != "download_file_changed" {
		t.Fatalf("expected stable changed code, payload=%+v", payload)
	}
	logs, err := st.AuditLogs(100)
	if err != nil {
		t.Fatalf("audit logs: %v", err)
	}
	for _, entry := range logs {
		if entry.Action == "download" {
			t.Fatalf("failed final revalidation must not be audited as download: %+v", entry)
		}
	}
}

func TestDownloadByLeaseFinalRevalidationFailureDoesNotTouchOrAuditUse(t *testing.T) {
	for _, mode := range []string{"replace", "remove"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "lease-final.txt")
			fixedTime := time.Unix(1700000200, 0)
			if err := os.WriteFile(path, []byte("old!"), 0600); err != nil {
				t.Fatalf("write original: %v", err)
			}
			if err := os.Chtimes(path, fixedTime, fixedTime); err != nil {
				t.Fatalf("set mtime: %v", err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat original: %v", err)
			}
			hashValue, err := fileSHA256Hex(path)
			if err != nil {
				t.Fatalf("hash original: %v", err)
			}
			cfg := testConfig(root)
			st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			defer st.DB.Close()
			plain := "final-revalidation-" + mode
			lease := &store.DownloadLease{
				Hash:                security.HashToken(plain),
				Source:              "session",
				DirID:               "default",
				Path:                "lease-final.txt",
				FileSize:            info.Size(),
				FileMtime:           normalizedFileMtime(info),
				FileSHA256:          sql.NullString{String: hashValue, Valid: true},
				ResourceFingerprint: testResourceFingerprint(t, cfg, "default"),
				ExpiresAt:           time.Now().Add(time.Hour),
			}
			if err := st.CreateDownloadLease(lease); err != nil {
				t.Fatalf("create lease: %v", err)
			}
			s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry()}
			var once sync.Once
			s.beforeDownloadFinalValidation = func() {
				once.Do(func() {
					if mode == "remove" {
						_ = os.Remove(path)
						return
					}
					replacement := filepath.Join(root, "lease-replacement.tmp")
					_ = os.WriteFile(replacement, []byte("new!"), 0600)
					_ = os.Chtimes(replacement, fixedTime, fixedTime)
					_ = os.Rename(replacement, path)
				})
			}
			app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
			s.routes(app)
			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/files/download-by-lease?lease="+plain, nil))
			if err != nil {
				t.Fatalf("download by lease: %v", err)
			}
			var payload map[string]any
			decodeJSON(t, resp, &payload)
			if resp.StatusCode != http.StatusConflict || payload["code"] != "download_file_changed" {
				t.Fatalf("unexpected final revalidation response: status=%d payload=%+v", resp.StatusCode, payload)
			}
			loaded, err := st.DownloadLeaseByHash(lease.Hash)
			if err != nil {
				t.Fatalf("load lease: %v", err)
			}
			if loaded.LastUsedAt.Valid {
				t.Fatalf("failed final revalidation updated last_used_at: %+v", loaded.LastUsedAt)
			}
			logs, err := st.AuditLogs(100)
			if err != nil {
				t.Fatalf("audit logs: %v", err)
			}
			for _, entry := range logs {
				if entry.Action == "download_lease_use" {
					t.Fatalf("failed final revalidation wrote use audit: %+v", entry)
				}
			}
		})
	}
}

func TestDownloadLeaseConcurrentFirstRangeHashesAndAuditsOnce(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "concurrent.bin")
	content := bytes.Repeat([]byte("range-data-"), 1024)
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	info, _ := os.Stat(path)
	contentHash, err := fileSHA256Hex(path)
	if err != nil {
		t.Fatalf("hash file: %v", err)
	}
	cfg := testConfig(root)
	cfg.Downloads.MaxConcurrentHashes = 2
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	if err := st.CreateSession("lease-owner-session", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	plain := "concurrent-first-use"
	lease := &store.DownloadLease{Hash: security.HashToken(plain), Source: "session", SessionID: sql.NullString{String: "lease-owner-session", Valid: true}, DirID: "default", Path: "concurrent.bin", ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), FileSize: info.Size(), FileMtime: normalizedFileMtime(info), FileSHA256: sql.NullString{String: contentHash, Valid: true}, ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.CreateDownloadLease(lease); err != nil {
		t.Fatalf("create lease: %v", err)
	}
	s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry(), downloadHashSlots: make(chan struct{}, 2), downloadHashFlights: make(map[string]*downloadHashFlight)}
	var hashCount atomic.Int32
	hashStarted := make(chan struct{})
	releaseHash := make(chan struct{})
	var once sync.Once
	s.duringDownloadFileHash = func() {
		hashCount.Add(1)
		once.Do(func() { close(hashStarted) })
		<-releaseHash
	}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	s.routes(app)
	type result struct {
		status int
		err    error
	}
	results := make(chan result, 6)
	for i := 0; i < 6; i++ {
		go func(i int) {
			req := httptest.NewRequest(http.MethodGet, "/api/files/download-by-lease?lease="+plain, nil)
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", i*10, i*10+9))
			resp, requestErr := app.Test(req)
			if resp != nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				results <- result{status: resp.StatusCode, err: requestErr}
				return
			}
			results <- result{err: requestErr}
		}(i)
	}
	select {
	case <-hashStarted:
	case <-time.After(3 * time.Second):
		t.Fatalf("first hash did not start")
	}
	time.Sleep(50 * time.Millisecond)
	close(releaseHash)
	for i := 0; i < 6; i++ {
		result := <-results
		if result.err != nil || result.status != http.StatusPartialContent {
			t.Fatalf("concurrent range result=%+v", result)
		}
	}
	if hashCount.Load() != 1 {
		t.Fatalf("expected one singleflight hash, got %d", hashCount.Load())
	}
	loaded, err := st.DownloadLeaseByHash(lease.Hash)
	if err != nil || !loaded.LastUsedAt.Valid {
		t.Fatalf("first use was not persisted: lease=%+v err=%v", loaded, err)
	}
	firstUsedAt := loaded.LastUsedAt.Time
	logs, err := st.AuditLogs(100)
	if err != nil {
		t.Fatalf("audit logs: %v", err)
	}
	useAudits := 0
	for _, entry := range logs {
		if entry.Action == "download_lease_use" {
			useAudits++
		}
	}
	if useAudits != 1 {
		t.Fatalf("expected one first-use audit, got %d", useAudits)
	}
	if err := st.DeleteSession("lease-owner-session"); err != nil {
		t.Fatalf("delete owner session: %v", err)
	}
	s.duringDownloadFileHash = func() { hashCount.Add(1) }
	for _, byteRange := range []string{"bytes=100-109", "bytes=110-119"} {
		req := httptest.NewRequest(http.MethodGet, "/api/files/download-by-lease?lease="+plain, nil)
		req.Header.Set("Range", byteRange)
		resp, err := app.Test(req)
		if err != nil || resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("subsequent range %s status=%v err=%v", byteRange, responseStatus(resp), err)
		}
		resp.Body.Close()
	}
	if hashCount.Load() != 1 {
		t.Fatalf("default subsequent ranges unexpectedly rehashed: %d", hashCount.Load())
	}
	loaded, _ = st.DownloadLeaseByHash(lease.Hash)
	if !loaded.LastUsedAt.Time.Equal(firstUsedAt) {
		t.Fatalf("subsequent ranges rewrote last_used_at: before=%v after=%v", firstUsedAt, loaded.LastUsedAt.Time)
	}
	logs, _ = st.AuditLogs(100)
	useAudits = 0
	for _, entry := range logs {
		if entry.Action == "download_lease_use" {
			useAudits++
		}
	}
	if useAudits != 1 {
		t.Fatalf("subsequent ranges wrote additional first-use audits: %d", useAudits)
	}

	restarted := &Server{config: cfg.Clone(), store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry(), downloadHashSlots: make(chan struct{}, 2), downloadHashFlights: make(map[string]*downloadHashFlight)}
	var restartHashes atomic.Int32
	restarted.duringDownloadFileHash = func() { restartHashes.Add(1) }
	restartApp := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	restarted.routes(restartApp)
	req := httptest.NewRequest(http.MethodGet, "/api/files/download-by-lease?lease="+plain, nil)
	req.Header.Set("Range", "bytes=120-129")
	resp, err := restartApp.Test(req)
	if err != nil || resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("restart range status=%v err=%v", responseStatus(resp), err)
	}
	resp.Body.Close()
	if restartHashes.Load() != 0 {
		t.Fatalf("persisted first use was not respected after restart")
	}
}

func responseStatus(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func TestDownloadLeaseStrictModeHashesEveryRequest(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "strict.txt")
	if err := os.WriteFile(path, []byte("strict-content"), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	info, _ := os.Stat(path)
	hashValue, _ := fileSHA256Hex(path)
	cfg := testConfig(root)
	cfg.Downloads.VerifyHashOnEveryRequest = true
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	plain := "strict-lease"
	lease := &store.DownloadLease{Hash: security.HashToken(plain), Source: "session", DirID: "default", Path: "strict.txt", ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), FileSize: info.Size(), FileMtime: normalizedFileMtime(info), FileSHA256: sql.NullString{String: hashValue, Valid: true}, ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.CreateDownloadLease(lease); err != nil {
		t.Fatalf("create lease: %v", err)
	}
	if first, err := st.MarkDownloadLeaseFirstUsed(lease.Hash, time.Now()); err != nil || !first {
		t.Fatalf("mark existing use: first=%v err=%v", first, err)
	}
	s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry(), downloadHashSlots: make(chan struct{}, 2), downloadHashFlights: make(map[string]*downloadHashFlight)}
	var hashes atomic.Int32
	s.duringDownloadFileHash = func() { hashes.Add(1) }
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	s.routes(app)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/files/download-by-lease?lease="+plain, nil)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", i, i))
		resp, err := app.Test(req)
		if err != nil || resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("strict request %d status=%v err=%v", i, responseStatus(resp), err)
		}
		resp.Body.Close()
	}
	if hashes.Load() != 2 {
		t.Fatalf("strict mode expected two hashes, got %d", hashes.Load())
	}
}

func TestDownloadHashCapacityRejectsUnrelatedLeaseWithoutMarkingUse(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "capacity.txt")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 4096), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	info, _ := os.Stat(path)
	hashValue, _ := fileSHA256Hex(path)
	cfg := testConfig(root)
	cfg.Downloads.MaxConcurrentHashes = 1
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	for _, plain := range []string{"capacity-one", "capacity-two"} {
		lease := &store.DownloadLease{Hash: security.HashToken(plain), Source: "session", DirID: "default", Path: "capacity.txt", ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), FileSize: info.Size(), FileMtime: normalizedFileMtime(info), FileSHA256: sql.NullString{String: hashValue, Valid: true}, ExpiresAt: time.Now().Add(time.Hour)}
		if err := st.CreateDownloadLease(lease); err != nil {
			t.Fatalf("create lease: %v", err)
		}
	}
	if err := st.CreateSession("hash-create-session", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry(), downloadHashSlots: make(chan struct{}, 1), downloadHashFlights: make(map[string]*downloadHashFlight)}
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.duringDownloadFileHash = func() {
		once.Do(func() { close(started) })
		<-release
	}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	s.routes(app)
	firstDone := make(chan *http.Response, 1)
	go func() {
		resp, _ := app.Test(httptest.NewRequest(http.MethodGet, "/api/files/download-by-lease?lease=capacity-one", nil))
		firstDone <- resp
	}()
	<-started
	second, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/files/download-by-lease?lease=capacity-two", nil))
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	var payload map[string]any
	decodeJSON(t, second, &payload)
	if second.StatusCode != http.StatusServiceUnavailable || payload["code"] != "download_hash_capacity_exhausted" || second.Header.Get("Retry-After") != "2" {
		t.Fatalf("unexpected capacity response: status=%d headers=%v payload=%+v", second.StatusCode, second.Header, payload)
	}
	secondLease, _ := st.DownloadLeaseByHash(security.HashToken("capacity-two"))
	if secondLease.LastUsedAt.Valid {
		t.Fatalf("capacity-rejected lease was marked used")
	}
	createReq := httptest.NewRequest(http.MethodPost, "/api/files/download-lease", strings.NewReader(`{"dirId":"default","path":"capacity.txt"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.AddCookie(&http.Cookie{Name: "sid", Value: "hash-create-session"})
	createResp, err := app.Test(createReq)
	if err != nil {
		t.Fatalf("create lease while hash busy: %v", err)
	}
	var createPayload map[string]any
	decodeJSON(t, createResp, &createPayload)
	if createResp.StatusCode != http.StatusServiceUnavailable || createPayload["code"] != "download_hash_capacity_exhausted" || createResp.Header.Get("Retry-After") != "2" {
		t.Fatalf("lease creation did not share hash capacity: status=%d payload=%+v", createResp.StatusCode, createPayload)
	}
	close(release)
	first := <-firstDone
	if first == nil || first.StatusCode != http.StatusOK {
		t.Fatalf("first hash request failed: status=%v", responseStatus(first))
	}
	first.Body.Close()
}

func TestDownloadLeaseHashMismatchAndNoHashFirstUseSemantics(t *testing.T) {
	for _, tc := range []struct {
		name       string
		storedHash sql.NullString
		wantStatus int
		wantUsed   bool
		wantHashes int32
	}{
		{name: "mismatch", storedHash: sql.NullString{String: strings.Repeat("0", 64), Valid: true}, wantStatus: http.StatusConflict, wantUsed: false, wantHashes: 1},
		{name: "large without content hash", storedHash: sql.NullString{String: "", Valid: true}, wantStatus: http.StatusPartialContent, wantUsed: true, wantHashes: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "file.bin")
			content := []byte("content")
			if tc.name == "large without content hash" {
				content = bytes.Repeat([]byte("x"), 2*1024*1024)
			}
			if err := os.WriteFile(path, content, 0600); err != nil {
				t.Fatalf("write file: %v", err)
			}
			info, _ := os.Stat(path)
			cfg := testConfig(root)
			if tc.name == "large without content hash" {
				cfg.Downloads.ContentHashMaxMB = 1
			}
			st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			defer st.DB.Close()
			plain := "hash-semantics-" + tc.name
			lease := &store.DownloadLease{Hash: security.HashToken(plain), Source: "session", DirID: "default", Path: "file.bin", ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), FileSize: info.Size(), FileMtime: normalizedFileMtime(info), FileSHA256: tc.storedHash, ExpiresAt: time.Now().Add(time.Hour)}
			if err := st.CreateDownloadLease(lease); err != nil {
				t.Fatalf("create lease: %v", err)
			}
			s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry(), downloadHashSlots: make(chan struct{}, 2), downloadHashFlights: make(map[string]*downloadHashFlight)}
			var hashes atomic.Int32
			s.duringDownloadFileHash = func() { hashes.Add(1) }
			app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
			s.routes(app)
			req := httptest.NewRequest(http.MethodGet, "/api/files/download-by-lease?lease="+url.QueryEscape(plain), nil)
			req.Header.Set("Range", "bytes=0-2")
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.wantStatus || hashes.Load() != tc.wantHashes {
				t.Fatalf("status=%d hashes=%d", resp.StatusCode, hashes.Load())
			}
			loaded, _ := st.DownloadLeaseByHash(lease.Hash)
			if loaded.LastUsedAt.Valid != tc.wantUsed {
				t.Fatalf("unexpected first-use state: %+v", loaded.LastUsedAt)
			}
		})
	}
}

func TestDownloadHashFlightPanicUnblocksWaiters(t *testing.T) {
	s, app, st, plain := newHashFlightTestServer(t, 2, "panic-file.txt")
	panicLog, restoreLog := captureTestLog(t)
	defer restoreLog()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.duringDownloadFileHash = func() {
		once.Do(func() { close(started) })
		<-release
		panic("forced hash panic")
	}
	type result struct {
		status int
		code   any
		err    error
	}
	results := make(chan result, 5)
	request := func() {
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/files/download-by-lease?lease="+url.QueryEscape(plain), nil))
		if err != nil {
			results <- result{err: err}
			return
		}
		var payload map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		results <- result{status: resp.StatusCode, code: payload["code"]}
	}
	go request()
	<-started
	for i := 0; i < 4; i++ {
		go request()
	}
	waitForDownloadHashWaiters(t, s, security.HashToken(plain), 4)
	close(release)
	for i := 0; i < 5; i++ {
		select {
		case got := <-results:
			if got.err != nil || got.status != http.StatusInternalServerError || got.code != "download_hash_failed" {
				t.Fatalf("panic flight result=%+v", got)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("panic flight waiter deadlocked")
		}
	}
	s.downloadHashMu.Lock()
	remaining := len(s.downloadHashFlights)
	s.downloadHashMu.Unlock()
	if remaining != 0 {
		t.Fatalf("panic flight leaked map entry")
	}
	if !strings.Contains(panicLog.String(), "download hash flight panicked") || strings.Contains(panicLog.String(), plain) || strings.Contains(panicLog.String(), "panic-file.txt") {
		t.Fatalf("panic log was missing or leaked capability/path: %q", panicLog.String())
	}
	lease, _ := st.DownloadLeaseByHash(security.HashToken(plain))
	if lease.LastUsedAt.Valid {
		t.Fatalf("panic flight marked lease used")
	}
}

func TestDownloadHashCapacityFlightMapsErrorForEveryWaiter(t *testing.T) {
	s, app, _, plain := newHashFlightTestServer(t, 1, "capacity-waiters.txt")
	s.downloadHashSlots <- struct{}{}
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.beforeDownloadHashAcquire = func() {
		once.Do(func() { close(started) })
		<-release
	}
	type result struct {
		status int
		code   any
		retry  string
	}
	results := make(chan result, 5)
	request := func() {
		resp, _ := app.Test(httptest.NewRequest(http.MethodGet, "/api/files/download-by-lease?lease="+url.QueryEscape(plain), nil))
		var payload map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		results <- result{status: resp.StatusCode, code: payload["code"], retry: resp.Header.Get("Retry-After")}
	}
	go request()
	<-started
	for i := 0; i < 4; i++ {
		go request()
	}
	waitForDownloadHashWaiters(t, s, security.HashToken(plain), 4)
	close(release)
	for i := 0; i < 5; i++ {
		got := <-results
		if got.status != http.StatusServiceUnavailable || got.code != "download_hash_capacity_exhausted" || got.retry != "2" {
			t.Fatalf("capacity waiter result=%+v", got)
		}
	}
	<-s.downloadHashSlots
}

func TestDownloadHashFailureReleasesSlotForAnotherLease(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"failure.txt", "success.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("content-"+name), 0600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	cfg := testConfig(root)
	cfg.Downloads.MaxConcurrentHashes = 1
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	for _, name := range []string{"failure.txt", "success.txt"} {
		path := filepath.Join(root, name)
		info, _ := os.Stat(path)
		hashValue, _ := fileSHA256Hex(path)
		lease := &store.DownloadLease{Hash: security.HashToken(name), Source: "session", DirID: "default", Path: name, ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), FileSize: info.Size(), FileMtime: normalizedFileMtime(info), FileSHA256: sql.NullString{String: hashValue, Valid: true}, ExpiresAt: time.Now().Add(time.Hour)}
		if err := st.CreateDownloadLease(lease); err != nil {
			t.Fatalf("create lease: %v", err)
		}
	}
	s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry(), downloadHashSlots: make(chan struct{}, 1), downloadHashFlights: make(map[string]*downloadHashFlight)}
	var once sync.Once
	s.duringDownloadFileHash = func() {
		once.Do(func() { _ = os.Remove(filepath.Join(root, "failure.txt")) })
	}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	s.routes(app)
	failed, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/files/download-by-lease?lease=failure.txt", nil))
	if err != nil {
		t.Fatalf("failure request: %v", err)
	}
	failed.Body.Close()
	if failed.StatusCode != http.StatusConflict {
		t.Fatalf("expected hash failure conflict, got %d", failed.StatusCode)
	}
	s.duringDownloadFileHash = nil
	succeeded, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/files/download-by-lease?lease=success.txt", nil))
	if err != nil || succeeded.StatusCode != http.StatusOK {
		t.Fatalf("second lease could not acquire released slot: status=%v err=%v", responseStatus(succeeded), err)
	}
	succeeded.Body.Close()
}

func newHashFlightTestServer(t *testing.T, capacity int, fileName string) (*Server, *fiber.App, *store.Store, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, fileName)
	if err := os.WriteFile(path, []byte("hash-flight-content"), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	info, _ := os.Stat(path)
	hashValue, _ := fileSHA256Hex(path)
	cfg := testConfig(root)
	cfg.Downloads.MaxConcurrentHashes = capacity
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.DB.Close() })
	plain := "flight-" + fileName
	lease := &store.DownloadLease{Hash: security.HashToken(plain), Source: "session", DirID: "default", Path: fileName, ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), FileSize: info.Size(), FileMtime: normalizedFileMtime(info), FileSHA256: sql.NullString{String: hashValue, Valid: true}, ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.CreateDownloadLease(lease); err != nil {
		t.Fatalf("create lease: %v", err)
	}
	s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry(), downloadHashSlots: make(chan struct{}, capacity), downloadHashFlights: make(map[string]*downloadHashFlight)}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	s.routes(app)
	return s, app, st, plain
}

func waitForDownloadHashWaiters(t *testing.T, s *Server, hash string, expected int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.downloadHashMu.Lock()
		flight := s.downloadHashFlights[hash]
		waiters := 0
		if flight != nil {
			waiters = flight.waiters
		}
		s.downloadHashMu.Unlock()
		if waiters >= expected {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("expected %d hash flight waiters", expected)
}

func TestAuthFastFailureAvoidsEmptyLookupAndRequestCleanup(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.Audit.Retain = 10000
	cfg.Audit.UnauthorizedSampleSeconds = 60
	cfg.Audit.UnauthorizedGlobalPerMinute = 120
	cfg.Audit.PruneEveryWrites = 0
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), cfg.Audit.Retain)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	st.SetAuditPolicy(cfg.Audit.Retain, 0)
	if err := st.CreateSession("expired-row", time.Now().Add(-time.Hour), "user", ""); err != nil {
		t.Fatalf("create expired row: %v", err)
	}
	var lookups atomic.Int32
	s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry(), auditLimiter: newWindowLimiter(), lookupSession: func(id string) (store.Session, error) {
		lookups.Add(1)
		return st.Session(id)
	}}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	app.Get("/protected", s.auth(func(c *fiber.Ctx) error { return c.SendStatus(http.StatusOK) }))
	for i := 0; i < 1000; i++ {
		resp, requestErr := app.Test(httptest.NewRequest(http.MethodGet, "/protected", nil))
		if requestErr != nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("missing-cookie request %d status=%v err=%v", i, responseStatus(resp), requestErr)
		}
		resp.Body.Close()
	}
	if lookups.Load() != 0 {
		t.Fatalf("empty sid unexpectedly queried sessions %d times", lookups.Load())
	}
	var expiredRows int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, security.HashToken("expired-row")).Scan(&expiredRows); err != nil {
		t.Fatalf("count expired row: %v", err)
	}
	if expiredRows != 1 {
		t.Fatalf("auth request unexpectedly cleaned expired sessions")
	}
	for i := 0; i < 1000; i++ {
		req := httptest.NewRequest(http.MethodGet, "/protected", nil)
		req.AddCookie(&http.Cookie{Name: "sid", Value: "invalid-session"})
		resp, requestErr := app.Test(req)
		if requestErr != nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("invalid-cookie request %d status=%v err=%v", i, responseStatus(resp), requestErr)
		}
		if i == 0 && !strings.Contains(resp.Header.Get("Set-Cookie"), "sid=") {
			t.Fatalf("invalid sid did not clear session cookie")
		}
		resp.Body.Close()
	}
	if lookups.Load() != 1000 {
		t.Fatalf("invalid sid lookup count=%d", lookups.Load())
	}
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, security.HashToken("expired-row")).Scan(&expiredRows); err != nil || expiredRows != 1 {
		t.Fatalf("invalid sid request unexpectedly cleaned sessions: rows=%d err=%v", expiredRows, err)
	}
	logs, err := st.AuditLogs(10000)
	if err != nil {
		t.Fatalf("audit logs: %v", err)
	}
	unauthorized := 0
	for _, entry := range logs {
		if entry.Action == "unauthorized" {
			unauthorized++
			if strings.Contains(entry.Detail, "/protected") || strings.Contains(entry.Detail, "invalid-session") {
				t.Fatalf("sampled auth audit leaked path/session: %+v", entry)
			}
		}
	}
	if unauthorized > 2 {
		t.Fatalf("unauthorized audit was not sampled: %d", unauthorized)
	}
}

func TestUnauthorizedAuditSamplingKeysAndDisabledSemantics(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.Audit.UnauthorizedSampleSeconds = 60
	cfg.Audit.UnauthorizedGlobalPerMinute = 100
	cfg.Audit.PruneEveryWrites = 0
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 1000)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	st.SetAuditPolicy(1000, 0)
	s := &Server{config: cfg, store: st, auditLimiter: newWindowLimiter()}
	s.sampledAudit("unauthorized", "192.0.2.1", "route-a", "denied")
	s.sampledAudit("unauthorized", "192.0.2.1", "route-a", "denied")
	s.sampledAudit("unauthorized", "192.0.2.2", "route-a", "denied")
	s.sampledAudit("unauthorized", "192.0.2.1", "route-b", "denied")
	logs, _ := st.AuditLogs(100)
	if len(logs) != 3 {
		t.Fatalf("expected independent IP/route sample keys, got %d", len(logs))
	}
	cfg.Audit.UnauthorizedSampleSeconds = 0
	cfg.Audit.UnauthorizedGlobalPerMinute = 1
	s.auditLimiter = newWindowLimiter()
	s.sampledAudit("unauthorized", "198.51.100.1", "route-a", "global limit")
	s.sampledAudit("unauthorized", "198.51.100.2", "route-b", "suppressed by unauthorized global")
	s.sampledAudit("csrf_denied", "198.51.100.3", "csrf", "independent csrf global")
	s.sampledAudit("csrf_denied", "198.51.100.4", "csrf", "suppressed by csrf global")
	logs, _ = st.AuditLogs(100)
	if len(logs) != 5 {
		t.Fatalf("per-action global audit limits were not independent: %d", len(logs))
	}
	cfg.Audit.UnauthorizedGlobalPerMinute = 0
	for i := 0; i < 5; i++ {
		s.sampledAudit("unauthorized", "192.0.2.1", "route-a", "disabled sampling")
	}
	logs, _ = st.AuditLogs(100)
	if len(logs) != 10 {
		t.Fatalf("zero sampling/limit should audit every event, got %d", len(logs))
	}
}

func TestLoginFailedAuditIsNotSampled(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.Audit.UnauthorizedSampleSeconds = 3600
	cfg.Audit.UnauthorizedGlobalPerMinute = 1
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 1000)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), rateLimiter: newWindowLimiter(), auditLimiter: newWindowLimiter(), transfers: newTransferRegistry()}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	s.routes(app)
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"code":"111111"}`))
		req.Header.Set("Content-Type", "application/json")
		resp, requestErr := app.Test(req)
		if requestErr != nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("login failure %d status=%v err=%v", i, responseStatus(resp), requestErr)
		}
		resp.Body.Close()
	}
	logs, _ := st.AuditLogs(100)
	count := 0
	for _, entry := range logs {
		if entry.Action == "login_failed" {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("login_failed audit was sampled: %d", count)
	}
}

func TestPolicyAuditIsCompleteAndFailureLogDoesNotLeakDetail(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.Audit.UnauthorizedSampleSeconds = 3600
	cfg.Audit.UnauthorizedGlobalPerMinute = 1
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	s := &Server{config: cfg, store: st, auditLimiter: newWindowLimiter()}
	for i := 0; i < 3; i++ {
		s.criticalAudit("illegal_access", "192.0.2.1", fmt.Sprintf("policy detail %d", i))
	}
	logs, err := st.AuditLogs(10)
	if err != nil || len(logs) != 3 {
		t.Fatalf("policy audit was sampled: len=%d err=%v", len(logs), err)
	}
	if err := st.DB.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	output, restoreLog := captureTestLog(t)
	criticalActions := []string{"login_failed", "login_success", "config_resource_create", "config_resource_update", "config_resource_delete", "config_upload_policy_update", "token_create", "token_revoke", "token_delete", "illegal_access", "file_picker_denied", "download_lease_create", "public_download_lease_create", "download_lease_resource_changed", "download_lease_file_changed", "upload_lease_create", "upload_lease_resource_changed", "upload_lease_failed", "token_upload_failed", "token_upload_denied", "token_denied"}
	for _, action := range criticalActions {
		s.criticalAudit(action, "sensitive-ip", "secret-path-and-token")
	}
	s.bestEffortAudit("download", "sensitive-ip", "ordinary-download-secret")
	restoreLog()
	if strings.Count(output.String(), "[CRITICAL]") != len(criticalActions) || strings.Contains(output.String(), "secret-path-and-token") || strings.Contains(output.String(), "ordinary-download-secret") || strings.Contains(output.String(), "sensitive-ip") {
		t.Fatalf("critical audit failure log missing or leaked detail: %q", output.String())
	}
	for _, action := range criticalActions {
		if strings.Count(output.String(), "action="+action) != 1 {
			t.Fatalf("critical action %s did not produce exactly one diagnostic: %q", action, output.String())
		}
	}
	if strings.Contains(output.String(), "action=download\n") {
		t.Fatalf("ordinary download audit failure was incorrectly escalated: %q", output.String())
	}
}

func TestCriticalAuditCallSiteClassification(t *testing.T) {
	var source strings.Builder
	for _, name := range []string{"server.go", "file_picker.go", "download_safety.go"} {
		content, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		source.Write(content)
	}
	criticalActions := []string{"login_failed", "login_success", "config_resource_create", "config_resource_update", "config_resource_delete", "config_upload_policy_update", "token_create", "token_revoke", "token_delete", "illegal_access", "file_picker_denied", "download_lease_create", "public_download_lease_create", "download_lease_resource_changed", "download_lease_file_changed", "upload_lease_create", "upload_lease_resource_changed", "upload_lease_failed", "token_upload_failed", "token_upload_denied", "token_denied"}
	for _, action := range criticalActions {
		if !strings.Contains(source.String(), `criticalAudit("`+action+`"`) {
			t.Fatalf("critical action %s is not routed through criticalAudit", action)
		}
		if strings.Contains(source.String(), `_ = s.store.Audit("`+action+`"`) {
			t.Fatalf("critical action %s still ignores Store.Audit errors", action)
		}
	}
	if !strings.Contains(source.String(), `bestEffortAudit("download"`) {
		t.Fatalf("ordinary download success should remain best-effort")
	}
}

func TestDirectoryAndPickerBoundedPaginationContract(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "directory-a"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for i := 0; i < 105; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file-%03d.txt", i)), []byte("x"), 0600); err != nil {
			t.Fatalf("write entry: %v", err)
		}
	}
	cfg := testConfig(root)
	cfg.Storage.DirectoryListScanLimit = 100
	cfg.Storage.DirectoryListMaxPageSize = 20
	cfg.FilePicker.MaxScanEntries = 100
	cfg.FilePicker.MaxPageSize = 20
	cfg.FilePicker.Roots = []config.FilePickerRoot{{ID: "bounded", Name: "Bounded", Path: root, AllowSelectFiles: true, AllowSelectDirs: true}}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	if err := st.CreateSession("bounded-user", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create user session: %v", err)
	}
	if err := st.CreateSession("bounded-admin", time.Now().Add(time.Hour), "admin", ""); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	app := New(cfg, st)

	list := func(path, session string, out any) *http.Response {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: "sid", Value: session})
		resp, requestErr := app.Test(req)
		if requestErr != nil {
			t.Fatalf("GET %s: %v", path, requestErr)
		}
		decodeJSON(t, resp, out)
		return resp
	}
	var first fileListResponse
	resp := list("/api/files/list?dirId=default&page=1&pageSize=20", "bounded-user", &first)
	if resp.StatusCode != http.StatusOK || !first.Truncated || first.TotalKnown || first.Total != nil || first.ScannedEntries != 100 || first.ScanLimit != 100 || len(first.Entries) != 20 || !first.HasMore {
		t.Fatalf("unexpected first bounded file page: status=%d response=%+v", resp.StatusCode, first)
	}
	var listedFile *fsutil.Entry
	for i := range first.Entries {
		if first.Entries[i].Type == "file" {
			listedFile = &first.Entries[i]
			break
		}
	}
	if listedFile == nil || !listedFile.MetadataKnown || !listedFile.Downloadable {
		t.Fatalf("file metadata contract failed: %+v", first.Entries)
	}
	for _, entry := range first.Entries {
		if strings.Contains(entry.Path, root) {
			t.Fatalf("ordinary listing leaked real root: %+v", entry)
		}
	}
	var last fileListResponse
	resp = list("/api/files/list?dirId=default&page=5&pageSize=20", "bounded-user", &last)
	if resp.StatusCode != http.StatusOK || last.HasMore || !last.Truncated {
		t.Fatalf("truncated final scanned page semantics failed: status=%d response=%+v", resp.StatusCode, last)
	}
	var legacy fileListResponse
	resp = list("/api/files/list?dirId=default", "bounded-user", &legacy)
	if resp.StatusCode != http.StatusOK || legacy.Page != 1 || legacy.PageSize != 100 || len(legacy.Entries) != 100 || !legacy.Truncated {
		t.Fatalf("legacy unpaged listing compatibility failed: status=%d response=%+v", resp.StatusCode, legacy)
	}
	for _, query := range []string{"page=6&pageSize=20", "page=9223372036854775807&pageSize=20"} {
		var payload map[string]any
		resp = list("/api/files/list?dirId=default&"+query, "bounded-user", &payload)
		if resp.StatusCode != http.StatusBadRequest || payload["code"] != "directory_page_out_of_range" {
			t.Fatalf("expected stable out-of-range for %s: status=%d payload=%+v", query, resp.StatusCode, payload)
		}
	}

	var picker filePickerListResponse
	resp = list("/api/config/file-picker/list?rootId=bounded&page=1&pageSize=20&sort=name", "bounded-admin", &picker)
	if resp.StatusCode != http.StatusOK || !picker.Truncated || picker.TotalKnown || picker.Total != nil || picker.ScannedEntries != 100 || picker.ScanLimit != 100 || len(picker.Items) != 20 || !picker.HasMore {
		t.Fatalf("unexpected bounded picker response: status=%d response=%+v", resp.StatusCode, picker)
	}
	var pickedFile *filePickerItemDTO
	for i := range picker.Items {
		if picker.Items[i].Type == "file" {
			pickedFile = &picker.Items[i]
			break
		}
	}
	if pickedFile == nil || !pickedFile.MetadataKnown || !pickedFile.Downloadable {
		t.Fatalf("picker file metadata contract failed: %+v", picker.Items)
	}
	var pickerLegacy filePickerListResponse
	resp = list("/api/config/file-picker/list?rootId=bounded", "bounded-admin", &pickerLegacy)
	if resp.StatusCode != http.StatusOK || pickerLegacy.Page != 1 || pickerLegacy.PageSize != 20 || len(pickerLegacy.Items) != 20 {
		t.Fatalf("legacy picker request compatibility failed: status=%d response=%+v", resp.StatusCode, pickerLegacy)
	}
	for _, sortBy := range []string{"name", "type", "size", "modifiedAt"} {
		var sorted filePickerListResponse
		resp = list("/api/config/file-picker/list?rootId=bounded&page=1&pageSize=20&sort="+sortBy+"&order=desc", "bounded-admin", &sorted)
		if resp.StatusCode != http.StatusOK || len(sorted.Items) == 0 {
			t.Fatalf("picker sort %s failed: status=%d items=%+v", sortBy, resp.StatusCode, sorted.Items)
		}
	}
}

func TestDirectoryMetadataFailureBecomesUnknownWithoutFailingRequest(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	cfg.Storage.DirectoryListScanLimit = 100
	cfg.Storage.DirectoryListMaxPageSize = 20
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	if err := st.CreateSession("metadata-user", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	reader := &serverFakeDirectoryReader{entries: []os.DirEntry{serverFakeDirEntry{name: "vanished.txt", infoErr: errors.New("transient stat failure")}}}
	s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry(), openDirectory: func(string) (fsutil.DirectoryReader, error) { return reader, nil }}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	s.routes(app)
	req := httptest.NewRequest(http.MethodGet, "/api/files/list?dirId=default&page=1&pageSize=20", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: "metadata-user"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("list files: %v", err)
	}
	var result fileListResponse
	decodeJSON(t, resp, &result)
	if resp.StatusCode != http.StatusOK || len(result.Entries) != 1 || result.Entries[0].Type != "unknown" || result.Entries[0].MetadataKnown || result.Entries[0].Downloadable {
		t.Fatalf("metadata failure did not produce unknown entry: status=%d result=%+v", resp.StatusCode, result)
	}
	pickerItem, included := s.pickerItem(resolvedPickerPath{root: config.FilePickerRoot{AllowSelectFiles: true}, rootReal: root, absolutePath: root}, serverFakeDirEntry{name: "vanished-picker.txt", infoErr: errors.New("transient")})
	if !included || pickerItem.Type != "unknown" || pickerItem.MetadataKnown || pickerItem.Downloadable || pickerItem.Selectable {
		t.Fatalf("picker metadata failure did not produce safe unknown item: %+v included=%v", pickerItem, included)
	}
	if _, included := s.pickerItem(resolvedPickerPath{root: config.FilePickerRoot{AllowSelectFiles: true, ShowHidden: true}, rootReal: root, absolutePath: root}, serverFakeDirEntry{name: ".upload-secret.tmp"}); included {
		t.Fatalf("picker exposed upload staging temp despite showHidden")
	}
	tempEntries := make([]os.DirEntry, 0, 6)
	for i := 0; i < 5; i++ {
		tempEntries = append(tempEntries, serverFakeDirEntry{name: fmt.Sprintf(".upload-%d.tmp", i)})
	}
	tempEntries = append(tempEntries, serverFakeDirEntry{name: "visible-after-window.txt"})
	s.openDirectory = func(string) (fsutil.DirectoryReader, error) {
		return &serverFakeDirectoryReader{entries: tempEntries}, nil
	}
	pickerResult, err := s.readPickerDir(resolvedPickerPath{root: config.FilePickerRoot{AllowSelectFiles: true, ShowHidden: true}, rootReal: root, absolutePath: root}, 1, 5, "name", "asc", 5)
	if err != nil {
		t.Fatalf("picker temp scan: %v", err)
	}
	if !pickerResult.Truncated || pickerResult.TotalKnown || pickerResult.Total != nil || pickerResult.HasMore || pickerResult.ScannedEntries != 5 || len(pickerResult.Items) != 0 {
		t.Fatalf("picker temp entries did not consume raw scan budget: %+v", pickerResult)
	}
}

func TestListingPageSizeClampsBeforeOffsetForFilesAndPicker(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 25; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("item-%02d.txt", i)), []byte("x"), 0600); err != nil {
			t.Fatalf("write item: %v", err)
		}
	}
	cfg := testConfig(root)
	cfg.Storage.DirectoryListScanLimit = 100
	cfg.Storage.DirectoryListMaxPageSize = 10
	cfg.FilePicker.MaxScanEntries = 100
	cfg.FilePicker.MaxPageSize = 10
	cfg.FilePicker.Roots = []config.FilePickerRoot{{ID: "clamped", Path: root, AllowSelectFiles: true, AllowSelectDirs: true}}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	if err := st.CreateSession("clamp-user", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create user session: %v", err)
	}
	if err := st.CreateSession("clamp-admin", time.Now().Add(time.Hour), "admin", ""); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	app := New(cfg, st)
	filePage := func(page, pageSize string) fileListResponse {
		req := httptest.NewRequest(http.MethodGet, "/api/files/list?dirId=default&page="+page+"&pageSize="+pageSize, nil)
		req.AddCookie(&http.Cookie{Name: "sid", Value: "clamp-user"})
		resp, requestErr := app.Test(req)
		if requestErr != nil {
			t.Fatalf("file page: %v", requestErr)
		}
		var result fileListResponse
		decodeJSON(t, resp, &result)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("file page status=%d result=%+v", resp.StatusCode, result)
		}
		return result
	}
	pickerPage := func(page, pageSize string) filePickerListResponse {
		req := httptest.NewRequest(http.MethodGet, "/api/config/file-picker/list?rootId=clamped&page="+page+"&pageSize="+pageSize, nil)
		req.AddCookie(&http.Cookie{Name: "sid", Value: "clamp-admin"})
		resp, requestErr := app.Test(req)
		if requestErr != nil {
			t.Fatalf("picker page: %v", requestErr)
		}
		var result filePickerListResponse
		decodeJSON(t, resp, &result)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("picker page status=%d result=%+v", resp.StatusCode, result)
		}
		return result
	}
	files1, files2 := filePage("1", "100"), filePage("2", "100")
	picker1, picker2 := pickerPage("1", "100"), pickerPage("2", "100")
	if files1.PageSize != 10 || files2.PageSize != 10 || len(files1.Entries) != 10 || len(files2.Entries) != 10 || files1.Entries[0].Name != "item-00.txt" || files2.Entries[0].Name != "item-10.txt" {
		t.Fatalf("file clamp/offset failed: page1=%+v page2=%+v", files1, files2)
	}
	if picker1.PageSize != 10 || picker2.PageSize != 10 || len(picker1.Items) != 10 || len(picker2.Items) != 10 || picker1.Items[0].Name != "item-00.txt" || picker2.Items[0].Name != "item-10.txt" {
		t.Fatalf("picker clamp/offset failed: page1=%+v page2=%+v", picker1, picker2)
	}
	seen := map[string]bool{}
	for _, entry := range files1.Entries {
		seen[entry.Name] = true
	}
	for _, entry := range files2.Entries {
		if seen[entry.Name] {
			t.Fatalf("clamped pages overlap at %q", entry.Name)
		}
	}
	if huge := filePage("2", "9223372036854775807"); huge.PageSize != 10 || huge.Entries[0].Name != "item-10.txt" {
		t.Fatalf("huge positive pageSize did not safely clamp: %+v", huge)
	}
	if huge := pickerPage("2", "9223372036854775807"); huge.PageSize != 10 || huge.Items[0].Name != "item-10.txt" {
		t.Fatalf("huge picker pageSize did not safely clamp: %+v", huge)
	}
	for _, query := range []string{"page=0&pageSize=100", "page=1&pageSize=0", "page=9223372036854775807&pageSize=9223372036854775807"} {
		req := httptest.NewRequest(http.MethodGet, "/api/files/list?dirId=default&"+query, nil)
		req.AddCookie(&http.Cookie{Name: "sid", Value: "clamp-user"})
		resp, requestErr := app.Test(req)
		if requestErr != nil {
			t.Fatalf("invalid page request: %v", requestErr)
		}
		var payload map[string]any
		decodeJSON(t, resp, &payload)
		if resp.StatusCode != http.StatusBadRequest || payload["code"] != "directory_page_out_of_range" {
			t.Fatalf("invalid pagination %q status=%d payload=%+v", query, resp.StatusCode, payload)
		}
	}
	legacyReq := httptest.NewRequest(http.MethodGet, "/api/files/list?dirId=default", nil)
	legacyReq.AddCookie(&http.Cookie{Name: "sid", Value: "clamp-user"})
	legacyResp, err := app.Test(legacyReq)
	if err != nil {
		t.Fatalf("legacy list: %v", err)
	}
	var legacy fileListResponse
	decodeJSON(t, legacyResp, &legacy)
	if legacy.PageSize != 100 || len(legacy.Entries) != 25 {
		t.Fatalf("legacy unpaged behavior changed: %+v", legacy)
	}
}

type serverFakeDirectoryReader struct {
	entries []os.DirEntry
}

func (r *serverFakeDirectoryReader) ReadDir(n int) ([]os.DirEntry, error) {
	if len(r.entries) <= n {
		return r.entries, io.EOF
	}
	return r.entries[:n], nil
}
func (r *serverFakeDirectoryReader) Close() error { return nil }

type serverFakeDirEntry struct {
	name    string
	mode    os.FileMode
	infoErr error
}

func (e serverFakeDirEntry) Name() string      { return e.name }
func (e serverFakeDirEntry) IsDir() bool       { return e.mode.IsDir() }
func (e serverFakeDirEntry) Type() os.FileMode { return e.mode.Type() }
func (e serverFakeDirEntry) Info() (os.FileInfo, error) {
	return serverFakeFileInfo(e), e.infoErr
}

type serverFakeFileInfo serverFakeDirEntry

func (i serverFakeFileInfo) Name() string       { return i.name }
func (i serverFakeFileInfo) Size() int64        { return 0 }
func (i serverFakeFileInfo) Mode() os.FileMode  { return i.mode }
func (i serverFakeFileInfo) ModTime() time.Time { return time.Unix(1700000000, 0) }
func (i serverFakeFileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i serverFakeFileInfo) Sys() any           { return nil }

func assertCapabilityHeaders(t *testing.T, resp *http.Response) {
	t.Helper()
	want := map[string]string{
		"Cache-Control":   "no-store",
		"Pragma":          "no-cache",
		"Referrer-Policy": "no-referrer",
		"X-Robots-Tag":    "noindex, nofollow, noarchive",
	}
	for key, value := range want {
		if got := resp.Header.Get(key); got != value {
			t.Fatalf("missing capability header %s: got=%q want=%q status=%d", key, got, value, resp.StatusCode)
		}
	}
}

func TestUploadGateRejectsReservedButUnregisteredUploadsAfterResourcePublish(t *testing.T) {
	for _, mode := range []string{"raw", "multipart"} {
		t.Run(mode, func(t *testing.T) {
			s, app, st, oldRoot, newRoot := newTransferGateTestServer(t)
			if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
				t.Fatalf("create session: %v", err)
			}
			lease := createTestUploadLease(t, app, mode+"-gate.txt", 5)
			reached := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			s.beforeUploadTransferRegister = func() {
				once.Do(func() { close(reached) })
				<-release
			}
			type result struct {
				resp *http.Response
				err  error
			}
			resultCh := make(chan result, 1)
			go func() {
				var req *http.Request
				if mode == "raw" {
					req = httptest.NewRequest(http.MethodPost, lease.RawUploadURL, strings.NewReader("hello"))
					req.ContentLength = 5
				} else {
					body, contentType := multipartUploadBody(t, "ignored.txt", []byte("hello"))
					req = httptest.NewRequest(http.MethodPost, lease.UploadURL, body)
					req.Header.Set("Content-Type", contentType)
				}
				req.Header.Set("Authorization", "Bearer "+lease.Lease)
				resp, err := app.Test(req, 5000)
				resultCh <- result{resp: resp, err: err}
			}()
			select {
			case <-reached:
			case <-time.After(3 * time.Second):
				t.Fatalf("upload did not reach pre-registration gate")
			}
			for _, item := range s.transfers.list() {
				if item.Type == "upload" && item.Status != transferCompleted {
					t.Fatalf("upload must not be registered before gate admission: %+v", item)
				}
			}
			if err := s.updateConfigResources(func(resources []config.Dir) ([]config.Dir, error) {
				resources[0].Path = newRoot
				return resources, nil
			}); err != nil {
				t.Fatalf("publish changed resource: %v", err)
			}
			close(release)
			res := <-resultCh
			if res.err != nil {
				t.Fatalf("upload request: %v", res.err)
			}
			res.resp.Body.Close()
			if res.resp.StatusCode != http.StatusForbidden {
				t.Fatalf("expected old fingerprint upload rejected, got %d", res.resp.StatusCode)
			}
			for _, root := range []string{oldRoot, newRoot} {
				if _, err := os.Stat(filepath.Join(root, mode+"-gate.txt")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("expected no final file in %s, stat=%v", root, err)
				}
				assertNoUploadTempFiles(t, root)
			}
		})
	}
}

func newTransferGateTestServer(t *testing.T) (*Server, *fiber.App, *store.Store, string, string) {
	t.Helper()
	oldRoot := t.TempDir()
	newRoot := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.DB.Close() })
	cfg := testConfig(oldRoot)
	cfg.Auth.DevAllowFixedCode = true
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	s := &Server{config: cfg, configPath: cfgPath, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry()}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler, StreamRequestBody: true})
	s.routes(app)
	return s, app, st, oldRoot, newRoot
}

func newResourcePolicyTestApp(t *testing.T, roots []config.FilePickerRoot, resources []config.Dir, staticDir string) (*fiber.App, []string) {
	t.Helper()
	metaDir := t.TempDir()
	configPath := filepath.Join(metaDir, "service.yaml")
	databasePath := filepath.Join(metaDir, "protected.db")
	for _, path := range []string{databasePath, databasePath + "-wal", databasePath + "-shm", databasePath + "-journal"} {
		if err := osWriteFile(path, []byte("protected")); err != nil {
			t.Fatalf("write protected database path: %v", err)
		}
	}
	cfg := config.Default()
	cfg.Auth.TOTPSecret = "JBSWY3DPEHPK3PXP"
	cfg.Auth.DevAllowFixedCode = false
	cfg.Auth.Admin.Username = "admin"
	setTestAdminPassword(cfg)
	cfg.Database.Path = databasePath
	cfg.Web.StaticDir = staticDir
	cfg.FilePicker.Roots = roots
	cfg.SetResources(resources)
	if err := config.SaveAtomic(configPath, cfg); err != nil {
		t.Fatalf("save resource policy config: %v", err)
	}
	if err := osWriteFile(configPath+".bak", []byte("backup")); err != nil {
		t.Fatalf("write protected backup: %v", err)
	}
	configCandidate := filepath.Join(filepath.Dir(configPath), ".config-oracle.yaml.tmp")
	if err := osWriteFile(configCandidate, []byte("candidate")); err != nil {
		t.Fatalf("write protected config candidate: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "runtime.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.DB.Close() })
	app := NewWithConfigPath(cfg, st, configPath)
	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	protected := []string{configPath, configPath + ".bak", configCandidate, databasePath, databasePath + "-wal", databasePath + "-shm", databasePath + "-journal"}
	if executable, err := os.Executable(); err == nil {
		protected = append(protected, executable)
	}
	return app, protected
}

func requestResourceChange(t *testing.T, app *fiber.App, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("resource request: %v", err)
	}
	var payload map[string]any
	decodeJSON(t, resp, &payload)
	return resp.StatusCode, payload
}

func assertResourcePolicyError(t *testing.T, status int, payload map[string]any, code, sensitivePath string) {
	t.Helper()
	if status != http.StatusForbidden || payload["code"] != code {
		t.Fatalf("expected policy error %s, status=%d payload=%+v", code, status, payload)
	}
	if sensitivePath != "" && strings.Contains(fmt.Sprint(payload), sensitivePath) {
		t.Fatalf("policy response leaked sensitive path: %+v", payload)
	}
}

func assertNoUploadTempFiles(t *testing.T, root string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("read upload root: %v", err)
		}
		remaining := ""
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".upload-") && strings.HasSuffix(entry.Name(), ".tmp") {
				remaining = entry.Name()
				break
			}
		}
		if remaining == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected successful upload to remove temp file, found %s", remaining)
		}
		time.Sleep(time.Millisecond)
	}
}

func multipartUploadBody(t *testing.T, fileName string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	if err := writer.WriteField("dirId", "default"); err != nil {
		t.Fatalf("write dir field: %v", err)
	}
	part, err := writer.CreateFormFile("files", fileName)
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write file content: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return body, writer.FormDataContentType()
}

func createTestUploadLease(t *testing.T, app *fiber.App, fileName string, fileSize int64) uploadLeaseResponse {
	return createTestUploadLeaseForSession(t, app, "user-sid", fileName, fileSize)
}

func createTestUploadLeaseForSession(t *testing.T, app *fiber.App, sessionID, fileName string, fileSize int64) uploadLeaseResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/files/upload-lease", strings.NewReader(fmt.Sprintf(`{"dirId":"default","path":"","fileName":%q,"fileSize":%d}`, fileName, fileSize)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "sid", Value: sessionID})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("create upload lease: %v", err)
	}
	var lease uploadLeaseResponse
	decodeJSON(t, resp, &lease)
	if resp.StatusCode != http.StatusOK || lease.Lease == "" {
		t.Fatalf("expected upload lease, status=%d lease=%+v", resp.StatusCode, lease)
	}
	return lease
}

func waitForSlowUploadTransfer(t *testing.T, client *http.Client, baseURL string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last []transferRecord
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, baseURL+"/api/transfers/active", nil)
		if err != nil {
			t.Fatalf("new active transfers request: %v", err)
		}
		req.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("active transfers request: %v", err)
		}
		var payload struct {
			Transfers []transferRecord `json:"transfers"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			_ = resp.Body.Close()
			t.Fatalf("decode active transfers: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected active transfers ok, got %d", resp.StatusCode)
		}
		last = payload.Transfers
		for _, item := range payload.Transfers {
			if item.Type == "upload" && item.FileName == "slow-raw.bin" && item.Cancelable && item.TotalBytes > 0 && item.TransferredBytes > 0 && item.TransferredBytes < item.TotalBytes {
				return item.ID
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected slow raw upload to be visible and cancelable during transfer, last=%+v", last)
	return ""
}

func waitForRegisteredUpload(t *testing.T, s *Server, fileName string) transferRecord {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, item := range s.transfers.list() {
			if item.Type == "upload" && item.FileName == fileName && item.Status == transferActive {
				return item
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected active upload %q", fileName)
	return transferRecord{}
}

func testConfig(root string) *config.Config {
	cfg := config.Default()
	cfg.Auth.TOTPSecret = "JBSWY3DPEHPK3PXP"
	cfg.Auth.DevAllowFixedCode = false
	cfg.Auth.Admin.Username = "admin"
	setTestAdminPassword(cfg)
	setTestStorageAndPickerRoot(cfg, root)
	return cfg
}

func setTestStorageAndPickerRoot(cfg *config.Config, root string) {
	cfg.Storage.Dirs = []config.Dir{{ID: "default", Name: "Default", Path: root, AllowDownload: true, AllowUpload: true}}
	cfg.FilePicker.Roots = []config.FilePickerRoot{{ID: "test-root", Name: "Test Root", Path: root, AllowSelectFiles: true, AllowSelectDirs: true, FollowSymlinks: true}}
}

var (
	testAdminPasswordOnce sync.Once
	testAdminPasswordPHC  string
)

func setTestAdminPassword(cfg *config.Config) {
	testAdminPasswordOnce.Do(func() {
		var err error
		testAdminPasswordPHC, err = security.Hash([]byte("secret"))
		if err != nil {
			panic("hash test admin password: " + err.Error())
		}
	})
	cfg.Auth.Admin.PasswordHash = testAdminPasswordPHC
	cfg.Auth.Admin.PasswordSHA256 = ""
}

func captureTestLog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	var output bytes.Buffer
	previous := log.Writer()
	var once sync.Once
	restore := func() {
		once.Do(func() { log.SetOutput(previous) })
	}
	log.SetOutput(&output)
	t.Cleanup(restore)
	return &output, restore
}

func discardTestLog(t *testing.T) func() {
	t.Helper()
	previous := log.Writer()
	var once sync.Once
	restore := func() {
		once.Do(func() { log.SetOutput(previous) })
	}
	log.SetOutput(io.Discard)
	t.Cleanup(restore)
	return restore
}

func testResourceFingerprint(t *testing.T, cfg *config.Config, id string) string {
	t.Helper()
	dir, ok := cfg.Dir(id)
	if !ok {
		t.Fatalf("missing test resource %q", id)
	}
	return resourceAuthorizationFingerprint(dir)
}

func decodeJSON(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode json: %v", err)
	}
}

func assertPartialBody(t *testing.T, resp *http.Response, expected string) {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("expected 206 partial content, got %d body=%q", resp.StatusCode, string(body))
	}
	if string(body) != expected {
		t.Fatalf("expected body %q, got %q", expected, string(body))
	}
}

func assertErrorContains(t *testing.T, resp *http.Response, status int, text string) {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read error body: %v", err)
	}
	if resp.StatusCode != status {
		t.Fatalf("expected status %d, got %d body=%q", status, resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), text) {
		t.Fatalf("expected body to contain %q, got %q", text, string(body))
	}
}

func osWriteFile(path string, content []byte) error {
	return os.WriteFile(path, content, 0644)
}

func leaseHashFromURL(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse lease url: %v", err)
	}
	plain := parsed.Query().Get("lease")
	if plain == "" {
		t.Fatalf("lease url missing token: %q", raw)
	}
	return security.HashToken(plain)
}

func sqlNullTime(t time.Time) sql.NullTime {
	return sql.NullTime{Time: t, Valid: true}
}
