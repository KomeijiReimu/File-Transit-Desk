package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

var ErrAccountChanged = errors.New("账号状态已变化，请重新登录")
var ErrUsernameTaken = errors.New("用户名已被使用")
var ErrLeaseSessionInvalid = errors.New("租约签发会话无效或已过期")

type AccountUser struct {
	ID           int64     `json:"id"`
	Username     string    `json:"username"`
	DisplayName  string    `json:"displayName"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	PasswordHash string    `json:"-"`
	AuthVersion  int64     `json:"-"`
}

func (s *Store) migrateUsers(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS users (
id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL COLLATE NOCASE UNIQUE,
display_name TEXT NOT NULL, status TEXT NOT NULL, password_hash TEXT NOT NULL,
auth_version INTEGER NOT NULL DEFAULT 1, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL)`)
	if err != nil {
		return err
	}
	for _, table := range []string{"sessions", "download_leases", "upload_leases"} {
		if err := s.addColumnIfMissing(tx, table, "user_id", "INTEGER"); err != nil {
			return err
		}
		if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_` + table + `_user_id ON ` + table + `(user_id)`); err != nil {
			return err
		}
	}
	if err := s.addColumnIfMissing(tx, "sessions", "auth_version", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	return s.addColumnIfMissing(tx, "sessions", "auth_method", "TEXT NOT NULL DEFAULT 'legacy'")
}

const accountColumns = `id, username, display_name, status, created_at, updated_at, password_hash, auth_version`

func scanAccount(row tokenScanner) (AccountUser, error) {
	var u AccountUser
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Status, &u.CreatedAt, &u.UpdatedAt, &u.PasswordHash, &u.AuthVersion)
	return u, err
}

func (s *Store) AccountByUsername(username string) (AccountUser, error) {
	return scanAccount(s.DB.QueryRow(`SELECT `+accountColumns+` FROM users WHERE username = ?`, username))
}

func (s *Store) Account(id int64) (AccountUser, error) {
	return scanAccount(s.DB.QueryRow(`SELECT `+accountColumns+` FROM users WHERE id = ?`, id))
}

func (s *Store) CreateAccount(username, name, hash, status string) (AccountUser, error) {
	now := time.Now().UTC()
	res, err := s.DB.Exec(`INSERT INTO users(username,display_name,password_hash,status,created_at,updated_at) VALUES(?,?,?,?,?,?)`, username, name, hash, status, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			return AccountUser{}, ErrUsernameTaken
		}
		return AccountUser{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return AccountUser{}, err
	}
	return s.Account(id)
}

func (s *Store) Accounts(query, status string, page, size int) ([]AccountUser, int, error) {
	where := ` WHERE (? = '' OR instr(lower(username), lower(?)) > 0 OR instr(lower(display_name), lower(?)) > 0) AND (? = '' OR status = ?)`
	args := []any{query, query, query, status, status}
	var total int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM users`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.DB.Query(`SELECT `+accountColumns+` FROM users`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, append(args, size, (page-1)*size)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]AccountUser, 0)
	for rows.Next() {
		u, err := scanAccount(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, u)
	}
	return out, total, rows.Err()
}

// UpdateAccount performs credential/status mutation and revocation in one transaction.
// expectedVersion guards password verification done outside the single-connection transaction.
func (s *Store) UpdateAccount(id int64, name, status *string, hash string, expectedVersion *int64) (AccountUser, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return AccountUser{}, err
	}
	defer tx.Rollback()
	u, err := scanAccount(tx.QueryRow(`SELECT `+accountColumns+` FROM users WHERE id = ?`, id))
	if err != nil {
		return u, err
	}
	if u.Status == "deleted" || expectedVersion != nil && (u.AuthVersion != *expectedVersion || u.Status != "active") {
		return u, ErrAccountChanged
	}
	revoke := hash != "" || status != nil && *status != u.Status && *status != "active"
	if name != nil {
		u.DisplayName = *name
	}
	if status != nil {
		u.Status = *status
	}
	if hash != "" {
		u.PasswordHash = hash
	}
	if u.Status == "deleted" {
		u.PasswordHash = ""
	}
	if revoke {
		u.AuthVersion++
	}
	u.UpdatedAt = time.Now().UTC()
	if _, err = tx.Exec(`UPDATE users SET display_name=?,status=?,password_hash=?,auth_version=?,updated_at=? WHERE id=?`, u.DisplayName, u.Status, u.PasswordHash, u.AuthVersion, u.UpdatedAt, id); err != nil {
		return u, err
	}
	if revoke {
		for _, q := range []string{`DELETE FROM sessions WHERE user_id=?`, `DELETE FROM download_leases WHERE user_id=?`, `DELETE FROM upload_leases WHERE user_id=? AND used_at IS NULL`} {
			if _, err = tx.Exec(q, id); err != nil {
				return u, err
			}
		}
	}
	return u, tx.Commit()
}

func (s *Store) CreateAccountSession(id string, expires, idle time.Time, u AccountUser) error {
	now := time.Now()
	res, err := s.DB.Exec(`INSERT INTO sessions(id,expires_at,idle_expires_at,last_seen_at,created_at,role,name,user_id,auth_version,auth_method)
SELECT ?,?,?,?,?, 'user',display_name,id,auth_version,'account' FROM users WHERE id=? AND status='active' AND auth_version=?`, hashSessionID(id), expires, idle, now, now, u.ID, u.AuthVersion)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return ErrAccountChanged
	}
	return err
}

// sessionLeaseOwner uses tx exclusively: Store has exactly one SQLite connection.
func sessionLeaseOwner(tx *sql.Tx, source, sessionID string) (sql.NullInt64, error) {
	var id sql.NullInt64
	if source != "session" {
		return id, nil
	}
	now := time.Now()
	err := tx.QueryRow(`SELECT s.user_id FROM sessions s LEFT JOIN users u ON s.user_id=u.id
WHERE s.id=? AND datetime(s.expires_at)>datetime(?) AND datetime(s.idle_expires_at)>datetime(?)
AND (s.user_id IS NULL OR (u.status='active' AND u.auth_version=s.auth_version))`, sessionID, now, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return id, ErrLeaseSessionInvalid
	}
	return id, err
}

func (s *Store) AdmitDownloadLease(hash string, now time.Time) error {
	var id int64
	return s.DB.QueryRow(`SELECT l.id FROM download_leases l LEFT JOIN users u ON l.user_id=u.id WHERE l.lease_hash=? AND datetime(l.expires_at)>datetime(?) AND (l.user_id IS NULL OR u.status='active')`, hash, now).Scan(&id)
}
