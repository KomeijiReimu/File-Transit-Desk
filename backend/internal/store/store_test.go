package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionRoleAndExpiryCleanup(t *testing.T) {
	// 会话 ID 入库必须是哈希值，测试同时覆盖角色字段和过期清理逻辑。
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
	// 空闲会话与下载票据是长下载保护的核心，放在同一个生命周期测试里验证读写和清理。
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
		Hash:                "lease-hash",
		Source:              "session",
		SessionID:           sql.NullString{String: "hashed-session", Valid: true},
		Role:                "user",
		DirID:               "default",
		Path:                "a.txt",
		ResourceFingerprint: "resource-v1",
		FileSize:            10,
		FileMtime:           now,
		FileSHA256:          sql.NullString{String: "abc123", Valid: true},
		ExpiresAt:           now.Add(time.Hour),
	}
	if err := st.CreateDownloadLease(lease); err != nil {
		t.Fatalf("create download lease: %v", err)
	}
	loaded, err := st.DownloadLeaseByHash("lease-hash")
	if err != nil {
		t.Fatalf("load download lease: %v", err)
	}
	if loaded.ID == 0 || loaded.Path != "a.txt" || loaded.ResourceFingerprint != "resource-v1" || loaded.FileSize != 10 || !loaded.SessionID.Valid || loaded.FileSHA256.String != "abc123" {
		t.Fatalf("unexpected loaded lease: %+v", loaded)
	}
	if first, err := st.MarkDownloadLeaseFirstUsed("lease-hash", now.Add(time.Minute)); err != nil || !first {
		t.Fatalf("mark first use: first=%v err=%v", first, err)
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

func TestMigrateLegacyDatabaseAddsSessionAndLeaseColumns(t *testing.T) {
	// 手工构造旧 schema，确保真实用户旧库启动时会自动补新列和索引。
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	_, err = db.Exec(`
CREATE TABLE sessions(
  id TEXT PRIMARY KEY,
  expires_at DATETIME NOT NULL,
  created_at DATETIME NOT NULL,
  role TEXT NOT NULL DEFAULT 'user',
  name TEXT NOT NULL DEFAULT ''
);
CREATE TABLE tokens(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  token_hash TEXT NOT NULL UNIQUE,
  type TEXT NOT NULL,
  dir_id TEXT NOT NULL,
  path TEXT NOT NULL,
  expires_at DATETIME,
  max_uses INTEGER NOT NULL DEFAULT 0,
  uses INTEGER NOT NULL DEFAULT 0,
  revoked INTEGER NOT NULL DEFAULT 0,
  created_at DATETIME NOT NULL
);
CREATE TABLE download_leases(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  lease_hash TEXT NOT NULL UNIQUE,
  source TEXT NOT NULL,
  session_id TEXT,
  token_id INTEGER,
  role TEXT NOT NULL DEFAULT '',
  dir_id TEXT NOT NULL,
  path TEXT NOT NULL,
  file_size INTEGER NOT NULL,
  file_mtime DATETIME NOT NULL,
  expires_at DATETIME NOT NULL,
  created_at DATETIME NOT NULL,
  last_used_at DATETIME
);
CREATE TABLE upload_leases(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  lease_hash TEXT NOT NULL UNIQUE,
  session_id TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT '',
  dir_id TEXT NOT NULL,
  path TEXT NOT NULL,
  file_name TEXT NOT NULL,
  file_size INTEGER NOT NULL,
  expires_at DATETIME NOT NULL,
  created_at DATETIME NOT NULL,
  used_at DATETIME,
  client_ip TEXT NOT NULL DEFAULT ''
);
`)
	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("close legacy db: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}

	st, err := Open(dbPath, 100)
	if err != nil {
		t.Fatalf("migrate legacy store: %v", err)
	}
	defer st.DB.Close()

	for _, column := range []string{"idle_expires_at", "last_seen_at"} {
		if !columnExists(t, st.DB, "sessions", column) {
			t.Fatalf("expected sessions.%s to be added", column)
		}
	}
	if !columnExists(t, st.DB, "download_leases", "file_sha256") {
		t.Fatalf("expected download_leases.file_sha256 to be added")
	}
	if !columnExists(t, st.DB, "download_leases", "resource_fingerprint") {
		t.Fatalf("expected download_leases.resource_fingerprint to be added")
	}
	if !columnExists(t, st.DB, "tokens", "resource_fingerprint") {
		t.Fatalf("expected tokens.resource_fingerprint to be added")
	}
	if !columnExists(t, st.DB, "upload_leases", "resource_fingerprint") {
		t.Fatalf("expected upload_leases.resource_fingerprint to be added")
	}
	if _, err := st.DB.Exec(`CREATE INDEX IF NOT EXISTS idx_sessions_idle_expires_at ON sessions(idle_expires_at)`); err != nil {
		t.Fatalf("expected idle expiry index to be creatable after migration: %v", err)
	}
	rows, err := st.DB.Query(`SELECT name FROM sqlite_master WHERE type = 'index'`)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	defer rows.Close()
	indexes := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		indexes[name] = true
	}
	for _, name := range []string{"idx_sessions_expires_datetime", "idx_sessions_idle_expires_datetime", "idx_tokens_active_datetime", "idx_tokens_expires_datetime", "idx_download_leases_expires_datetime", "idx_download_leases_session_datetime", "idx_download_leases_token_datetime", "idx_upload_leases_outstanding_datetime_v2", "idx_upload_leases_session_datetime_v2", "idx_upload_leases_token_datetime_v2"} {
		if !indexes[name] {
			t.Fatalf("expected migrated expression index %s", name)
		}
	}
	now := time.Now()
	assertQueryUsesIndex(t, st.DB, "idx_sessions_expires_datetime", `SELECT id FROM sessions WHERE datetime(expires_at) <= datetime(?)`, now)
	assertQueryUsesIndex(t, st.DB, "idx_tokens_expires_datetime", `SELECT id FROM tokens WHERE expires_at IS NOT NULL AND datetime(expires_at) <= datetime(?)`, now)
	assertQueryUsesIndex(t, st.DB, "idx_download_leases_expires_datetime", `SELECT COUNT(*) FROM download_leases WHERE datetime(expires_at) > datetime(?)`, now)
	assertQueryUsesIndex(t, st.DB, "idx_upload_leases_outstanding_datetime_v2", `SELECT COUNT(*) FROM upload_leases WHERE used_at IS NULL AND datetime(expires_at) > datetime(?)`, now)
	assertQueryUsesIndex(t, st.DB, "idx_upload_leases_session_datetime_v2", `SELECT COUNT(*) FROM upload_leases WHERE source = ? AND session_id = ? AND used_at IS NULL AND datetime(expires_at) > datetime(?)`, "session", "owner", now)
	assertQueryUsesIndex(t, st.DB, "idx_upload_leases_token_datetime_v2", `SELECT COUNT(*) FROM upload_leases WHERE source = ? AND token_id = ? AND used_at IS NULL AND datetime(expires_at) > datetime(?)`, "public_token", int64(1), now)
}

func assertQueryUsesIndex(t *testing.T, db *sql.DB, indexName, query string, args ...any) {
	t.Helper()
	rows, err := db.Query(`EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatalf("explain %s: %v", indexName, err)
	}
	defer rows.Close()
	used := false
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan explain %s: %v", indexName, err)
		}
		if strings.Contains(detail, indexName) {
			used = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows %s: %v", indexName, err)
	}
	if !used {
		t.Fatalf("query did not use target index %s", indexName)
	}
}

func TestTokenUploadByteQuota(t *testing.T) {
	// 上传令牌容量使用预占策略，保存失败后必须能回滚次数和容量。
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()

	token := &Token{Hash: "hash", Type: "upload", DirID: "default", Path: "", ResourceFingerprint: "resource-v1", MaxUses: 0}
	if err := st.CreateToken(token); err != nil {
		t.Fatalf("create token: %v", err)
	}
	loadedToken, err := st.TokenByHash("hash")
	if err != nil || loadedToken.ResourceFingerprint != "resource-v1" {
		t.Fatalf("expected token fingerprint round trip, token=%+v err=%v", loadedToken, err)
	}
	loadedByID, err := st.TokenByID(token.ID)
	if err != nil || loadedByID.ResourceFingerprint != "resource-v1" {
		t.Fatalf("expected token fingerprint from id lookup, token=%+v err=%v", loadedByID, err)
	}
	tokens, err := st.Tokens()
	if err != nil || len(tokens) != 1 || tokens[0].ResourceFingerprint != "resource-v1" {
		t.Fatalf("expected token fingerprint from list, tokens=%+v err=%v", tokens, err)
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
	if err := st.AdjustTokenUploadedBytes(reserved.ID, 500, 1000); !errors.Is(err, ErrTokenUploadLimitExceeded) {
		t.Fatalf("expected adjustment over quota to fail, got %v", err)
	}
	if err := st.AdjustTokenUploadedBytes(reserved.ID, -100, 1000); err != nil {
		t.Fatalf("adjust token bytes down: %v", err)
	}
	adjusted, err := st.TokenByHash("hash")
	if err != nil {
		t.Fatalf("reload adjusted token: %v", err)
	}
	if adjusted.UploadedBytes != 500 || adjusted.Uses != 1 {
		t.Fatalf("expected byte adjustment without use rollback, got bytes=%d uses=%d", adjusted.UploadedBytes, adjusted.Uses)
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

func TestExpiredTokenCleanupRemovesDownloadLeasesAndSanitizesAudit(t *testing.T) {
	// 过期 token 清理和审计详情净化都属于长期运行维护能力，避免隐藏授权和脏数据累积。
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	now := time.Now().UTC()
	token := &Token{Hash: "expired-hash", Type: "download", DirID: "default", Path: "a.txt", MaxUses: 1, ExpiresAt: sql.NullTime{Time: now.Add(-time.Minute), Valid: true}}
	if err := st.CreateToken(token); err != nil {
		t.Fatalf("create token: %v", err)
	}
	lease := &DownloadLease{Hash: "lease-hash", Source: "public_token", TokenID: sql.NullInt64{Int64: token.ID, Valid: true}, DirID: "default", Path: "a.txt", FileSize: 1, FileMtime: now, FileSHA256: sql.NullString{String: "", Valid: true}, ExpiresAt: now.Add(time.Hour)}
	if err := st.CreateDownloadLease(lease); err != nil {
		t.Fatalf("create lease: %v", err)
	}
	if err := st.DeleteExpiredTokens(now); err != nil {
		t.Fatalf("delete expired tokens: %v", err)
	}
	if _, err := st.DownloadLeaseByHash("lease-hash"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected expired token lease to be deleted, got %v", err)
	}
	if err := st.Audit("test", "ip", "第一行\n第二行\x00"+strings.Repeat("长", 600)); err != nil {
		t.Fatalf("audit: %v", err)
	}
	logs, err := st.AuditLogs(1)
	if err != nil {
		t.Fatalf("audit logs: %v", err)
	}
	if len(logs) != 1 || len([]rune(logs[0].Detail)) > 501 || logs[0].Detail == "" {
		t.Fatalf("expected sanitized bounded audit detail, got %+v", logs)
	}
}

func TestDeleteUploadLeasesByDirID(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	now := time.Now()
	keep := &UploadLease{Hash: "keep", SessionID: "s", Role: "user", DirID: "keep", Path: "", FileName: "a.txt", FileSize: 1, ExpiresAt: now.Add(time.Hour)}
	drop := &UploadLease{Hash: "drop", SessionID: "s", Role: "user", DirID: "old", Path: "", FileName: "b.txt", FileSize: 1, ExpiresAt: now.Add(time.Hour)}
	if err := st.CreateUploadLease(keep); err != nil {
		t.Fatalf("create keep lease: %v", err)
	}
	if err := st.CreateUploadLease(drop); err != nil {
		t.Fatalf("create drop lease: %v", err)
	}
	if err := st.DeleteUploadLeasesByDirID("old"); err != nil {
		t.Fatalf("delete upload leases: %v", err)
	}
	if _, err := st.UploadLeaseByHash("drop"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected old dir upload lease deleted, got %v", err)
	}
	if _, err := st.UploadLeaseByHash("keep"); err != nil {
		t.Fatalf("expected other dir lease to remain: %v", err)
	}
}

func TestCreateTokenLimitedConcurrentAndInactiveExclusion(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			token := &Token{Hash: "limited-token-" + strconv.Itoa(i), Type: "download", DirID: "default"}
			err := st.CreateTokenLimited(token, 2)
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrActiveTokenLimitReached) {
				t.Errorf("unexpected create error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if successes.Load() != 2 {
		t.Fatalf("expected exactly two active tokens, got %d", successes.Load())
	}
	if err := st.Revoke("1"); err != nil {
		t.Fatalf("revoke token: %v", err)
	}
	expired := &Token{Hash: "expired-token", Type: "download", DirID: "default", ExpiresAt: sql.NullTime{Time: time.Now().Add(-time.Minute), Valid: true}}
	if err := st.CreateToken(expired); err != nil {
		t.Fatalf("create expired token: %v", err)
	}
	if err := st.CreateTokenLimited(&Token{Hash: "replacement-token", Type: "download", DirID: "default"}, 2); err != nil {
		t.Fatalf("revoked and expired tokens must not count: %v", err)
	}
}

func TestCreateTokenLimitedExcludesExhaustedTokens(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	exhausted := &Token{Hash: "one-use-token", Type: "download", DirID: "default", MaxUses: 1}
	if err := st.CreateTokenLimited(exhausted, 1); err != nil {
		t.Fatalf("create one-use token: %v", err)
	}
	if _, err := st.ReserveTokenUse(exhausted.Hash, "download", time.Now(), 0, 0); err != nil {
		t.Fatalf("exhaust token: %v", err)
	}
	if err := st.CreateTokenLimited(&Token{Hash: "replacement-after-exhaustion", Type: "download", DirID: "default"}, 1); err != nil {
		t.Fatalf("exhausted token must not count as active: %v", err)
	}
}

func TestCreateLeaseLimitedCountsBothKindsAndIgnoresExpiredUsed(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	now := time.Now()
	if err := st.CreateDownloadLease(&DownloadLease{Hash: "expired-download", Source: "session", SessionID: sql.NullString{String: "owner", Valid: true}, FileSHA256: sql.NullString{String: "", Valid: true}, ExpiresAt: now.Add(-time.Minute)}); err != nil {
		t.Fatalf("create expired download: %v", err)
	}
	used := &UploadLease{Hash: "used-upload", Source: "session", SessionID: "owner", ExpiresAt: now.Add(time.Hour)}
	if err := st.CreateUploadLease(used); err != nil {
		t.Fatalf("create upload: %v", err)
	}
	if _, err := st.ReserveUploadLease(used.Hash, now); err != nil {
		t.Fatalf("reserve upload: %v", err)
	}
	active := &DownloadLease{Hash: "active-download", Source: "session", SessionID: sql.NullString{String: "owner", Valid: true}, ExpiresAt: now.Add(time.Hour)}
	if err := st.CreateDownloadLeaseLimited(active, 2, 1); err != nil {
		t.Fatalf("expired and used leases must not count: %v", err)
	}
	if err := st.CreateUploadLeaseLimited(&UploadLease{Hash: "owner-overflow", Source: "session", SessionID: "owner", ExpiresAt: now.Add(time.Hour)}, 2, 1); !errors.Is(err, ErrOutstandingLeaseLimit) {
		t.Fatalf("expected owner limit across lease kinds, got %v", err)
	}
	if err := st.CreateUploadLeaseLimited(&UploadLease{Hash: "other-owner", Source: "session", SessionID: "other", ExpiresAt: now.Add(time.Hour)}, 2, 2); err != nil {
		t.Fatalf("create second total lease: %v", err)
	}
	if err := st.CreateDownloadLeaseLimited(&DownloadLease{Hash: "total-overflow", Source: "session", SessionID: sql.NullString{String: "third", Valid: true}, ExpiresAt: now.Add(time.Hour)}, 2, 2); !errors.Is(err, ErrOutstandingLeaseLimit) {
		t.Fatalf("expected total outstanding limit, got %v", err)
	}
}

func TestSessionDeletionDoesNotRemoveExistingLimitedLease(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	if err := st.CreateSession("session-owner", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	lease := &DownloadLease{Hash: "surviving-lease", Source: "session", SessionID: sql.NullString{String: "session-owner", Valid: true}, ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.CreateDownloadLeaseLimited(lease, 10, 10); err != nil {
		t.Fatalf("create limited lease: %v", err)
	}
	if err := st.DeleteSession("session-owner"); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := st.DownloadLeaseByHash(lease.Hash); err != nil {
		t.Fatalf("existing lease must survive session deletion: %v", err)
	}
}

func TestMarkDownloadLeaseFirstUsedIsConditionalAndConcurrent(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	lease := &DownloadLease{Hash: "first-use-lease", Source: "session", DirID: "default", Path: "file.txt", FileSHA256: sql.NullString{String: "", Valid: true}, ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.CreateDownloadLease(lease); err != nil {
		t.Fatalf("create lease: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	var firstCount atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			first, err := st.MarkDownloadLeaseFirstUsed(lease.Hash, now)
			if err != nil {
				t.Errorf("mark first use: %v", err)
				return
			}
			if first {
				firstCount.Add(1)
			}
		}()
	}
	wg.Wait()
	if firstCount.Load() != 1 {
		t.Fatalf("expected exactly one first marker, got %d", firstCount.Load())
	}
	first, err := st.MarkDownloadLeaseFirstUsed(lease.Hash, now.Add(time.Hour))
	if err != nil || first {
		t.Fatalf("later use unexpectedly updated first-use timestamp: first=%v err=%v", first, err)
	}
	loaded, err := st.DownloadLeaseByHash(lease.Hash)
	if err != nil || !loaded.LastUsedAt.Valid || !loaded.LastUsedAt.Time.Equal(now) {
		t.Fatalf("unexpected persisted first-use timestamp: lease=%+v err=%v", loaded, err)
	}
}

func TestAuditBatchedPruneRetainsExactBoundary(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), 3)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	st.SetAuditPolicy(3, 2)
	for i := 1; i <= 4; i++ {
		if err := st.Audit("event", "", strconv.Itoa(i)); err != nil {
			t.Fatalf("audit %d: %v", i, err)
		}
		var count int
		if err := st.DB.QueryRow(`SELECT COUNT(*) FROM audit_logs`).Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if i == 3 && count != 3 {
			t.Fatalf("audit unexpectedly pruned on a non-trigger write: %d", count)
		}
	}
	logs, err := st.AuditLogs(10)
	if err != nil || len(logs) != 3 || logs[2].Detail != "2" {
		t.Fatalf("unexpected exact retained boundary: logs=%+v err=%v", logs, err)
	}
	if err := st.PruneAudit(); err != nil {
		t.Fatalf("explicit prune: %v", err)
	}
	logs, _ = st.AuditLogs(10)
	if len(logs) != 3 {
		t.Fatalf("exact retain boundary should not delete rows: %d", len(logs))
	}
}

func TestAuditLogsPageFilteredGlobalKeywordStatusAndEscaping(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), 1000)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	entries := []struct {
		action string
		ip     string
		detail string
	}{
		{"login_success", "192.0.2.1", "ordinary"},
		{"token_create", "192.0.2.2", "global-needle"},
		{"login_failed", "192.0.2.3", "bad credentials"},
		{"file_picker_denied", "192.0.2.4", "policy"},
		{"download_lease_file_changed", "192.0.2.5", "changed"},
		{"upload", "192.0.2.6", "literal 100% done"},
		{"download", "192.0.2.7", "under_score"},
		{"config_view", `2001:db8::1`, `back\slash`},
		{"config_changed", "192.0.2.8", "successful configuration change"},
		{"capacity_increased", "192.0.2.9", "successful capacity increase"},
		{"file_changed_successfully", "192.0.2.10", "successful file replacement"},
		{"token_download_failed", "192.0.2.11", "historical failure action"},
	}
	for _, entry := range entries {
		if err := st.Audit(entry.action, entry.ip, entry.detail); err != nil {
			t.Fatalf("audit seed: %v", err)
		}
	}
	logs, total, err := st.AuditLogsPageFiltered(2, 0, AuditLogFilter{Keyword: "global-needle", Status: "all"})
	if err != nil || total != 1 || len(logs) != 1 || logs[0].Action != "token_create" {
		t.Fatalf("global keyword filter logs=%+v total=%d err=%v", logs, total, err)
	}
	for _, tc := range []struct {
		keyword string
		action  string
	}{
		{"login_success", "login_success"},
		{"2001:db8", "config_view"},
	} {
		matched, count, err := st.AuditLogsPageFiltered(10, 0, AuditLogFilter{Keyword: tc.keyword, Status: "all"})
		if err != nil || count != 1 || len(matched) != 1 || matched[0].Action != tc.action {
			t.Fatalf("action/IP keyword %q logs=%+v count=%d err=%v", tc.keyword, matched, count, err)
		}
	}
	failed, failedTotal, err := st.AuditLogsPageFiltered(20, 0, AuditLogFilter{Status: "failed"})
	if err != nil || failedTotal != 4 || len(failed) != failedTotal {
		t.Fatalf("failed filter logs=%+v total=%d err=%v", failed, failedTotal, err)
	}
	for _, entry := range failed {
		if !IsAuditFailureAction(entry.Action) {
			t.Fatalf("failed filter included ok action: %+v", entry)
		}
	}
	for _, action := range []string{"config_changed", "capacity_increased", "file_changed_successfully", "unknown_action"} {
		if IsAuditFailureAction(action) {
			t.Fatalf("successful/unknown action misclassified as failed: %s", action)
		}
	}
	for _, action := range []string{"download_lease_file_changed", "download_lease_resource_changed", "upload_lease_resource_changed", "token_download_failed"} {
		if !IsAuditFailureAction(action) {
			t.Fatalf("real resource/file change failure not classified: %s", action)
		}
	}
	okLogs, okTotal, err := st.AuditLogsPageFiltered(2, 0, AuditLogFilter{Status: "ok"})
	if err != nil || okTotal != len(entries)-failedTotal || len(okLogs) != 2 {
		t.Fatalf("ok filter page logs=%+v total=%d err=%v", okLogs, okTotal, err)
	}
	allLogs, allTotal, err := st.AuditLogsPageFiltered(50, 0, AuditLogFilter{Status: "all"})
	if err != nil || len(allLogs) != len(entries) || allTotal != len(entries) || failedTotal+okTotal != allTotal {
		t.Fatalf("failed+ok did not partition all rows: all=%d failed=%d ok=%d logs=%d err=%v", allTotal, failedTotal, okTotal, len(allLogs), err)
	}
	okPage2, page2Total, err := st.AuditLogsPageFiltered(2, 2, AuditLogFilter{Status: "ok"})
	if err != nil || page2Total != okTotal || len(okPage2) != 2 {
		t.Fatalf("ok page2 logs=%+v total=%d err=%v", okPage2, page2Total, err)
	}
	for _, tc := range []struct {
		keyword string
		detail  string
	}{
		{"%", "literal 100% done"},
		{"under_", "under_score"},
		{`\`, `back\slash`},
	} {
		filtered, count, err := st.AuditLogsPageFiltered(10, 0, AuditLogFilter{Keyword: tc.keyword, Status: "all"})
		if err != nil || count != 1 || len(filtered) != 1 || filtered[0].Detail != tc.detail {
			t.Fatalf("escaped keyword %q logs=%+v count=%d err=%v", tc.keyword, filtered, count, err)
		}
	}
	injected, count, err := st.AuditLogsPageFiltered(10, 0, AuditLogFilter{Keyword: `%' OR 1=1 --`, Status: "all"})
	if err != nil || count != 0 || len(injected) != 0 {
		t.Fatalf("SQL injection-like keyword matched rows: logs=%+v count=%d err=%v", injected, count, err)
	}
	if _, _, err := st.AuditLogsPageFiltered(10, 0, AuditLogFilter{Status: "not-a-status"}); !errors.Is(err, ErrInvalidAuditFilter) {
		t.Fatalf("expected store-level invalid status defense, got %v", err)
	}
}

func TestAuditConcurrentWritesAndExplicitPrune(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), 10)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	st.SetAuditPolicy(10, 0)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := st.Audit("concurrent", "", strconv.Itoa(i)); err != nil {
				t.Errorf("audit: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if err := st.PruneAudit(); err != nil {
		t.Fatalf("prune: %v", err)
	}
	logs, err := st.AuditLogs(100)
	if err != nil || len(logs) != 10 {
		t.Fatalf("expected exactly ten retained logs: len=%d err=%v", len(logs), err)
	}
}

func TestAuditPruneFailureReturnsErrorAfterInsertPersists(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), 1)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	st.SetAuditPolicy(1, 1)
	if err := st.Audit("first", "", ""); err != nil {
		t.Fatalf("first audit: %v", err)
	}
	if _, err := st.DB.Exec(`CREATE TRIGGER block_audit_delete BEFORE DELETE ON audit_logs BEGIN SELECT RAISE(FAIL, 'blocked prune'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if err := st.Audit("second", "", "persisted before failed prune"); !errors.Is(err, ErrAuditMaintenance) {
		t.Fatalf("expected typed prune failure, got %v", err)
	}
	var count int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE action = 'second'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("triggering audit insert did not persist: count=%d err=%v", count, err)
	}
}

func TestCreateLeaseLimitedConcurrentDoesNotExceedTotal(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lease := &DownloadLease{Hash: "concurrent-lease-" + strconv.Itoa(i), Source: "session", SessionID: sql.NullString{String: "owner-" + strconv.Itoa(i), Valid: true}, ExpiresAt: time.Now().Add(time.Hour)}
			err := st.CreateDownloadLeaseLimited(lease, 3, 3)
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrOutstandingLeaseLimit) {
				t.Errorf("unexpected lease create error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if successes.Load() != 3 {
		t.Fatalf("expected exactly three outstanding leases, got %d", successes.Load())
	}
}

func TestCreateLeaseLimitedUsesPublicTokenIDAsOwner(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	tokenID := sql.NullInt64{Int64: 42, Valid: true}
	first := &DownloadLease{Hash: "public-download", Source: "public_token", TokenID: tokenID, ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.CreateDownloadLeaseLimited(first, 10, 1); err != nil {
		t.Fatalf("create public download: %v", err)
	}
	second := &UploadLease{Hash: "public-upload", Source: "public_token", TokenID: tokenID, ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.CreateUploadLeaseLimited(second, 10, 1); !errors.Is(err, ErrOutstandingLeaseLimit) {
		t.Fatalf("expected shared public token owner limit, got %v", err)
	}
}

func TestPingContext(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := st.PingContext(ctx); err != nil {
		t.Fatalf("ping open store: %v", err)
	}
	if err := st.DB.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	if err := st.PingContext(ctx); err == nil {
		t.Fatalf("expected closed store ping to fail")
	}
}

func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("table info %s: %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan table info %s: %v", table, err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table info %s: %v", table, err)
	}
	return false
}
