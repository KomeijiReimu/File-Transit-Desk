package server

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filetrans-backend/internal/config"
	"filetrans-backend/internal/fsutil"
	"filetrans-backend/internal/store"
)

type maintenanceEntry struct {
	name string
	info os.FileInfo
}

func (e maintenanceEntry) Name() string { return e.name }
func (e maintenanceEntry) IsDir() bool  { return e.info != nil && e.info.IsDir() }
func (e maintenanceEntry) Type() fs.FileMode {
	if e.info == nil {
		return 0
	}
	return e.info.Mode().Type()
}
func (e maintenanceEntry) Info() (os.FileInfo, error) { return e.info, nil }

func TestUploadTempCleanupEntryAndTimeBounds(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.Storage.UploadTempCleanupMaxEntries = 100
	cfg.Storage.UploadTempCleanupMaxDurationSeconds = 5
	s := &Server{config: cfg, transfers: newTransferRegistry()}
	var requested int
	s.uploadTempWalker = func(root string, fn fs.WalkDirFunc) error {
		for i := 0; i < 101; i++ {
			requested++
			if err := fn(filepath.Join(root, "entry"), maintenanceEntry{name: "entry"}, nil); err != nil {
				return err
			}
		}
		return nil
	}
	stats, err := s.runUploadTempCleanup(context.Background(), uploadTempCleanupRequest{Source: uploadCleanupSourcePeriodic, Roots: cfg.Resources()})
	if err != nil || stats.Scanned != 100 || stats.Complete || !stats.Truncated || requested != 101 {
		t.Fatalf("entry bound stats=%+v requested=%d err=%v", stats, requested, err)
	}

	base := time.Unix(1700000000, 0)
	var nowCalls atomic.Int32
	s.maintenanceNow = func() time.Time {
		if nowCalls.Add(1) <= 2 {
			return base
		}
		return base.Add(2 * time.Second)
	}
	cfg.Storage.UploadTempCleanupMaxDurationSeconds = 1
	requested = 0
	stats, err = s.runUploadTempCleanup(context.Background(), uploadTempCleanupRequest{Source: uploadCleanupSourcePeriodic, Roots: cfg.Resources()})
	if err != nil || stats.Scanned != 1 || stats.Complete || !stats.Truncated || requested != 2 {
		t.Fatalf("duration bound stats=%+v requested=%d err=%v", stats, requested, err)
	}
}

