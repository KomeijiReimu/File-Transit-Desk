package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filetrans-backend/internal/config"
	"filetrans-backend/internal/security"
	"filetrans-backend/internal/store"
	"github.com/gofiber/fiber/v2"
)

func TestAccountAPIApprovalStablePrincipalAndRevocation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("hello-world"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(root)
	cfg.FilePicker.Mode = "system"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	if err := config.MigrateConfig(path); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatal(err)
	}
	defer st.DB.Close()
	app := NewWithConfigPath(cfg, st, path)
	if err := st.CreateSession("admin", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatal(err)
	}
	var cookie *http.Cookie
	call := func(method, url, body, sid string, want int) map[string]json.RawMessage {
		t.Helper()
		req := httptest.NewRequest(method, url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if sid != "" {
			req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
		}
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]json.RawMessage
		if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
			decodeJSON(t, resp, &out)
		} else {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
		if resp.StatusCode != want {
			t.Fatalf("%s %s: status %d want %d: %s", method, url, resp.StatusCode, want, out)
		}
		if strings.Contains(url, "account-login") && want == 200 {
			cookie = responseCookie(t, resp, "sid")
		}
		return out
	}
	call("GET", "/api/auth/options", "", "", 200)
	call("POST", "/api/auth/account-login", `{"username":"missing-user","password":"non-authorizing-account-dummy-password"}`, "", 401)
	call("POST", "/api/auth/register", `{"username":"alice","password":"initial-password","displayName":"Alice"}`, "", 201)
	call("POST", "/api/auth/account-login", `{"username":"alice","password":"wrong-password"}`, "", 401)
	call("POST", "/api/auth/account-login", `{"username":"alice","password":"initial-password"}`, "", 403)
	u, err := st.AccountByUsername("alice")
	if err != nil {
		t.Fatal(err)
	}
	userURL := fmt.Sprintf("/api/admin/users/%d", u.ID)
	call("PUT", userURL, `{"status":"active"}`, "admin", 200)
	first := call("POST", "/api/auth/account-login", `{"username":"alice","password":"initial-password"}`, "", 200)
	firstCookie := cookie.Value
	call("POST", "/api/chat/messages", `{"body":"account-owned message"}`, firstCookie, 201)
	second := call("POST", "/api/auth/account-login", `{"username":"ALICE","password":"initial-password"}`, "", 200)
	secondCookie := cookie.Value
	if string(first["userId"]) != string(second["userId"]) || string(first["sessionBinding"]) == string(second["sessionBinding"]) {
		t.Fatal("user ID and per-session binding were conflated")
	}
	var author string
	if err := st.DB.QueryRow(`SELECT author_key FROM chat_messages ORDER BY id DESC LIMIT 1`).Scan(&author); err != nil {
		t.Fatal(err)
	}
	if author != fmt.Sprintf("account:%d", u.ID) {
		t.Fatalf("chat principal %q", author)
	}
	leaseOut := call("POST", "/api/files/download-lease", `{"dirId":"default","path":"file.txt"}`, firstCookie, 200)
	var leaseURL string
	if err := json.Unmarshal(leaseOut["url"], &leaseURL); err != nil {
		t.Fatal(err)
	}
	up := createTestUploadLeaseForSession(t, app, firstCookie, "next.txt", 4)
	call("GET", leaseURL, "", "", 200)
	if err := st.DeleteSession(firstCookie); err != nil {
		t.Fatal(err)
	}
	call("PUT", userURL, `{"status":"disabled"}`, "admin", 200)
	call("GET", "/api/auth/me", "", firstCookie, 401)
	call("GET", "/api/auth/me", "", secondCookie, 401)
	call("GET", leaseURL, "", "", 401)
	if _, err := st.UploadLeaseByHash(security.HashToken(up.Lease)); err == nil {
		t.Fatal("unclaimed upload survived account disable")
	}
	call("PUT", userURL, `{"status":"active"}`, "admin", 200)
	call("POST", "/api/auth/account-login", `{"username":"alice","password":"initial-password"}`, "", 200)
	wrongPassword := call("POST", "/api/auth/change-password", `{"currentPassword":"wrong-password","newPassword":"next-password"}`, cookie.Value, 400)
	if string(wrongPassword["code"]) != `"current_password_incorrect"` {
		t.Fatalf("wrong current password code: %s", wrongPassword)
	}
	call("GET", "/api/auth/me", "", cookie.Value, 200)
	call("POST", "/api/auth/change-password", `{"currentPassword":"initial-password","newPassword":"next-password"}`, cookie.Value, 200)
	call("GET", "/api/auth/me", "", cookie.Value, 401)
	call("POST", "/api/auth/account-login", `{"username":"alice","password":"next-password"}`, "", 200)
	call("POST", userURL+"/reset-password", `{"password":"reset-password"}`, "admin", 200)
	call("GET", "/api/auth/me", "", cookie.Value, 401)
	call("POST", "/api/admin/users", `{"username":"bob","password":"another-password"}`, "admin", 201)
	call("GET", "/api/admin/users?query=alice&status=active&page=1&pageSize=10", "", "admin", 200)
	call("PUT", "/api/config/accounts", `{"registrationEnabled":false,"registrationApprovalRequired":true,"totpEnabled":false}`, "admin", 200)
	outside := t.TempDir()
	call("POST", "/api/config/resources", fmt.Sprintf(`{"id":"system-resource","type":"directory","path":%q,"allowDownload":true,"allowUpload":false}`, outside), "admin", 200)
	call("PUT", "/api/config/resources/system-resource", fmt.Sprintf(`{"type":"directory","path":%q,"allowDownload":true,"allowUpload":true}`, outside), "admin", 200)
	call("POST", "/api/auth/register", `{"username":"carol","password":"another-password"}`, "", 403)
	call("DELETE", userURL, "", "admin", 200)
	deleted, err := st.Account(u.ID)
	if err != nil || deleted.Status != "deleted" || deleted.PasswordHash != "" || deleted.Username != u.Username {
		t.Fatalf("deleted account retained credentials or lost identity: %+v, %v", deleted, err)
	}
	call("POST", "/api/auth/account-login", `{"username":"alice","password":"reset-password"}`, "", 401)
	call("POST", "/api/auth/account-login", `{"username":"alice","password":"non-authorizing-account-dummy-password"}`, "", 401)
	if err := st.DB.Close(); err != nil {
		t.Fatal(err)
	}
	call("POST", "/api/auth/account-login", `{"username":"missing-user","password":"wrong-password"}`, "", 500)
}

