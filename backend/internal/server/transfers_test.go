package server

import (
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

func TestUploadPermitEnforcesGlobalResourceAndOwnerLimits(t *testing.T) {
	cases := []struct {
		name   string
		limits uploadAdmissionLimits
		first  *uploadPermit
		second *uploadPermit
		want   error
	}{
		{"global", uploadAdmissionLimits{Global: 1}, permitRec("a", "d1", "f1", "session", "s1"), permitRec("b", "d2", "f2", "session", "s2"), errUploadGlobalCapacity},
		{"resource", uploadAdmissionLimits{Global: 10, PerResource: 1}, permitRec("a", "d1", "f1", "session", "s1"), permitRec("b", "d1", "f1", "session", "s2"), errUploadScopedCapacity},
		{"session", uploadAdmissionLimits{Global: 10, PerSession: 1}, permitRec("a", "d1", "f1", "session", "s1"), permitRec("b", "d2", "f2", "session", "s1"), errUploadScopedCapacity},
		{"token", uploadAdmissionLimits{Global: 10, PerToken: 1}, permitRec("a", "d1", "f1", "token", "1"), permitRec("b", "d2", "f2", "token", "1"), errUploadScopedCapacity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry := newTransferRegistry()
			if err := registry.tryAcquireUploadPermit(tc.first, tc.limits); err != nil {
				t.Fatalf("first permit: %v", err)
			}
			if err := registry.tryAcquireUploadPermit(tc.second, tc.limits); !errors.Is(err, tc.want) {
				t.Fatalf("second permit error=%v want=%v", err, tc.want)
			}
			if registry.uploadPermitCount() != 1 {
				t.Fatalf("rejected permit must not be retained")
			}
		})
	}
}

