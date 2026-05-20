package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var ErrTokenNotUsable = errors.New("token is expired, revoked, exhausted, or wrong type")
var ErrTokenUploadLimitExceeded = errors.New("token upload quota exceeded")

type Store struct {
	DB     *sql.DB
	Retain int
}

type Token struct {
	ID            int64
	Hash          string
	Type          string
	DirID         string
	Path          string
	ExpiresAt     sql.NullTime
	MaxUses       int
	Uses          int
	UploadedBytes int64
	Revoked       bool
	CreatedAt     time.Time
}

type Session struct {
	ID        string
	ExpiresAt time.Time
	CreatedAt time.Time
	Role      string
	Name      string
}

type AuditLog struct {
	ID        int64
	Action    string
	IP        string
	Detail    string
	CreatedAt time.Time
}

func Open(path string, retain int) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{DB: db, Retain: retain}
	if err := s.Migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Migrate() error {
	_, err := s.DB.Exec(`
CREATE TABLE IF NOT EXISTS sessions(
  id TEXT PRIMARY KEY,
  expires_at DATETIME NOT NULL,
  created_at DATETIME NOT NULL,
  role TEXT NOT NULL DEFAULT 'user',
  name TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS tokens(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  token_hash TEXT NOT NULL UNIQUE,
  type TEXT NOT NULL,
  dir_id TEXT NOT NULL,
  path TEXT NOT NULL,
  expires_at DATETIME,
  max_uses INTEGER NOT NULL DEFAULT 0,
  uses INTEGER NOT NULL DEFAULT 0,
  uploaded_bytes INTEGER NOT NULL DEFAULT 0,
  revoked INTEGER NOT NULL DEFAULT 0,
  created_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tokens_hash ON tokens(token_hash);
CREATE INDEX IF NOT EXISTS idx_tokens_expires_at ON tokens(expires_at);

CREATE TABLE IF NOT EXISTS audit_logs(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  action TEXT NOT NULL,
  ip TEXT,
  detail TEXT,
  created_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_logs_created_at ON audit_logs(created_at);
`)
	if err != nil {
		return err
	}
	if err := s.addColumnIfMissing("sessions", "role", "TEXT NOT NULL DEFAULT 'user'"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("sessions", "name", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	return s.addColumnIfMissing("tokens", "uploaded_bytes", "INTEGER NOT NULL DEFAULT 0")
}

func (s *Store) addColumnIfMissing(table, column, definition string) error {
	rows, err := s.DB.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.DB.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition)
	return err
}

func (s *Store) CreateSession(id string, expiresAt time.Time, role, name string) error {
	if role == "" {
		role = "user"
	}
	storedID := hashSessionID(id)
	_, err := s.DB.Exec(
		`INSERT INTO sessions(id, expires_at, created_at, role, name) VALUES(?, ?, ?, ?, ?)`,
		storedID,
		expiresAt,
		time.Now(),
		role,
		name,
	)
	return err
}

func (s *Store) Session(id string) (Session, error) {
	var sess Session
	err := s.DB.QueryRow(`SELECT id, expires_at, created_at, role, name FROM sessions WHERE id = ?`, hashSessionID(id)).Scan(
		&sess.ID, &sess.ExpiresAt, &sess.CreatedAt, &sess.Role, &sess.Name,
	)
	return sess, err
}

func (s *Store) SessionValid(id string) bool {
	sess, err := s.Session(id)
	return err == nil && time.Now().Before(sess.ExpiresAt)
}

func (s *Store) DeleteSession(id string) error {
	_, err := s.DB.Exec(`DELETE FROM sessions WHERE id = ?`, hashSessionID(id))
	return err
}

func hashSessionID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

func (s *Store) DeleteExpiredSessions(now time.Time) error {
	_, err := s.DB.Exec(`DELETE FROM sessions WHERE datetime(expires_at) <= datetime(?)`, now)
	return err
}

func (s *Store) DeleteExpiredTokens(now time.Time) error {
	_, err := s.DB.Exec(`DELETE FROM tokens WHERE expires_at IS NOT NULL AND datetime(expires_at) <= datetime(?)`, now)
	return err
}

func (s *Store) Audit(action, ip, detail string) error {
	_, err := s.DB.Exec(
		`INSERT INTO audit_logs(action, ip, detail, created_at) VALUES(?, ?, ?, ?)`,
		action,
		ip,
		detail,
		time.Now(),
	)
	if err != nil {
		return err
	}
	if s.Retain > 0 {
		_, err = s.DB.Exec(
			`DELETE FROM audit_logs WHERE id NOT IN (SELECT id FROM audit_logs ORDER BY id DESC LIMIT ?)`,
			s.Retain,
		)
	}
	return err
}

func (s *Store) CreateToken(t *Token) error {
	res, err := s.DB.Exec(
		`INSERT INTO tokens(token_hash, type, dir_id, path, expires_at, max_uses, created_at) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		t.Hash,
		t.Type,
		t.DirID,
		t.Path,
		t.ExpiresAt,
		t.MaxUses,
		time.Now(),
	)
	if err != nil {
		return err
	}
	t.ID, err = res.LastInsertId()
	return err
}

func (s *Store) Tokens() ([]Token, error) {
	rows, err := s.DB.Query(`SELECT id, token_hash, type, dir_id, path, expires_at, max_uses, uses, uploaded_bytes, revoked, created_at FROM tokens ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Token
	for rows.Next() {
		var t Token
		if err := scanToken(rows, &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) TokenByHash(hash string) (Token, error) {
	var t Token
	err := s.DB.QueryRow(`SELECT id, token_hash, type, dir_id, path, expires_at, max_uses, uses, uploaded_bytes, revoked, created_at FROM tokens WHERE token_hash = ?`, hash).Scan(
		&t.ID,
		&t.Hash,
		&t.Type,
		&t.DirID,
		&t.Path,
		&t.ExpiresAt,
		&t.MaxUses,
		&t.Uses,
		&t.UploadedBytes,
		&t.Revoked,
		&t.CreatedAt,
	)
	return t, err
}

func (s *Store) ReserveTokenUse(hash, tokenType string, now time.Time, uploadBytes, uploadMaxBytes int64) (Token, error) {
	res, err := s.DB.Exec(`
UPDATE tokens
SET uses = uses + 1,
    uploaded_bytes = uploaded_bytes + ?
WHERE token_hash = ?
  AND type = ?
  AND revoked = 0
  AND (expires_at IS NULL OR datetime(expires_at) > datetime(?))
  AND (max_uses <= 0 OR uses < max_uses)
  AND (? <= 0 OR uploaded_bytes + ? <= ?)
`, uploadBytes, hash, tokenType, now, uploadMaxBytes, uploadBytes, uploadMaxBytes)
	if err != nil {
		return Token{}, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Token{}, err
	}
	if affected != 1 {
		if uploadBytes > 0 && uploadMaxBytes > 0 {
			if t, loadErr := s.TokenByHash(hash); loadErr == nil && t.Type == tokenType && t.UploadedBytes+uploadBytes > uploadMaxBytes {
				return Token{}, ErrTokenUploadLimitExceeded
			}
		}
		return Token{}, ErrTokenNotUsable
	}
	return s.TokenByHash(hash)
}

func (s *Store) ReleaseTokenUse(id int64, uploadBytes int64) error {
	_, err := s.DB.Exec(`UPDATE tokens SET uses = CASE WHEN uses > 0 THEN uses - 1 ELSE 0 END, uploaded_bytes = CASE WHEN uploaded_bytes >= ? THEN uploaded_bytes - ? ELSE 0 END WHERE id = ?`, uploadBytes, uploadBytes, id)
	return err
}

func (s *Store) Revoke(id string) error {
	_, err := s.DB.Exec(`UPDATE tokens SET revoked = 1 WHERE id = ?`, id)
	return err
}

func (s *Store) DeleteToken(id string) error {
	res, err := s.DB.Exec(`DELETE FROM tokens WHERE id = ?`, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) AuditLogs(limit int) ([]AuditLog, error) {
	rows, err := s.DB.Query(`SELECT id, action, ip, detail, created_at FROM audit_logs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AuditLog
	for rows.Next() {
		var log AuditLog
		if err := rows.Scan(&log.ID, &log.Action, &log.IP, &log.Detail, &log.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, log)
	}
	return out, rows.Err()
}

type tokenScanner interface {
	Scan(dest ...any) error
}

func scanToken(row tokenScanner, t *Token) error {
	return row.Scan(
		&t.ID,
		&t.Hash,
		&t.Type,
		&t.DirID,
		&t.Path,
		&t.ExpiresAt,
		&t.MaxUses,
		&t.Uses,
		&t.UploadedBytes,
		&t.Revoked,
		&t.CreatedAt,
	)
}
