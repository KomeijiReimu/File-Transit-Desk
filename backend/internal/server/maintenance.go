package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filetrans-backend/internal/config"
	"filetrans-backend/internal/fsutil"
)

type uploadTempCleanupStats struct {
	Scanned   int
	Removed   int
	Skipped   int
	Complete  bool
	Truncated bool
}

type uploadTempCleanupRequest struct {
	Source uploadTempCleanupSource
	Roots  []config.Dir
}

type uploadTempCleanupSource uint8

const (
	uploadCleanupSourceStartup uploadTempCleanupSource = iota + 1
	uploadCleanupSourcePeriodic
	uploadCleanupSourceResourceChange
	uploadCleanupSourceCoalesced
)

func (source uploadTempCleanupSource) String() string {
	switch source {
	case uploadCleanupSourceStartup:
		return "startup"
	case uploadCleanupSourcePeriodic:
		return "periodic"
	case uploadCleanupSourceResourceChange:
		return "resource-change"
	default:
		return "coalesced"
	}
}

type uploadTempWalkFunc func(string, fs.WalkDirFunc) error

var errUploadTempCleanupStopped = errors.New("upload temp cleanup stopped")

func (s *Server) triggerCurrentUploadTempCleanup(source uploadTempCleanupSource) bool {
	return s.triggerUploadTempCleanup(uploadTempCleanupRequest{Source: source, Roots: s.cfg().Resources()})
}

func (s *Server) triggerUploadTempCleanup(request uploadTempCleanupRequest) bool {
	if s.maintenanceContext != nil {
		select {
		case <-s.maintenanceContext().Done():
			return false
		default:
		}
	}
	request.Roots = canonicalCleanupRoots(request.Roots)
	s.uploadCleanupMu.Lock()
	if s.uploadCleanupRunning {
		s.mergePendingUploadCleanupLocked(request)
		s.uploadCleanupMu.Unlock()
		return false
	}
	s.uploadCleanupRunning = true
	s.uploadCleanupMu.Unlock()
	if s.maintenanceWG != nil {
		s.maintenanceWG.Add(1)
	}
	go func() {
		if s.maintenanceWG != nil {
			defer s.maintenanceWG.Done()
		}
		s.runUploadTempCleanupWorker(request)
	}()
	return true
}

func (s *Server) runUploadTempCleanupWorker(request uploadTempCleanupRequest) {
	for {
		ctx := context.Background()
		if s.maintenanceContext != nil {
			ctx = s.maintenanceContext()
		}
		stats, err := s.runUploadTempCleanup(ctx, request)
		detail := fmt.Sprintf("source=%s scanned=%d removed=%d skipped=%d complete=%t truncated=%t", request.Source.String(), stats.Scanned, stats.Removed, stats.Skipped, stats.Complete, stats.Truncated)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.criticalAudit("upload_temp_cleanup_failed", "", detail)
		} else if stats.Removed > 0 || stats.Skipped > 0 || stats.Truncated {
			s.bestEffortAudit("upload_temp_cleanup", "", detail)
		}
		s.uploadCleanupMu.Lock()
		if len(s.uploadCleanupPendingRoots) == 0 {
			s.uploadCleanupRunning = false
			s.uploadCleanupMu.Unlock()
			return
		}
		request = uploadTempCleanupRequest{Source: s.uploadCleanupPendingSource, Roots: make([]config.Dir, 0, len(s.uploadCleanupPendingRoots))}
		for _, dir := range s.uploadCleanupPendingRoots {
			request.Roots = append(request.Roots, dir)
		}
		s.uploadCleanupPendingRoots = nil
		s.uploadCleanupPendingSource = 0
		s.uploadCleanupMu.Unlock()
	}
}

func (s *Server) mergePendingUploadCleanupLocked(request uploadTempCleanupRequest) {
	if s.uploadCleanupPendingRoots == nil {
		s.uploadCleanupPendingRoots = make(map[string]config.Dir)
	}
	for _, dir := range request.Roots {
		if existing, ok := s.uploadCleanupPendingRoots[dir.Path]; ok {
			dir.AllowUpload = dir.AllowUpload || existing.AllowUpload
		}
		s.uploadCleanupPendingRoots[dir.Path] = dir
	}
	if s.uploadCleanupPendingSource == 0 {
		s.uploadCleanupPendingSource = request.Source
	} else if s.uploadCleanupPendingSource != request.Source {
		s.uploadCleanupPendingSource = uploadCleanupSourceCoalesced
	}
}

func canonicalCleanupRoots(roots []config.Dir) []config.Dir {
	unique := make(map[string]config.Dir, len(roots))
	for _, dir := range roots {
		if dir.Type != config.ResourceDirectory || strings.TrimSpace(dir.Path) == "" {
			continue
		}
		canonical, err := fsutil.Canonical(dir.Path)
		if err != nil {
			continue
		}
		dir.Path = canonical
		if existing, ok := unique[canonical]; ok {
			dir.AllowUpload = dir.AllowUpload || existing.AllowUpload
		}
		unique[canonical] = dir
	}
	out := make([]config.Dir, 0, len(unique))
	for _, dir := range unique {
		out = append(out, dir)
	}
	return out
}

func (s *Server) runUploadTempCleanup(ctx context.Context, request uploadTempCleanupRequest) (uploadTempCleanupStats, error) {
	cfg := s.cfg()
	maxEntries := cfg.Storage.UploadTempCleanupMaxEntries
	maxDuration := time.Duration(cfg.Storage.UploadTempCleanupMaxDurationSeconds) * time.Second
	retention := time.Duration(cfg.Storage.UploadTempRetentionSeconds) * time.Second
	now := time.Now
	if s.maintenanceNow != nil {
		now = s.maintenanceNow
	}
	walk := uploadTempWalkFunc(filepath.WalkDir)
	if s.uploadTempWalker != nil {
		walk = s.uploadTempWalker
	}
	started := now()
	deadline := started.Add(maxDuration)
	cutoff := started.Add(-retention)
	active := s.transfers.activeTempPaths()
	stats := uploadTempCleanupStats{Complete: true}
	var stopErr error
	stop := func(err error) error {
		stats.Complete = false
		stats.Truncated = true
		stopErr = err
		return errUploadTempCleanupStopped
	}
	for _, dir := range request.Roots {
		if dir.Type != config.ResourceDirectory || strings.TrimSpace(dir.Path) == "" || !dir.AllowUpload {
			continue
		}
		err := walk(dir.Path, func(path string, entry fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return stop(err)
			}
			if !now().Before(deadline) {
				return stop(nil)
			}
			if stats.Scanned >= maxEntries {
				return stop(nil)
			}
			stats.Scanned++
			if walkErr != nil || entry == nil {
				stats.Skipped++
				return nil
			}
			if entry.IsDir() || !strings.HasPrefix(entry.Name(), ".upload-") || !strings.HasSuffix(entry.Name(), ".tmp") {
				return nil
			}
			if _, ok := active[canonicalTempPath(path)]; ok {
				stats.Skipped++
				return nil
			}
			info, err := entry.Info()
			if err != nil || info.ModTime().After(cutoff) {
				stats.Skipped++
				return nil
			}
			if err := os.Remove(path); err != nil {
				stats.Skipped++
			} else {
				stats.Removed++
			}
			return nil
		})
		if errors.Is(err, errUploadTempCleanupStopped) {
			return stats, stopErr
		}
		if err != nil {
			stats.Complete = false
			return stats, err
		}
	}
	return stats, nil
}