func TestUploadPermitConcurrentAdmissionIsAtomic(t *testing.T) {
	registry := newTransferRegistry()
	limits := uploadAdmissionLimits{Global: 4, PerResource: 4, PerSession: 4}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := registry.tryAcquireUploadPermit(permitRec("concurrent-"+strconv.Itoa(i), "dir", "fingerprint", "session", "session"), limits); err == nil {
				successes.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if successes.Load() != 4 {
		t.Fatalf("expected exactly four permits, got %d", successes.Load())
	}
}

func TestCancelUploadsByDirIDsIsSelectiveAndIdempotent(t *testing.T) {
	registry := newTransferRegistry()
	uploadCalls := 0
	downloadCalls := 0
	registry.add(&transferRecord{ID: "upload", Type: "upload", Status: transferActive, DirID: "changed", Cancelable: true, cancel: func() {
		uploadCalls++
		// 回调必须在 registry mutex 外执行，否则这里会死锁。
		registry.update("upload", func(rec *transferRecord) { rec.BestEffort = true })
	}})
	registry.add(&transferRecord{ID: "download", Type: "download", Status: transferActive, DirID: "changed", Cancelable: true, cancel: func() { downloadCalls++ }})
	registry.add(&transferRecord{ID: "other", Type: "upload", Status: transferActive, DirID: "other", Cancelable: true, cancel: func() { t.Fatal("unexpected other upload cancellation") }})
	registry.add(&transferRecord{ID: "completed", Type: "upload", Status: transferCompleted, DirID: "changed", Cancelable: true, cancel: func() { t.Fatal("unexpected completed upload cancellation") }})

	if got := registry.cancelUploadsByDirIDs([]string{"changed"}); got != 1 {
		t.Fatalf("expected one canceled upload, got %d", got)
	}
	if got := registry.cancelUploadsByDirIDs([]string{"changed"}); got != 0 {
		t.Fatalf("expected repeated cancellation to be idempotent, got %d", got)
	}
	if uploadCalls != 1 || downloadCalls != 0 {
		t.Fatalf("unexpected callbacks: upload=%d download=%d", uploadCalls, downloadCalls)
	}
	items := registry.list()
	statuses := map[string]transferStatus{}
	for _, item := range items {
		statuses[item.ID] = item.Status
	}
	if statuses["upload"] != transferCanceling || statuses["download"] != transferActive || statuses["other"] != transferActive || statuses["completed"] != transferCompleted {
		t.Fatalf("unexpected transfer statuses: %+v", statuses)
	}
}

func TestUploadPermitBindAndRecordsDoNotDoubleCount(t *testing.T) {
	registry := newTransferRegistry()
	limits := uploadAdmissionLimits{Global: 2, PerResource: 1, PerSession: 2}
	first := &uploadPermit{ID: "request-one", OwnerType: "session", OwnerID: "session-one"}
	if err := registry.tryAcquireUploadPermit(first, limits); err != nil {
		t.Fatalf("acquire unbound permit: %v", err)
	}
	if err := registry.bindUploadPermit(first.ID, "dir", "fingerprint", limits.PerResource); err != nil {
		t.Fatalf("bind permit: %v", err)
	}
	for i := 0; i < 3; i++ {
		registry.add(&transferRecord{ID: "file-" + strconv.Itoa(i), PermitID: first.ID, Type: "upload", Status: transferActive, DirID: "dir"})
	}
	second := &uploadPermit{ID: "request-two", OwnerType: "session", OwnerID: "session-two"}
	if err := registry.tryAcquireUploadPermit(second, limits); err != nil {
		t.Fatalf("progress records must not double-count request permit: %v", err)
	}
	if err := registry.bindUploadPermit(second.ID, "dir", "fingerprint", limits.PerResource); !errors.Is(err, errUploadScopedCapacity) {
		t.Fatalf("expected resource limit at bind, got %v", err)
	}
	third := &uploadPermit{ID: "request-three", OwnerType: "session", OwnerID: "session-three"}
	if err := registry.tryAcquireUploadPermit(third, limits); !errors.Is(err, errUploadGlobalCapacity) {
		t.Fatalf("expected two request permits to fill global capacity, got %v", err)
	}
}

func TestUploadPermitRejectsUnknownOrEmptyOwner(t *testing.T) {
	registry := newTransferRegistry()
	for _, permit := range []*uploadPermit{
		{ID: "empty-owner", OwnerType: "session"},
		{ID: "unknown-owner", OwnerType: "other", OwnerID: "value"},
		{ID: "bound-without-fingerprint", DirID: "dir", OwnerType: "session", OwnerID: "value"},
		{ID: "fingerprint-without-dir", ResourceFingerprint: "fingerprint", OwnerType: "session", OwnerID: "value"},
	} {
		if err := registry.tryAcquireUploadPermit(permit, uploadAdmissionLimits{}); !errors.Is(err, errUploadScopedCapacity) {
			t.Fatalf("expected fail-closed owner rejection for %+v, got %v", permit, err)
		}
	}
	if registry.uploadPermitCount() != 0 {
		t.Fatalf("invalid owners must not acquire permits")
	}
	valid := &uploadPermit{ID: "valid", OwnerType: "session", OwnerID: "owner"}
	if err := registry.tryAcquireUploadPermit(valid, uploadAdmissionLimits{}); err != nil {
		t.Fatalf("acquire valid permit: %v", err)
	}
	registry.releaseUploadPermit(valid.ID)
	registry.releaseUploadPermit(valid.ID)
	if registry.uploadPermitCount() != 0 {
		t.Fatalf("permit release must be idempotent")
	}
}

func TestCancelUploadsByDirIDsCancelsBoundPermitOnce(t *testing.T) {
	registry := newTransferRegistry()
	permitCalls, recordCalls := 0, 0
	permit := &uploadPermit{ID: "request", DirID: "changed", ResourceFingerprint: "fingerprint", OwnerType: "session", OwnerID: "owner", cancel: func() { permitCalls++ }}
	if err := registry.tryAcquireUploadPermit(permit, uploadAdmissionLimits{Global: 2}); err != nil {
		t.Fatalf("acquire permit: %v", err)
	}
	registry.add(&transferRecord{ID: "record", PermitID: permit.ID, Type: "upload", Status: transferActive, DirID: "changed", Cancelable: true, cancel: func() { recordCalls++ }})
	if got := registry.cancelUploadsByDirIDs([]string{"changed"}); got != 1 {
		t.Fatalf("expected one cancellation callback, got %d", got)
	}
	if permitCalls != 0 || recordCalls != 1 || !registry.permits[permit.ID].canceling || registry.records["record"].Status != transferCanceling {
		t.Fatalf("active record must use only record cancellation: permitCalls=%d recordCalls=%d permit=%+v record=%+v", permitCalls, recordCalls, registry.permits[permit.ID], registry.records["record"])
	}
}

func TestCancelUploadsByDirIDsUsesPermitCallbackBetweenFiles(t *testing.T) {
	registry := newTransferRegistry()
	permitCalls := 0
	permit := &uploadPermit{ID: "between-files", DirID: "changed", ResourceFingerprint: "fingerprint", OwnerType: "session", OwnerID: "owner", cancel: func() { permitCalls++ }}
	if err := registry.tryAcquireUploadPermit(permit, uploadAdmissionLimits{Global: 2}); err != nil {
		t.Fatalf("acquire permit: %v", err)
	}
	if got := registry.cancelUploadsByDirIDs([]string{"changed"}); got != 1 {
		t.Fatalf("expected permit-only callback, got %d", got)
	}
	if permitCalls != 1 || !registry.permits[permit.ID].canceling {
		t.Fatalf("permit-only request was not canceled correctly: calls=%d permit=%+v", permitCalls, registry.permits[permit.ID])
	}
}

func permitRec(id, dirID, fingerprint, ownerType, ownerID string) *uploadPermit {
	return &uploadPermit{ID: id, DirID: dirID, ResourceFingerprint: fingerprint, OwnerType: ownerType, OwnerID: ownerID}
}
