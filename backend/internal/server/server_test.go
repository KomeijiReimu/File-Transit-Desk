package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
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
	cfg := testConfig(filepath.Join(base, "uploads"))
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
	if adminResp.StatusCode != http.StatusOK || strings.Contains(fmt.Sprint(safe), "password_sha256") || strings.Contains(fmt.Sprint(safe), "totp_secret") {
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

	manualToken := &store.Token{Hash: "manual-token", Type: "download", DirID: "manual", Path: "", MaxUses: 10}
	if err := st.CreateToken(manualToken); err != nil {
		t.Fatalf("create manual token: %v", err)
	}
	manualLease := &store.DownloadLease{Hash: "manual-lease", Source: "public_token", TokenID: sql.NullInt64{Int64: manualToken.ID, Valid: true}, DirID: "manual", Path: "", FileSize: 6, FileMtime: time.Now(), FileSHA256: sql.NullString{String: "", Valid: true}, ExpiresAt: time.Now().Add(time.Hour)}
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
	badResp, err := app.Test(badReq)
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

func TestOriginFromIPUsesFrontendPort(t *testing.T) {
	if got := originFromIP("http", net.ParseIP("192.168.124.9"), "5173"); got != "http://192.168.124.9:5173" {
		t.Fatalf("unexpected IPv4 origin: %s", got)
	}
	if got := originFromIP("https", net.ParseIP("2001:db8::1"), "8443"); got != "https://[2001:db8::1]:8443" {
		t.Fatalf("unexpected IPv6 origin: %s", got)
	}
}

func TestDevelopmentFrontendOriginPolicy(t *testing.T) {
	t.Setenv("FILE_TRANS_DEV_FRONTEND_PORT", "5173")
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
		if got := developmentFrontendOrigin(tc.origin); got != tc.want {
			t.Fatalf("developmentFrontendOrigin(%q)=%v, want %v", tc.origin, got, tc.want)
		}
	}
}

func TestDevelopmentFrontendOriginUsesConfiguredPort(t *testing.T) {
	t.Setenv("FILE_TRANS_DEV_FRONTEND_PORT", "5174")
	if !developmentFrontendOrigin("http://192.168.124.9:5174") {
		t.Fatalf("expected configured frontend port 5174 to be allowed")
	}
	if developmentFrontendOrigin("http://192.168.124.9:5173") {
		t.Fatalf("expected default port 5173 to be rejected when configured port is 5174")
	}
}

func TestDevelopmentFrontendPortRejectsInvalidRange(t *testing.T) {
	for _, value := range []string{"0", "65536", "not-a-port"} {
		t.Setenv("FILE_TRANS_DEV_FRONTEND_PORT", value)
		if got := developmentFrontendPort(); got != "5173" {
			t.Fatalf("expected invalid dev frontend port %q to fall back to 5173, got %s", value, got)
		}
	}
}

func TestDevelopmentTransferOriginAllowedForSameHost(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app := New(testConfig(t.TempDir()), st)
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
	t.Setenv("FILE_TRANS_DEV_FRONTEND_PORT", "5174")
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	app := New(testConfig(t.TempDir()), st)
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
	app := New(testConfig(t.TempDir()), st)
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
	cfg := testConfig(t.TempDir())
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

func TestAdminFilePickerProvidesSystemRootWithoutConfiguredRoots(t *testing.T) {
	// 文件选择器默认给管理员提供系统入口；配置 roots 只是常用位置快捷方式。
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(t.TempDir())
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
	if rootsResp.StatusCode != http.StatusOK || len(roots) == 0 {
		t.Fatalf("expected built-in picker roots, status=%d roots=%+v", rootsResp.StatusCode, roots)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/config/file-picker/list?rootId="+url.QueryEscape(roots[0].ID)+"&pageSize=1", nil)
	listReq.AddCookie(&http.Cookie{Name: "sid", Value: "admin-sid"})
	listResp, err := app.Test(listReq)
	if err != nil {
		t.Fatalf("list built-in root: %v", err)
	}
	listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("expected built-in root to be browsable, got %d", listResp.StatusCode)
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
	tok := &store.Token{Hash: security.HashToken("short-token"), Type: "download", DirID: "default", Path: "short.txt", MaxUses: 1, ExpiresAt: sqlNullTime(tokenExpiresAt)}
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

func TestDangerousRootRejectsBroadAndSensitiveSystemPaths(t *testing.T) {
	// 配置管理保存资源时拦截系统顶层目录和常见凭据目录，但不禁止正常业务子目录。
	blocked := []string{"/", "/home", "/var", "/usr/lib", "/home/alice/.ssh", "/srv/app/.kube", `C:\`, `C:\Windows\System32`, `C:\Users`}
	for _, value := range blocked {
		if !isDangerousRoot(value) {
			t.Fatalf("expected %q to be dangerous", value)
		}
	}
	allowed := []string{"/home/alice/share", "/mnt/data", `D:\data`}
	for _, value := range allowed {
		if isDangerousRoot(value) {
			t.Fatalf("expected %q to be allowed", value)
		}
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

func TestUploadLeaseRegistersTransferBeforeBodyUpload(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	s := &Server{config: testConfig(root), store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry()}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
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
	items := s.transfers.list()
	if len(items) != 1 || items[0].Type != "upload" || items[0].Status != transferActive || items[0].FileName != "visible.bin" || items[0].TotalBytes != 7 {
		t.Fatalf("expected lease creation to pre-register visible transfer, got %+v", items)
	}
	if items[0].Cancelable {
		t.Fatalf("pre-upload transfer must not be cancelable before request connection exists")
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
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(oldRoot)
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
	if _, err := os.Stat(oldTmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected old resource temp to be removed, stat=%v", err)
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
	cfg := testConfig(oldRoot)
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
	if _, err := os.Stat(oldTmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected old temp to be removed, stat=%v", err)
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
	tok := &store.Token{Hash: security.HashToken("public-upload"), Type: "upload", DirID: "default", Path: "", MaxUses: 1, ExpiresAt: sqlNullTime(time.Now().Add(time.Hour))}
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

func assertNoUploadTempFiles(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read upload root: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".upload-") && strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("expected successful upload to remove temp file, found %s", entry.Name())
		}
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