func TestUploadTempCleanupCancellationAndActiveTemp(t *testing.T) {
	root := t.TempDir()
	activePath := filepath.Join(root, ".upload-active.tmp")
	stalePath := filepath.Join(root, ".upload-stale.tmp")
	for _, path := range []string{activePath, stalePath} {
		if err := os.WriteFile(path, []byte("temp"), 0600); err != nil {
			t.Fatalf("write temp: %v", err)
		}
		old := time.Now().Add(-2 * time.Hour)
		_ = os.Chtimes(path, old, old)
	}
	cfg := testConfig(root)
	cfg.Storage.UploadTempRetentionSeconds = 60
	s := &Server{config: cfg, transfers: newTransferRegistry()}
	s.transfers.add(&transferRecord{ID: "active", Type: "upload", Status: transferActive, TempPath: activePath})
	stats, err := s.runUploadTempCleanup(context.Background(), uploadTempCleanupRequest{Source: uploadCleanupSourcePeriodic, Roots: cfg.Resources()})
	if err != nil || stats.Removed != 1 || stats.Skipped != 1 {
		t.Fatalf("active cleanup stats=%+v err=%v", stats, err)
	}
	if _, err := os.Stat(activePath); err != nil {
		t.Fatalf("active temp was removed: %v", err)
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("stale temp was not removed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.uploadTempWalker = func(root string, fn fs.WalkDirFunc) error {
		return fn(root, maintenanceEntry{name: "root"}, nil)
	}
	stats, err = s.runUploadTempCleanup(ctx, uploadTempCleanupRequest{Source: uploadCleanupSourcePeriodic, Roots: cfg.Resources()})
	if err != context.Canceled || stats.Scanned != 0 || stats.Complete || !stats.Truncated {
		t.Fatalf("cancel stats=%+v err=%v", stats, err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	workerStarted := make(chan struct{})
	var announced atomic.Bool
	s.store = st
	s.maintenanceContext = func() context.Context { return workerCtx }
	s.uploadTempWalker = func(root string, fn fs.WalkDirFunc) error {
		if announced.CompareAndSwap(false, true) {
			close(workerStarted)
		}
		for {
			if err := fn(root, maintenanceEntry{name: "root"}, nil); err != nil {
				return err
			}
			time.Sleep(time.Millisecond)
		}
	}
	if !s.triggerCurrentUploadTempCleanup(uploadCleanupSourcePeriodic) {
		t.Fatalf("cleanup trigger rejected")
	}
	<-workerStarted
	cancelWorker()
	waitForUploadCleanupIdle(t, s)
	logs, _ := st.AuditLogs(10)
	for _, entry := range logs {
		if entry.Action == "upload_temp_cleanup_failed" {
			t.Fatalf("context cancellation was incorrectly audited as failure: %+v", entry)
		}
	}
}

func TestUploadTempCleanupOverlapAndTriggerAreNonBlocking(t *testing.T) {
	oldRoot := t.TempDir()
	newRoot := t.TempDir()
	cfg := testConfig(oldRoot)
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	firstStarted := make(chan struct{})
	firstRelease := make(chan struct{})
	pendingStarted := make(chan struct{})
	pendingRelease := make(chan struct{})
	var calls atomic.Int32
	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32
	var rootsMu sync.Mutex
	rootCalls := map[string]int{}
	s := &Server{config: cfg, store: st, transfers: newTransferRegistry(), uploadTempWalker: func(root string, fn fs.WalkDirFunc) error {
		current := concurrent.Add(1)
		defer concurrent.Add(-1)
		for {
			old := maxConcurrent.Load()
			if current <= old || maxConcurrent.CompareAndSwap(old, current) {
				break
			}
		}
		rootsMu.Lock()
		rootCalls[root]++
		rootsMu.Unlock()
		switch calls.Add(1) {
		case 1:
			close(firstStarted)
			<-firstRelease
		case 2:
			close(pendingStarted)
			<-pendingRelease
		}
		return nil
	}}
	begin := time.Now()
	if !s.triggerCurrentUploadTempCleanup(uploadCleanupSourceStartup) {
		t.Fatalf("first cleanup trigger rejected")
	}
	if time.Since(begin) > 100*time.Millisecond {
		t.Fatalf("cleanup trigger blocked caller")
	}
	<-firstStarted
	if s.triggerCurrentUploadTempCleanup(uploadCleanupSourcePeriodic) {
		t.Fatalf("overlapping cleanup should coalesce without starting a worker")
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(configPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	s.configPath = configPath
	updateDone := make(chan error, 1)
	go func() {
		updateDone <- s.updateConfigResources(func(resources []config.Dir) ([]config.Dir, error) {
			resources[0].Path = newRoot
			return resources, nil
		})
	}()
	select {
	case err := <-updateDone:
		if err != nil {
			t.Fatalf("resource update: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("resource update waited for cleanup walker")
	}
	if s.triggerCurrentUploadTempCleanup(uploadCleanupSourcePeriodic) {
		t.Fatalf("post-update periodic trigger should merge into pending")
	}
	if s.triggerCurrentUploadTempCleanup(uploadCleanupSourcePeriodic) {
		t.Fatalf("duplicate pending trigger should not start a worker")
	}
	s.uploadCleanupMu.Lock()
	pendingRoots := len(s.uploadCleanupPendingRoots)
	pendingSource := s.uploadCleanupPendingSource
	s.uploadCleanupMu.Unlock()
	if pendingRoots != 2 || pendingSource != uploadCleanupSourceCoalesced {
		t.Fatalf("unexpected pending merge: roots=%d source=%v", pendingRoots, pendingSource)
	}
	close(firstRelease)
	<-pendingStarted
	s.uploadCleanupMu.Lock()
	runningDuringPending := s.uploadCleanupRunning
	s.uploadCleanupMu.Unlock()
	if !runningDuringPending {
		t.Fatalf("cleanup incorrectly became idle while pending batch was running")
	}
	close(pendingRelease)
	waitForUploadCleanupIdle(t, s)
	if maxConcurrent.Load() != 1 || calls.Load() != 3 {
		t.Fatalf("cleanup worker concurrency/calls max=%d calls=%d", maxConcurrent.Load(), calls.Load())
	}
	oldCanonical, _ := fsutil.Canonical(oldRoot)
	newCanonical, _ := fsutil.Canonical(newRoot)
	rootsMu.Lock()
	oldCalls := rootCalls[oldCanonical]
	newCalls := rootCalls[newCanonical]
	rootsMu.Unlock()
	if oldCalls != 2 || newCalls != 1 {
		t.Fatalf("pending roots were not deduplicated/scanned: old=%d new=%d all=%v", oldCalls, newCalls, rootCalls)
	}
}

func TestStartupCleanupDoesNotBlockAndTruncatedAuditHasNoPath(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	cfg.Storage.UploadTempCleanupMaxEntries = 100
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	started := make(chan struct{})
	release := make(chan struct{})
	walker := func(root string, fn fs.WalkDirFunc) error {
		close(started)
		<-release
		for i := 0; i < 101; i++ {
			if err := fn(filepath.Join(root, "secret-path", "entry"), maintenanceEntry{name: "entry"}, nil); err != nil {
				return err
			}
		}
		return nil
	}
	begin := time.Now()
	if _, err := NewWithOptions(cfg, st, "", Options{DevFrontendPort: 5173, uploadTempWalker: walker}); err != nil {
		t.Fatalf("new server: %v", err)
	}
	if time.Since(begin) > 250*time.Millisecond {
		t.Fatalf("server startup waited for cleanup walker")
	}
	<-started
	close(release)
	// NewWithOptions 已创建内部 Server，单独验证相同触发器的审计契约。
	s := &Server{config: cfg, store: st, transfers: newTransferRegistry(), uploadTempWalker: func(root string, fn fs.WalkDirFunc) error {
		for i := 0; i < 101; i++ {
			if err := fn(filepath.Join(root, "secret-path", "entry"), maintenanceEntry{name: "entry"}, nil); err != nil {
				return err
			}
		}
		return nil
	}}
	if !s.triggerCurrentUploadTempCleanup(uploadCleanupSourcePeriodic) {
		t.Fatalf("audit cleanup trigger rejected")
	}
	waitForUploadCleanupIdle(t, s)
	logs, err := st.AuditLogs(20)
	if err != nil {
		t.Fatalf("audit logs: %v", err)
	}
	found := false
	for _, entry := range logs {
		if entry.Action == "upload_temp_cleanup" && strings.Contains(entry.Detail, "truncated=true") {
			found = true
			if strings.Contains(entry.Detail, root) || strings.Contains(entry.Detail, "secret-path") {
				t.Fatalf("cleanup audit leaked path: %+v", entry)
			}
		}
	}
	if !found {
		t.Fatalf("missing truncated cleanup audit: %+v", logs)
	}
	s.uploadTempWalker = func(root string, fn fs.WalkDirFunc) error {
		return errors.New("walker failed at " + root)
	}
	if !s.triggerCurrentUploadTempCleanup(uploadCleanupSourcePeriodic) {
		t.Fatalf("failure cleanup trigger rejected")
	}
	waitForUploadCleanupIdle(t, s)
	logs, _ = st.AuditLogs(20)
	foundFailure := false
	for _, entry := range logs {
		if entry.Action == "upload_temp_cleanup_failed" {
			foundFailure = true
			if strings.Contains(entry.Detail, root) || strings.Contains(entry.Detail, "walker failed") {
				t.Fatalf("cleanup failure audit leaked walker/path detail: %+v", entry)
			}
		}
	}
	if !foundFailure {
		t.Fatalf("missing cleanup failure audit: %+v", logs)
	}
}

func waitForUploadCleanupIdle(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.uploadCleanupMu.Lock()
		running := s.uploadCleanupRunning
		s.uploadCleanupMu.Unlock()
		if !running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("upload cleanup did not finish")
}
