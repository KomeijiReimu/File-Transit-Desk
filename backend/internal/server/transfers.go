package server

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"time"
)

type transferStatus string

const (
	transferActive             transferStatus = "active"
	transferCanceling          transferStatus = "canceling"
	transferCompleted          transferStatus = "completed"
	transferFailed             transferStatus = "failed"
	completedTransferRetention                = 30 * time.Second
)

var errUploadGlobalCapacity = errors.New("global upload concurrency exhausted")
var errUploadScopedCapacity = errors.New("scoped upload concurrency exhausted")

type uploadAdmissionLimits struct {
	Global      int
	PerResource int
	PerSession  int
	PerToken    int
}

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
	OwnerType        string         `json:"-"`
	OwnerID          string         `json:"-"`
	PermitID         string         `json:"-"`
	cancel           context.CancelFunc
	lastBytes        int64
	lastSpeedAt      time.Time
	keepUntil        time.Time
}

type transferRegistry struct {
	mu      sync.RWMutex
	records map[string]*transferRecord
	permits map[string]*uploadPermit
}

type uploadPermit struct {
	ID                  string
	DirID               string
	ResourceFingerprint string
	OwnerType           string
	OwnerID             string
	canceling           bool
	cancel              context.CancelFunc
}

func newTransferRegistry() *transferRegistry {
	return &transferRegistry{records: map[string]*transferRecord{}, permits: map[string]*uploadPermit{}}
}

func (r *transferRegistry) add(rec *transferRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addLocked(rec)
}

func (r *transferRegistry) addLocked(rec *transferRecord) {
	rec.TempPath = canonicalTempPath(rec.TempPath)
	now := time.Now()
	if existing := r.records[rec.ID]; existing != nil && rec.StartedAt.IsZero() {
		rec.StartedAt = existing.StartedAt
	}
	if rec.StartedAt.IsZero() {
		rec.StartedAt = now
	}
	if rec.UpdatedAt.IsZero() {
		rec.UpdatedAt = now
	}
	rec.lastSpeedAt = rec.UpdatedAt
	r.records[rec.ID] = rec
}

func (r *transferRegistry) tryAcquireUploadPermit(permit *uploadPermit, limits uploadAdmissionLimits) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if permit == nil || permit.ID == "" || permit.OwnerID == "" || permit.OwnerType != "session" && permit.OwnerType != "token" || permit.DirID == "" && permit.ResourceFingerprint != "" || permit.DirID != "" && permit.ResourceFingerprint == "" {
		return errUploadScopedCapacity
	}
	global, resource, owner := 0, 0, 0
	for _, existing := range r.permits {
		global++
		if permit.DirID != "" && existing.DirID == permit.DirID {
			resource++
		}
		if existing.OwnerType == permit.OwnerType && existing.OwnerID == permit.OwnerID {
			owner++
		}
	}
	if limits.Global > 0 && global >= limits.Global {
		return errUploadGlobalCapacity
	}
	if permit.DirID != "" && limits.PerResource > 0 && resource >= limits.PerResource {
		return errUploadScopedCapacity
	}
	ownerLimit := 0
	if permit.OwnerType == "session" {
		ownerLimit = limits.PerSession
	} else if permit.OwnerType == "token" {
		ownerLimit = limits.PerToken
	}
	if ownerLimit > 0 && owner >= ownerLimit {
		return errUploadScopedCapacity
	}
	r.permits[permit.ID] = permit
	return nil
}

func (r *transferRegistry) bindUploadPermit(id, dirID, fingerprint string, perResource int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	permit := r.permits[id]
	if permit == nil || permit.canceling || dirID == "" || fingerprint == "" {
		return errUploadScopedCapacity
	}
	if permit.DirID != "" {
		if permit.DirID != dirID || permit.ResourceFingerprint != fingerprint {
			return errUploadScopedCapacity
		}
		return nil
	}
	resource := 0
	for otherID, existing := range r.permits {
		if otherID != id && existing.DirID == dirID {
			resource++
		}
	}
	if perResource > 0 && resource >= perResource {
		return errUploadScopedCapacity
	}
	permit.DirID = dirID
	permit.ResourceFingerprint = fingerprint
	return nil
}

func (r *transferRegistry) releaseUploadPermit(id string) {
	if id == "" {
		return
	}
	r.mu.Lock()
	delete(r.permits, id)
	r.mu.Unlock()
}

func (r *transferRegistry) uploadPermitCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.permits)
}

func (r *transferRegistry) hasBoundUploadPermit(id, dirID, fingerprint string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	permit := r.permits[id]
	return permit != nil && !permit.canceling && permit.DirID == dirID && permit.ResourceFingerprint == fingerprint
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
	r.purgeExpiredRecords()
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
	rec := r.records[id]
	if rec == nil || rec.cancel == nil || !rec.Cancelable || rec.Status == transferCanceling || rec.Status == transferCompleted {
		r.mu.Unlock()
		return false
	}
	rec.Status = transferCanceling
	rec.UpdatedAt = time.Now()
	cancel := rec.cancel
	r.mu.Unlock()
	cancel()
	return true
}

func (r *transferRegistry) cancelUploadsByDirIDs(dirIDs []string) int {
	wanted := make(map[string]struct{}, len(dirIDs))
	for _, id := range dirIDs {
		wanted[id] = struct{}{}
	}
	now := time.Now()
	callbacksByRequest := map[string]context.CancelFunc{}
	r.mu.Lock()
	matchingPermits := map[string]*uploadPermit{}
	for id, permit := range r.permits {
		if _, ok := wanted[permit.DirID]; !ok || permit.canceling {
			continue
		}
		permit.canceling = true
		matchingPermits[id] = permit
	}
	for _, rec := range r.records {
		if _, ok := wanted[rec.DirID]; !ok || rec.Type != "upload" || rec.Status == transferCompleted || rec.Status == transferCanceling || rec.cancel == nil || !rec.Cancelable {
			continue
		}
		rec.Status = transferCanceling
		rec.UpdatedAt = now
		key := rec.PermitID
		if key == "" {
			key = "record:" + rec.ID
		}
		if _, exists := callbacksByRequest[key]; !exists {
			callbacksByRequest[key] = rec.cancel
		}
	}
	for id, permit := range matchingPermits {
		if _, hasRecordCallback := callbacksByRequest[id]; !hasRecordCallback && permit.cancel != nil {
			callbacksByRequest[id] = permit.cancel
		}
	}
	r.mu.Unlock()
	for _, cancel := range callbacksByRequest {
		cancel()
	}
	return len(callbacksByRequest)
}

func (r *transferRegistry) activeTempPaths() map[string]struct{} {
	r.purgeExpiredRecords()
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

func (r *transferRegistry) purgeExpiredRecords() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for id, rec := range r.records {
		if !rec.keepUntil.IsZero() && now.After(rec.keepUntil) {
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
