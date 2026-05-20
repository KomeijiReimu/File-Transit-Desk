package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionRoleAndExpiryCleanup(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()

	expiresAt := time.Now().Add(time.Hour).UTC()
	if err := st.CreateSession("sid-admin", expiresAt, "admin", "root"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	sess, err := st.Session("sid-admin")
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if sess.Role != "admin" || sess.Name != "root" {
		t.Fatalf("unexpected session role/name: %q/%q", sess.Role, sess.Name)
	}
	if sess.ID == "sid-admin" {
		t.Fatalf("expected stored session id to be hashed, got plaintext id")
	}
	var rawCount int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, "sid-admin").Scan(&rawCount); err != nil {
		t.Fatalf("query raw session id: %v", err)
	}
	if rawCount != 0 {
		t.Fatalf("expected no plaintext session id in database")
	}
	if !st.SessionValid("sid-admin") {
		t.Fatalf("expected session to be valid")
	}

	if err := st.CreateSession("sid-expired", time.Now().Add(-time.Hour), "user", ""); err != nil {
		t.Fatalf("create expired session: %v", err)
	}
	if err := st.DeleteExpiredSessions(time.Now()); err != nil {
		t.Fatalf("delete expired sessions: %v", err)
	}
	if st.SessionValid("sid-expired") {
		t.Fatalf("expected expired session to be removed or invalid")
	}
	if !st.SessionValid("sid-admin") {
		t.Fatalf("expected unexpired session to remain valid")
	}
}

func TestSessionIdleAndDownloadLeaseLifecycle(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()

	now := time.Now().UTC()
	if err := st.CreateSessionWithIdle("sid-idle", now.Add(time.Hour), now.Add(-time.Minute), "user", ""); err != nil {
		t.Fatalf("create idle session: %v", err)
	}
	if st.SessionValid("sid-idle") {
		t.Fatalf("expected idle-expired session to be invalid")
	}
	if err := st.TouchSession("sid-idle", now, now.Add(time.Minute)); err != nil {
		t.Fatalf("touch session: %v", err)
	}
	if !st.SessionValid("sid-idle") {
		t.Fatalf("expected touched session to become valid")
	}
	if err := st.DeleteExpiredSessions(now.Add(2 * time.Minute)); err != nil {
		t.Fatalf("delete idle expired sessions: %v", err)
	}
	if st.SessionValid("sid-idle") {
		t.Fatalf("expected idle-expired session to be removed")
	}

	lease := &DownloadLease{
		Hash:       "lease-hash",
		Source:     "session",
		SessionID:  sql.NullString{String: "hashed-session", Valid: true},
		Role:       "user",
		DirID:      "default",
		Path:       "a.txt",
		FileSize:   10,
		FileMtime:  now,
		FileSHA256: sql.NullString{String: "abc123", Valid: true},
		ExpiresAt:  now.Add(time.Hour),
	}
	if err := st.CreateDownloadLease(lease); err != nil {
		t.Fatalf("create download lease: %v", err)
	}
	loaded, err := st.DownloadLeaseByHash("lease-hash")
	if err != nil {
		t.Fatalf("load download lease: %v", err)
	}
	if loaded.ID == 0 || loaded.Path != "a.txt" || loaded.FileSize != 10 || !loaded.SessionID.Valid || loaded.FileSHA256.String != "abc123" {
		t.Fatalf("unexpected loaded lease: %+v", loaded)
	}
	if err := st.TouchDownloadLease("lease-hash", now.Add(time.Minute)); err != nil {
		t.Fatalf("touch download lease: %v", err)
	}
	touched, err := st.DownloadLeaseByHash("lease-hash")
	if err != nil {
		t.Fatalf("reload touched lease: %v", err)
	}
	if !touched.LastUsedAt.Valid {
		t.Fatalf("expected last_used_at to be set")
	}
	if err := st.DeleteExpiredDownloadLeases(now.Add(2 * time.Hour)); err != nil {
		t.Fatalf("delete expired leases: %v", err)
	}
	if _, err := st.DownloadLeaseByHash("lease-hash"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected lease to be deleted, got %v", err)
	}
}

func TestTokenUploadByteQuota(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()

	token := &Token{Hash: "hash", Type: "upload", DirID: "default", Path: "", MaxUses: 0}
	if err := st.CreateToken(token); err != nil {
		t.Fatalf("create token: %v", err)
	}

	reserved, err := st.ReserveTokenUse("hash", "upload", time.Now(), 600, 1000)
	if err != nil {
		t.Fatalf("reserve token: %v", err)
	}
	if reserved.UploadedBytes != 600 || reserved.Uses != 1 {
		t.Fatalf("unexpected uploaded bytes/uses: %d/%d", reserved.UploadedBytes, reserved.Uses)
	}

	if _, err := st.ReserveTokenUse("hash", "upload", time.Now(), 500, 1000); !errors.Is(err, ErrTokenUploadLimitExceeded) {
		t.Fatalf("expected upload quota error, got %v", err)
	}

	if err := st.ReleaseTokenUse(reserved.ID, 600); err != nil {
		t.Fatalf("release token: %v", err)
	}
	released, err := st.TokenByHash("hash")
	if err != nil {
		t.Fatalf("reload token: %v", err)
	}
	if released.UploadedBytes != 0 || released.Uses != 0 {
		t.Fatalf("expected rollback to zero, got bytes=%d uses=%d", released.UploadedBytes, released.Uses)
	}
}
