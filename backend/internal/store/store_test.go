package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
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
	if _, err := st.DB.Exec(`CREATE INDEX IF NOT EXISTS idx_sessions_idle_expires_at ON sessions(idle_expires_at)`); err != nil {
		t.Fatalf("expected idle expiry index to be creatable after migration: %v", err)
	}
}

func TestTokenUploadByteQuota(t *testing.T) {
	// 上传令牌容量使用预占策略，保存失败后必须能回滚次数和容量。
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
