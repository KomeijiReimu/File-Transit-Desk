package server

import (
	"context"
	"database/sql"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"filetrans-backend/internal/security"
	"filetrans-backend/internal/store"

	"github.com/gofiber/fiber/v2"
)

func TestHealthLiveReadyDrainAndClosedStore(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	runtime, err := NewRuntimeWithOptions(testConfig(t.TempDir()), st, "", Options{DevFrontendPort: 5173})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	request := func(path string) *http.Response {
		t.Helper()
		resp, err := runtime.App.Test(httptest.NewRequest(http.MethodGet, path, nil))
		if err != nil {
			t.Fatalf("request %s: %v", path, err)
		}
		return resp
	}
	for _, path := range []string{"/api/health", "/api/health/ready", "/api/health/live"} {
		resp := request(path)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected %s healthy, got %d", path, resp.StatusCode)
		}
		if resp.Header.Get("Cache-Control") != "" {
			t.Fatalf("health endpoint unexpectedly received capability headers")
		}
	}
	runtime.initialized.Store(false)
	resp := request("/api/health/ready")
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("uninitialized runtime should not be ready: %d", resp.StatusCode)
	}
	runtime.initialized.Store(true)
	runtime.BeginDrain()
	for _, path := range []string{"/api/health", "/api/health/ready"} {
		resp = request(path)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("draining runtime should not be ready: path=%s status=%d", path, resp.StatusCode)
		}
	}
	resp = request("/api/health/live")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("live endpoint should stay healthy while draining: %d", resp.StatusCode)
	}
	for _, path := range []string{"/api/auth/me", "/t/secret/info", "/api/files/download-by-lease?lease=secret"} {
		resp = request(path)
		var payload map[string]any
		decodeJSON(t, resp, &payload)
		if resp.StatusCode != http.StatusServiceUnavailable || payload["code"] != "server_draining" || resp.Header.Get("Retry-After") != "5" {
			t.Fatalf("unexpected draining response path=%s status=%d payload=%v retry=%q", path, resp.StatusCode, payload, resp.Header.Get("Retry-After"))
		}
		if strings.HasPrefix(path, "/t/") || strings.Contains(path, "download-by-lease") {
			assertCapabilityHeaders(t, resp)
		}
	}
	runtime.draining.Store(false)
	runtime.cancel()
	runtime.wg.Wait()
	if err := st.DB.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	resp = request("/api/health/ready")
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("closed store should not be ready: %d", resp.StatusCode)
	}
}

func TestShutdownWaitsForActiveHandlerAndClosesStoreLast(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	runtime, err := NewRuntimeWithOptions(testConfig(t.TempDir()), st, "", Options{DevFrontendPort: 5173})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	storeAlive := make(chan bool, 1)
	runtime.App.Get("/test/block", func(c *fiber.Ctx) error {
		close(started)
		<-release
		storeAlive <- st.PingContext(context.Background()) == nil
		return c.SendString("done")
	})
	baseURL, listenErr := startRuntimeListener(t, runtime)
	responseDone := make(chan error, 1)
	go func() {
		resp, err := http.Get(baseURL + "/test/block")
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		responseDone <- err
	}()
	<-started
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- runtime.Shutdown() }()
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown returned before handler completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-responseDone; err != nil {
		t.Fatalf("blocked response: %v", err)
	}
	if !<-storeAlive {
		t.Fatalf("store closed before active handler completed")
	}
	if err := <-shutdownDone; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	<-listenErr
	if err := st.PingContext(context.Background()); err == nil {
		t.Fatalf("store remained open after shutdown")
	}
}

func TestShutdownWaitsForSlowRawUploadAfterSessionDeletion(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg := testConfig(root)
	runtime, err := NewRuntimeWithOptions(cfg, st, "", Options{DevFrontendPort: 5173})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	if err := st.CreateSession("shutdown-upload-session", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	const size = 2 * 1024 * 1024
	plain := "shutdown-upload-lease"
	lease := &store.UploadLease{Hash: security.HashToken(plain), Source: "session", SessionID: "shutdown-upload-session", DirID: "default", Path: "", FileName: "shutdown.bin", FileSize: size, ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.CreateUploadLease(lease); err != nil {
		t.Fatalf("create upload lease: %v", err)
	}
	baseURL, listenErr := startRuntimeListener(t, runtime)
	reader, writer := io.Pipe()
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/files/upload-raw-by-lease", reader)
	req.Header.Set("Authorization", "Bearer "+plain)
	req.ContentLength = size
	responseDone := make(chan *http.Response, 1)
	requestErr := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			requestErr <- err
			return
		}
		responseDone <- resp
	}()
	firstChunk := make([]byte, 64*1024)
	if _, err := writer.Write(firstChunk); err != nil {
		t.Fatalf("write first upload chunk: %v", err)
	}
	permitDeadline := time.Now().Add(3 * time.Second)
	for runtime.server.transfers.uploadPermitCount() == 0 {
		if time.Now().After(permitDeadline) {
			t.Fatalf("raw upload was not admitted")
		}
		time.Sleep(time.Millisecond)
	}
	if err := st.DeleteSession("shutdown-upload-session"); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- runtime.Shutdown() }()
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown returned during raw upload: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	remaining := make([]byte, size-len(firstChunk))
	if _, err := writer.Write(remaining); err != nil {
		t.Fatalf("write remaining upload: %v", err)
	}
	_ = writer.Close()
	select {
	case err := <-requestErr:
		t.Fatalf("raw upload request: %v", err)
	case resp := <-responseDone:
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("raw upload status: %d", resp.StatusCode)
		}
	}
	if err := <-shutdownDone; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	<-listenErr
	if info, err := os.Stat(filepath.Join(root, "shutdown.bin")); err != nil || info.Size() != size {
		t.Fatalf("completed upload missing: info=%v err=%v", info, err)
	}
}

