package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"filetrans-backend/internal/config"
	"filetrans-backend/internal/store"

	"github.com/pquerna/otp/totp"
)

func TestAdminOnlyTokenRoutes(t *testing.T) {
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

func TestValidateLoginCodeAcceptsAdjacentTOTPWindow(t *testing.T) {
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

func sqlNullTime(t time.Time) sql.NullTime {
	return sql.NullTime{Time: t, Valid: true}
}
