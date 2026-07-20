package server

import (
	"context"
	"database/sql"
	"errors"
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

func TestStoreMaintenanceHonorsConfiguredIdleGrace(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()

	cfg := testConfig(t.TempDir())
	cfg.Auth.IdleGraceSeconds = 45
	runtime := newRuntime(st)
	defer runtime.cancel()
	runtime.server = &Server{config: cfg}

	now := time.Now().UTC()
	if err := st.CreateSessionWithIdle("within-grace", now.Add(time.Hour), now.Add(-30*time.Second), "user", ""); err != nil {
		t.Fatalf("create within-grace session: %v", err)
	}
	if err := st.CreateSessionWithIdle("past-grace", now.Add(time.Hour), now.Add(-50*time.Second), "user", ""); err != nil {
		t.Fatalf("create past-grace session: %v", err)
	}
	if err := st.CreateSessionWithIdle("absolute-expired", now.Add(-time.Second), now.Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create absolute-expired session: %v", err)
	}

	runtime.runStoreMaintenance()
	if _, err := st.Session("within-grace"); err != nil {
		t.Fatalf("maintenance removed session before configured grace ended: %v", err)
	}
	for _, id := range []string{"past-grace", "absolute-expired"} {
		if _, err := st.Session(id); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("maintenance retained expired session %q: %v", id, err)
		}
	}

	if err := st.TouchSession("within-grace", now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatalf("move session beyond grace: %v", err)
	}
	runtime.runStoreMaintenance()
	if _, err := st.Session("within-grace"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("maintenance retained session after grace ended: %v", err)
	}
}

func TestStartupChatRetentionCatchesUpBeforeReadyAcrossBatches(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "startup-chat.db"), 1000)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Now().UTC()
	seedLifecycleChatMessages(t, st, 7, now.AddDate(0, 0, -2))
	cfg := testConfig(t.TempDir())
	cfg.Chat.RetentionDays = 1
	cfg.Chat.MaxMessages = 2
	cfg.Chat.CleanupBatch = 2
	runtime, err := NewRuntimeWithOptions(cfg, st, "", Options{DevFrontendPort: 5173, maintenanceNow: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	defer runtime.Shutdown()
	if !runtime.IsReady() {
		t.Fatalf("runtime was not ready after successful startup catch-up")
	}
	if count := lifecycleChatMessageCount(t, st); count != 0 {
		// Every seeded message is older than the age policy, so all seven must be
		// drained despite a batch size of two before readiness is published.
		t.Fatalf("startup exposed over-retention messages: %d", count)
	}
	state, err := st.CurrentChatSyncState()
	if err != nil || state.Generation != 5 {
		t.Fatalf("startup cleanup generation=%+v err=%v", state, err)
	}
}

func TestStartupChatRetentionFailureKeepsRuntimeNotReady(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "startup-chat-failure.db"), 1000)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Now().UTC()
	seedLifecycleChatMessages(t, st, 1, now.AddDate(0, 0, -2))
	if _, err := st.DB.Exec(`DROP TABLE chat_sync_metadata`); err != nil {
		t.Fatalf("drop sync metadata: %v", err)
	}
	cfg := testConfig(t.TempDir())
	cfg.Chat.RetentionDays = 1
	runtime := newRuntime(st)
	_, err = NewWithOptions(cfg, st, "", Options{DevFrontendPort: 5173, runtime: runtime, maintenanceNow: func() time.Time { return now }})
	if err == nil {
		t.Fatalf("startup succeeded after chat retention metadata failure")
	}
	if runtime.IsReady() {
		t.Fatalf("failed startup runtime became ready")
	}
	if count := lifecycleChatMessageCount(t, st); count != 1 {
		t.Fatalf("failed cleanup transaction did not roll back: messages=%d", count)
	}
	_ = runtime.Shutdown()
}

func TestPeriodicChatRetentionBudgetRetriesQuicklyUntilCaughtUp(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "periodic-chat.db"), 1000)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Now().UTC()
	seedLifecycleChatMessages(t, st, 7, now.Add(-time.Hour))
	cfg := testConfig(t.TempDir())
	cfg.Chat.RetentionDays = 90
	cfg.Chat.MaxMessages = 1
	cfg.Chat.CleanupBatch = 2
	runtime := newRuntime(st)
	runtime.server = &Server{runtime: runtime, config: cfg, store: st, transfers: newTransferRegistry()}
	runtime.storeMaintenanceInterval = time.Hour
	runtime.chatCleanupBudget = time.Nanosecond
	runtime.chatCleanupRetryDelay = 10 * time.Millisecond
	runtime.startMaintenance()
	defer runtime.Shutdown()
	deadline := time.Now().Add(time.Second)
	for lifecycleChatMessageCount(t, st) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("periodic retry did not catch up: messages=%d", lifecycleChatMessageCount(t, st))
		}
		time.Sleep(time.Millisecond)
	}
	state, err := st.CurrentChatSyncState()
	if err != nil || state.Generation != 4 {
		t.Fatalf("periodic multi-batch generation=%+v err=%v", state, err)
	}
}