func TestAccountRevocationDuringDownloadValidationAndUploadAdmission(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(root)
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatal(err)
	}
	defer st.DB.Close()
	setTestAdminPassword(cfg)
	u, err := st.CreateAccount("alice", "Alice", cfg.Auth.Admin.PasswordHash, "active")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAccountSession("account", time.Now().Add(time.Hour), time.Now().Add(time.Hour), u); err != nil {
		t.Fatal(err)
	}
	s := &Server{config: cfg, store: st, transfers: newTransferRegistry(), loginLimiter: newLoginLimiter(), downloadHashSlots: make(chan struct{}, 2), downloadHashFlights: make(map[string]*downloadHashFlight)}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	s.routes(app)
	req := httptest.NewRequest("POST", "/api/files/download-lease", strings.NewReader(`{"dirId":"default","path":"file.txt"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "sid", Value: "account"})
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	var dl downloadLeaseResponse
	decodeJSON(t, resp, &dl)
	if resp.StatusCode != 200 {
		t.Fatal("create download lease failed")
	}
	upload := createTestUploadLeaseForSession(t, app, "account", "upload.txt", 4)
	claimed := createTestUploadLeaseForSession(t, app, "account", "claimed.txt", 4)
	if _, err := st.ReserveUploadLease(security.HashToken(claimed.Lease), time.Now()); err != nil {
		t.Fatal(err)
	}
	used, err := st.DownloadLeaseByHash(leaseHashFromURL(t, dl.URL))
	if err != nil {
		t.Fatal(err)
	}
	used.Hash = security.HashToken("already-used")
	if err := st.CreateDownloadLease(&used); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkDownloadLeaseFirstUsed(used.Hash, time.Now()); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	s.duringDownloadFileHash = func() { close(started); <-release }
	statuses := make(chan int, 2)
	request := func() {
		resp, err := app.Test(httptest.NewRequest("GET", dl.URL, nil), -1)
		if err != nil {
			statuses <- -1
			return
		}
		resp.Body.Close()
		statuses <- resp.StatusCode
	}
	go request()
	<-started
	go request()
	waitForDownloadHashWaiters(t, s, leaseHashFromURL(t, dl.URL), 1)
	s.beforeDownloadFinalValidation = func() {
		status := "disabled"
		if _, err := st.UpdateAccount(u.ID, nil, &status, "", nil); err != nil {
			t.Error(err)
		}
	}
	rangeReq := httptest.NewRequest("GET", "/api/files/download-by-lease?lease=already-used", nil)
	rangeReq.Header.Set("Range", "bytes=0-1")
	resp, err = app.Test(rangeReq, -1)
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("revoked used Range admitted after validation: %d", resp.StatusCode)
	}
	for i := 0; i < 2; i++ {
		if status := <-statuses; status != 401 {
			t.Fatalf("revoked first-hash/waiter admitted: %d", status)
		}
	}
	if _, err := st.UploadLeaseByHash(security.HashToken(upload.Lease)); err == nil {
		t.Fatal("unclaimed upload not revoked")
	}
	if _, err := st.UploadLeaseByHash(security.HashToken(claimed.Lease)); err != nil {
		t.Fatalf("claimed upload must remain finishable: %v", err)
	}
	if err := st.CreateAccountSession("stale", time.Now().Add(time.Hour), time.Now().Add(time.Hour), u); err == nil {
		t.Fatal("stale authentication issued a session")
	}
	s.adminVerifySlots = make(chan struct{}, 1)
	s.adminVerifySlots <- struct{}{}
	for _, tc := range []struct{ endpoint, username string }{
		{"/api/auth/register", "other-user"},
		{"/api/auth/admin-login", "admin"},
		{"/api/auth/account-login", "missing-user"},
		{"/api/auth/account-login", "alice"},
	} {
		req := httptest.NewRequest("POST", tc.endpoint, strings.NewReader(fmt.Sprintf(`{"username":%q,"password":"password-123"}`, tc.username)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 503 {
			t.Fatalf("shared password pool %s (%s): %d", tc.endpoint, tc.username, resp.StatusCode)
		}
	}
	<-s.adminVerifySlots

	// Revocation between middleware admission and issuance must not become a 500.
	creationApp := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	creationApp.Get("/download", func(c *fiber.Ctx) error {
		return s.creationStoreError(c, st.CreateDownloadLeaseLimited(&store.DownloadLease{Source: "session", SessionID: used.SessionID}, 0, 0))
	})
	creationApp.Get("/upload", func(c *fiber.Ctx) error {
		return s.creationStoreError(c, st.CreateUploadLeaseLimited(&store.UploadLease{Source: "session", SessionID: used.SessionID.String}, 0, 0))
	})
	creationApp.Get("/sql", func(c *fiber.Ctx) error { return s.creationStoreError(c, sql.ErrNoRows) })
	checkCreation := func(path string, want int) {
		t.Helper()
		resp, err := creationApp.Test(httptest.NewRequest("GET", path, nil), -1)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		decodeJSON(t, resp, &out)
		if resp.StatusCode != want || want == 401 && out["code"] != "session_invalid" {
			t.Fatalf("creation %s: status=%d body=%v", path, resp.StatusCode, out)
		}
	}
	checkCreation("/download", 401)
	checkCreation("/upload", 401)
	checkCreation("/sql", 500)
	if err := st.DB.Close(); err != nil {
		t.Fatal(err)
	}
	checkCreation("/download", 500)
	checkCreation("/upload", 500)
}

func TestRestrictedPickerResolveCanonicalRoots(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "MiXeD-Suffix")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"relative", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			rootPath := relative
			if kind == "symlink" {
				rootPath = filepath.Join(t.TempDir(), "root-link")
				if err := os.Symlink(root, rootPath); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			cfg := testConfig(root)
			cfg.FilePicker.Mode = "restricted"
			cfg.FilePicker.Roots = []config.FilePickerRoot{{ID: "canonical", Path: rootPath}}
			s := &Server{config: cfg}
			app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
			app.Post("/resolve", s.filePickerResolve)
			for _, tc := range []struct {
				path string
				want int
			}{{child, 200}, {t.TempDir(), 403}} {
				req := httptest.NewRequest("POST", "/resolve", strings.NewReader(fmt.Sprintf(`{"path":%q}`, tc.path)))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req, -1)
				if err != nil {
					t.Fatal(err)
				}
				var out map[string]any
				decodeJSON(t, resp, &out)
				if resp.StatusCode != tc.want || tc.want == 200 && out["path"] != "/MiXeD-Suffix" {
					t.Fatalf("resolve %s: status=%d body=%v", kind, resp.StatusCode, out)
				}
			}
		})
	}
}
