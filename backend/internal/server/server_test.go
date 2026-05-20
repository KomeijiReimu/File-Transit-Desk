package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"filetrans-backend/internal/config"
	"filetrans-backend/internal/security"
	"filetrans-backend/internal/store"

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
	cfg.Auth.Admin.Username = "admin"
	cfg.Auth.Admin.PasswordSHA256 = "2bb80d537b1da3e38bd30361aa855686bde0ba34388b29d94bb536a73f23c8db"
	cfg.Tokens.UploadMaxMB = 1
	cfg.Storage.Dirs = []config.Dir{{ID: "default", Name: "Default", Path: t.TempDir(), AllowDownload: true, AllowUpload: true}}
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
	cfg.Auth.Admin.PasswordSHA256 = "2bb80d537b1da3e38bd30361aa855686bde0ba34388b29d94bb536a73f23c8db"
	cfg.Storage.Dirs = []config.Dir{{ID: "default", Name: "Default", Path: root, AllowDownload: true, AllowUpload: true}}
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

func TestTokenListReturnsValidityAndDeleteAudit(t *testing.T) {
	// 令牌列表需要返回失效原因，删除操作也必须留下审计记录。
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()

	cfg := config.Default()
	cfg.Auth.Admin.Username = "admin"
	cfg.Auth.Admin.PasswordSHA256 = "2bb80d537b1da3e38bd30361aa855686bde0ba34388b29d94bb536a73f23c8db"
	cfg.Tokens.UploadMaxMB = 1
	cfg.Storage.Dirs = []config.Dir{{ID: "default", Name: "Default", Path: t.TempDir(), AllowDownload: true, AllowUpload: true}}
	app := New(cfg, st)
	if err := st.CreateSession("admin-sid", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	expiredAt := time.Now().Add(-time.Minute)
	tok := &store.Token{Hash: "expired", Type: "download", DirID: "default", Path: "a.txt", MaxUses: 1, ExpiresAt: sqlNullTime(expiredAt)}
	if err := st.CreateToken(tok); err != nil {
		t.Fatalf("create token: %v", err)
	}
	quotaTok := &store.Token{Hash: "quota", Type: "upload", DirID: "default", Path: "", MaxUses: 0}
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

func TestUploadPolicyRejectsBlockedExtension(t *testing.T) {
	// 上传扩展名策略必须在文件落盘前拒绝危险类型。
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()

	cfg := config.Default()
	cfg.Auth.Admin.Username = "admin"
	cfg.Auth.Admin.PasswordSHA256 = "2bb80d537b1da3e38bd30361aa855686bde0ba34388b29d94bb536a73f23c8db"
	cfg.Storage.BlockedExtensions = []string{".exe"}
	cfg.Storage.Dirs = []config.Dir{{ID: "default", Name: "Default", Path: t.TempDir(), AllowDownload: true, AllowUpload: true}}
	app := New(cfg, st)

	if err := st.CreateSession("user-sid", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create user session: %v", err)
	}
	body, contentType := multipartUploadBody(t, "bad.exe", []byte("x"))
	req := httptest.NewRequest(http.MethodPost, "/api/files/upload", body)
	req.Header.Set("Content-Type", contentType)
	req.AddCookie(&http.Cookie{Name: "sid", Value: "user-sid"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("upload request: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected blocked extension to be forbidden, got %d", resp.StatusCode)
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

	tok := &store.Token{Hash: security.HashToken("large-token"), Type: "download", DirID: "default", Path: "large.bin", MaxUses: 1, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))}
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
	tok := &store.Token{Hash: security.HashToken("public-token"), Type: "download", DirID: "default", Path: "public.txt", MaxUses: 1, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))}
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

	deleteTok := &store.Token{Hash: security.HashToken("delete-token"), Type: "download", DirID: "default", Path: "public.txt", MaxUses: 0, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))}
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

func TestValidateLoginCodeAcceptsAdjacentTOTPWindow(t *testing.T) {
	// TOTP 允许相邻窗口，降低客户端和服务器时间轻微偏移导致的登录失败。
	cfg := config.Default()
	cfg.Auth.TOTPSecret = "JBSWY3DPEHPK3PXP"
	cfg.Auth.Admin.Username = "admin"
	cfg.Auth.Admin.PasswordSHA256 = "2bb80d537b1da3e38bd30361aa855686bde0ba34388b29d94bb536a73f23c8db"
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

func testConfig(root string) *config.Config {
	cfg := config.Default()
	cfg.Auth.Admin.Username = "admin"
	cfg.Auth.Admin.PasswordSHA256 = "2bb80d537b1da3e38bd30361aa855686bde0ba34388b29d94bb536a73f23c8db"
	cfg.Storage.Dirs = []config.Dir{{ID: "default", Name: "Default", Path: root, AllowDownload: true, AllowUpload: true}}
	return cfg
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
