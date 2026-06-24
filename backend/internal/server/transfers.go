package server

import (
	"context"
	"path/filepath"
	"sync"
	"time"
)

type transferStatus string

const (
	transferActive             transferStatus = "active"
	transferCanceling          transferStatus = "canceling"
	transferCompleted          transferStatus = "completed"
	completedTransferRetention                = 30 * time.Second
)

type transferRecord struct {
	ID               string         `json:"id"`
	Type             string         `json:"type"`
	Status           transferStatus `json:"status"`
	Source           string         `json:"source"`
	DirID            string         `json:"dirId"`
	Path             string         `json:"path"`
	FileName         string         `json:"fileName"`
	TotalBytes       int64          `json:"totalBytes"`
	TransferredBytes int64          `json:"transferredBytes"`
	CurrentSpeedBps  int64          `json:"currentSpeedBps,omitempty"`
	AverageSpeedBps  int64          `json:"averageSpeedBps,omitempty"`
	StartedAt        time.Time      `json:"startedAt"`
	UpdatedAt        time.Time      `json:"updatedAt"`
	ClientIP         string         `json:"clientIP"`
	Cancelable       bool           `json:"cancelable"`
	BestEffort       bool           `json:"bestEffort"`
	TempPath         string         `json:"-"`
	cancel           context.CancelFunc
	lastBytes        int64
	lastSpeedAt      time.Time
	keepUntil        time.Time
}

type transferRegistry struct {
	mu      sync.RWMutex
	records map[string]*transferRecord
}

func newTransferRegistry() *transferRegistry {
	return &transferRegistry{records: map[string]*transferRecord{}}
}

func (r *transferRegistry) add(rec *transferRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec.TempPath = canonicalTempPath(rec.TempPath)
	now := time.Now()
	if rec.StartedAt.IsZero() {
		rec.StartedAt = now
	}
	if rec.UpdatedAt.IsZero() {
		rec.UpdatedAt = now
	}
	rec.lastSpeedAt = rec.UpdatedAt
	r.records[rec.ID] = rec
}

func (r *transferRegistry) update(id string, fn func(*transferRecord)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec := r.records[id]; rec != nil {
		// 回调在注册表锁内执行，只允许做轻量字段更新，不能执行 I/O 或再次调用 registry 方法。
		fn(rec)
	}
}

func (r *transferRegistry) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec := r.records[id]; rec != nil {
		now := time.Now()
		rec.Status = transferCompleted
		rec.UpdatedAt = now
		rec.Cancelable = false
		rec.cancel = nil
		rec.TempPath = ""
		rec.keepUntil = now.Add(completedTransferRetention)
	}
}

func (r *transferRegistry) list() []transferRecord {
	r.purgeExpiredCompleted()
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]transferRecord, 0, len(r.records))
	for _, rec := range r.records {
		copy := *rec
		copy.cancel = nil
		out = append(out, copy)
	}
	return out
}

func (r *transferRegistry) cancel(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := r.records[id]
	if rec == nil || rec.cancel == nil || !rec.Cancelable {
		return false
	}
	rec.Status = transferCanceling
	rec.UpdatedAt = time.Now()
	rec.cancel()
	return true
}

func (r *transferRegistry) activeTempPaths() map[string]struct{} {
	r.purgeExpiredCompleted()
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string]struct{}{}
	for _, rec := range r.records {
		if rec.Status == transferCompleted {
			continue
		}
		if normalized := canonicalTempPath(rec.TempPath); normalized != "" {
			out[normalized] = struct{}{}
		}
	}
	return out
}

func (r *transferRegistry) purgeExpiredCompleted() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for id, rec := range r.records {
		if rec.Status == transferCompleted && !rec.keepUntil.IsZero() && now.After(rec.keepUntil) {
			delete(r.records, id)
		}
	}
}

func canonicalTempPath(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	return filepath.Clean(path)
}