func TestChatCleanupLoopStopsOnDrainCancellation(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "cancel-chat.db"), 1000)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	now := time.Now().UTC()
	seedLifecycleChatMessages(t, st, 5, now.Add(-time.Hour))
	cfg := testConfig(t.TempDir())
	cfg.Chat.MaxMessages = 1
	cfg.Chat.CleanupBatch = 1
	runtime := newRuntime(st)
	runtime.server = &Server{runtime: runtime, config: cfg, store: st}
	runtime.BeginDrain()
	if _, err := runtime.cleanupChatBatches(runtime.ctx, now, cfg.Chat, time.Time{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cleanup did not stop on drain cancellation: %v", err)
	}
	if count := lifecycleChatMessageCount(t, st); count != 5 {
		t.Fatalf("canceled cleanup removed messages: %d", count)
	}
}

func TestDefaultChatRetentionThroughputConvergesAtMaximumWriteRate(t *testing.T) {
	if defaultChatCleanupRetryDelay > time.Second {
		t.Fatalf("chat retry delay exceeds one second: %s", defaultChatCleanupRetryDelay)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "throughput-chat.db"), 1000)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	now := time.Now().UTC()
	cfg := testConfig(t.TempDir())
	cfg.Chat.RetentionDays = 3650
	cfg.Chat.MaxMessages = 10
	seedLifecycleChatMessages(t, st, 1510, now.Add(-time.Hour))
	runtime := newRuntime(st)
	defer runtime.cancel()
	runtime.server = &Server{runtime: runtime, config: cfg, store: st}
	runtime.chatCleanupBudget = time.Nanosecond
	writesPerRetry := (cfg.Chat.GlobalMessagesPerMinute*int(defaultChatCleanupRetryDelay/time.Second) + 59) / 60
	if writesPerRetry >= cfg.Chat.CleanupBatch {
		t.Fatalf("default cleanup cannot outpace writes: batch=%d writes/retry=%d", cfg.Chat.CleanupBatch, writesPerRetry)
	}
	for cycle := 0; cycle < 10; cycle++ {
		needsRetry := runtime.runPeriodicChatMaintenanceAt(now)
		count := lifecycleChatMessageCount(t, st)
		if !needsRetry {
			if count != cfg.Chat.MaxMessages {
				t.Fatalf("cleanup stopped above max messages: %d", count)
			}
			return
		}
		seedLifecycleChatMessages(t, st, writesPerRetry, now.Add(time.Duration(cycle+1)*time.Minute))
	}
	t.Fatalf("default cleanup did not converge: messages=%d", lifecycleChatMessageCount(t, st))
}

func seedLifecycleChatMessages(t *testing.T, st *store.Store, count int, createdAt time.Time) {
	t.Helper()
	tx, err := st.DB.Begin()
	if err != nil {
		t.Fatalf("begin chat seed: %v", err)
	}
	defer tx.Rollback()
	for index := 0; index < count; index++ {
		at := createdAt.Add(time.Duration(index) * time.Nanosecond)
		result, err := tx.Exec(`INSERT INTO chat_messages(author_key, author_tag, author_role, source_ip, body, created_at) VALUES('owner', '访客-AAAAAA', 'user', '192.0.2.1', 'seed', ?)`, at)
		if err != nil {
			t.Fatalf("insert chat seed: %v", err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatalf("chat seed id: %v", err)
		}
		if _, err := tx.Exec(`INSERT INTO chat_changes(message_id, kind, created_at) VALUES(?, 'create', ?)`, id, at); err != nil {
			t.Fatalf("insert chat seed change: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit chat seed: %v", err)
	}
}

func lifecycleChatMessageCount(t *testing.T, st *store.Store) int {
	t.Helper()
	var count int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM chat_messages`).Scan(&count); err != nil {
		t.Fatalf("count chat messages: %v", err)
	}
	return count
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