func TestShutdownWaitsForSlowRangeDownload(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "shutdown-download.bin")
	const fileSize = 64 * 1024 * 1024
	file, err := os.Create(filePath)
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	if err := file.Truncate(fileSize); err != nil {
		t.Fatalf("truncate file: %v", err)
	}
	file.Close()
	info, _ := os.Stat(filePath)
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg := testConfig(root)
	runtime, err := NewRuntimeWithOptions(cfg, st, "", Options{DevFrontendPort: 5173})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	if err := st.CreateSession("shutdown-download-session", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	plain := "shutdown-download-lease"
	lease := &store.DownloadLease{Hash: security.HashToken(plain), Source: "session", SessionID: sql.NullString{String: "shutdown-download-session", Valid: true}, DirID: "default", Path: "shutdown-download.bin", ResourceFingerprint: testResourceFingerprint(t, cfg, "default"), FileSize: info.Size(), FileMtime: normalizedFileMtime(info), FileSHA256: sql.NullString{String: "", Valid: true}, ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.CreateDownloadLease(lease); err != nil {
		t.Fatalf("create lease: %v", err)
	}
	baseURL, listenErr := startRuntimeListener(t, runtime)
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/api/files/download-by-lease?lease="+plain, nil)
	req.Header.Set("Range", "bytes=0-33554431")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("start range download: %v", err)
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range status: %d", resp.StatusCode)
	}
	if err := st.DeleteSession("shutdown-download-session"); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- runtime.Shutdown() }()
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown returned during range response: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("read range response: %v", err)
	}
	resp.Body.Close()
	if err := <-shutdownDone; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	<-listenErr
}

func TestRuntimeShutdownCancelsMaintenanceWorker(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	started := make(chan struct{})
	var announced atomic.Bool
	walker := func(root string, fn fs.WalkDirFunc) error {
		if announced.CompareAndSwap(false, true) {
			close(started)
		}
		for {
			if err := fn(root, maintenanceEntry{name: "entry"}, nil); err != nil {
				return err
			}
			time.Sleep(time.Millisecond)
		}
	}
	runtime, err := NewRuntimeWithOptions(testConfig(root), st, "", Options{DevFrontendPort: 5173, uploadTempWalker: walker})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	<-started
	done := make(chan error, 1)
	go func() { done <- runtime.Shutdown() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("shutdown did not cancel maintenance worker")
	}
	runtime.server.uploadCleanupMu.Lock()
	running := runtime.server.uploadCleanupRunning
	runtime.server.uploadCleanupMu.Unlock()
	if running {
		t.Fatalf("cleanup worker remained running after shutdown")
	}
}

func TestStartupMaintenanceRunsImmediatelyInBackground(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	if err := st.CreateSession("expired-startup-session", past, "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := st.CreateToken(&store.Token{Hash: "expired-startup-token", Type: "download", DirID: "default", ExpiresAt: sql.NullTime{Time: past, Valid: true}}); err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := st.CreateDownloadLease(&store.DownloadLease{Hash: "expired-startup-download", Source: "session", DirID: "default", FileMtime: past, FileSHA256: sql.NullString{String: "", Valid: true}, ExpiresAt: past}); err != nil {
		t.Fatalf("create download lease: %v", err)
	}
	if err := st.CreateUploadLease(&store.UploadLease{Hash: "expired-startup-upload", Source: "session", SessionID: "owner", DirID: "default", ExpiresAt: past}); err != nil {
		t.Fatalf("create upload lease: %v", err)
	}
	for i := 0; i < 101; i++ {
		if err := st.Audit("startup-test", "", "entry"); err != nil {
			t.Fatalf("seed audit: %v", err)
		}
	}
	cfg := testConfig(t.TempDir())
	cfg.Audit.Retain = 100
	cfg.Audit.PruneEveryWrites = 0
	runtime, err := NewRuntimeWithOptions(cfg, st, "", Options{DevFrontendPort: 5173})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var sessions, tokens, downloads, uploads, audits int
		_ = st.DB.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions)
		_ = st.DB.QueryRow(`SELECT COUNT(*) FROM tokens`).Scan(&tokens)
		_ = st.DB.QueryRow(`SELECT COUNT(*) FROM download_leases`).Scan(&downloads)
		_ = st.DB.QueryRow(`SELECT COUNT(*) FROM upload_leases`).Scan(&uploads)
		_ = st.DB.QueryRow(`SELECT COUNT(*) FROM audit_logs`).Scan(&audits)
		if sessions == 0 && tokens == 0 && downloads == 0 && uploads == 0 && audits == 100 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("startup maintenance incomplete: sessions=%d tokens=%d downloads=%d uploads=%d audits=%d", sessions, tokens, downloads, uploads, audits)
		}
		time.Sleep(time.Millisecond)
	}
	if err := runtime.Shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func startRuntimeListener(t *testing.T, runtime *Runtime) (string, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- runtime.App.Listener(listener) }()
	return "http://" + listener.Addr().String(), errCh
}
