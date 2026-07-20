package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	ChatChangeCreate   = "create"
	ChatChangeWithdraw = "withdraw"
	ChatChangeDelete   = "delete"

	ChatStatusActive    = "active"
	ChatStatusWithdrawn = "withdrawn"
	ChatStatusDeleted   = "deleted"
)

var (
	ErrChatMessageNotFound     = errors.New("chat message not found")
	ErrChatWithdrawForbidden   = errors.New("chat message withdraw forbidden")
	ErrChatWithdrawExpired     = errors.New("chat message withdraw window expired")
	ErrChatStateConflict       = errors.New("chat message state conflict")
	ErrChatCursorResetRequired = errors.New("chat cursor reset required")
)

type ChatMessage struct {
	ID          int64
	AuthorKey   string
	AuthorTag   string
	AuthorRole  string
	SourceIP    string
	Body        sql.NullString
	CreatedAt   time.Time
	WithdrawnAt sql.NullTime
	DeletedAt   sql.NullTime
	DeletedBy   sql.NullString
}

func (message ChatMessage) Status() string {
	if message.DeletedAt.Valid {
		return ChatStatusDeleted
	}
	if message.WithdrawnAt.Valid {
		return ChatStatusWithdrawn
	}
	return ChatStatusActive
}

type ChatCreateInput struct {
	AuthorKey  string
	AuthorTag  string
	AuthorRole string
	SourceIP   string
	Body       string
	CreatedAt  time.Time
}

type ChatChange struct {
	Seq       int64
	MessageID int64
	Kind      string
	CreatedAt time.Time
	Message   ChatMessage
}

type ChatSyncState struct {
	Generation      int64
	LatestChangeSeq int64
}

type chatMutationState struct {
	gate   sync.Mutex
	active int
}

func (s *Store) migrateChat() error {
	_, err := s.DB.Exec(`
CREATE TABLE IF NOT EXISTS chat_messages(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  author_key TEXT NOT NULL,
  author_tag TEXT NOT NULL,
  author_role TEXT NOT NULL,
  source_ip TEXT NOT NULL DEFAULT '',
  body TEXT,
  created_at DATETIME NOT NULL,
  withdrawn_at DATETIME,
  deleted_at DATETIME,
  deleted_by TEXT
);
CREATE INDEX IF NOT EXISTS idx_chat_messages_created_id ON chat_messages(created_at, id);
CREATE INDEX IF NOT EXISTS idx_chat_messages_author_active ON chat_messages(author_key, deleted_at, withdrawn_at, id);

CREATE TABLE IF NOT EXISTS chat_changes(
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  message_id INTEGER NOT NULL,
  kind TEXT NOT NULL,
  created_at DATETIME NOT NULL,
  FOREIGN KEY(message_id) REFERENCES chat_messages(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_chat_changes_message ON chat_changes(message_id, seq);

CREATE TABLE IF NOT EXISTS chat_sync_metadata(
  singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
  generation INTEGER NOT NULL CHECK(generation > 0)
);
INSERT OR IGNORE INTO chat_sync_metadata(singleton, generation) VALUES(1, 1);
`)
	return err
}

func (s *Store) CreateChatMessage(input ChatCreateInput) (ChatMessage, int64, error) {
	input.AuthorKey = strings.TrimSpace(input.AuthorKey)
	input.AuthorTag = strings.TrimSpace(input.AuthorTag)
	input.AuthorRole = strings.TrimSpace(input.AuthorRole)
	if input.AuthorKey == "" || input.AuthorTag == "" || input.AuthorRole != "user" && input.AuthorRole != "admin" || input.Body == "" {
		return ChatMessage{}, 0, errors.New("invalid chat message input")
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now()
	}
	input.CreatedAt = input.CreatedAt.UTC()
	tx, err := s.DB.Begin()
	if err != nil {
		return ChatMessage{}, 0, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`
INSERT INTO chat_messages(author_key, author_tag, author_role, source_ip, body, created_at)
VALUES(?, ?, ?, ?, ?, ?)`, input.AuthorKey, input.AuthorTag, input.AuthorRole, input.SourceIP, input.Body, input.CreatedAt)
	if err != nil {
		return ChatMessage{}, 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ChatMessage{}, 0, err
	}
	seq, err := insertChatChangeTx(tx, id, ChatChangeCreate, input.CreatedAt)
	if err != nil {
		return ChatMessage{}, 0, err
	}
	if err := tx.Commit(); err != nil {
		return ChatMessage{}, 0, err
	}
	return ChatMessage{
		ID:         id,
		AuthorKey:  input.AuthorKey,
		AuthorTag:  input.AuthorTag,
		AuthorRole: input.AuthorRole,
		SourceIP:   input.SourceIP,
		Body:       sql.NullString{String: input.Body, Valid: true},
		CreatedAt:  input.CreatedAt,
	}, seq, nil
}

func (s *Store) ChatMessage(id int64) (ChatMessage, error) {
	return scanChatMessage(s.DB.QueryRow(chatMessageSelect+` WHERE id = ?`, id))
}

func (s *Store) ChatMessages(beforeID int64, limit int) ([]ChatMessage, bool, error) {
	messages, hasMore, _, err := s.ChatMessagesPage(beforeID, limit)
	return messages, hasMore, err
}

func (s *Store) ChatMessagesPage(beforeID int64, limit int) ([]ChatMessage, bool, ChatSyncState, error) {
	if beforeID < 0 || limit < 1 {
		return nil, false, ChatSyncState{}, errors.New("invalid chat pagination")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, false, ChatSyncState{}, err
	}
	defer tx.Rollback()
	syncState, err := chatSyncState(tx)
	if err != nil {
		return nil, false, ChatSyncState{}, err
	}
	rows, err := tx.Query(chatMessageSelect+`
WHERE (? = 0 OR id < ?)
ORDER BY id DESC
LIMIT ?`, beforeID, beforeID, limit+1)
	if err != nil {
		return nil, false, ChatSyncState{}, err
	}
	defer rows.Close()
	messages := make([]ChatMessage, 0, limit+1)
	for rows.Next() {
		message, err := scanChatMessage(rows)
		if err != nil {
			return nil, false, ChatSyncState{}, err
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, false, ChatSyncState{}, err
	}
	if err := rows.Close(); err != nil {
		return nil, false, ChatSyncState{}, err
	}
	hasMore := len(messages) > limit
	if hasMore {
		messages = messages[:limit]
	}
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
	if err := tx.Commit(); err != nil {
		return nil, false, ChatSyncState{}, err
	}
	return messages, hasMore, syncState, nil
}

func (s *Store) ChatChanges(afterSeq, generation int64, limit int) ([]ChatChange, bool, error) {
	changes, hasMore, _, err := s.ChatChangesPage(afterSeq, generation, limit)
	return changes, hasMore, err
}

func (s *Store) ChatChangesPage(afterSeq, generation int64, limit int) ([]ChatChange, bool, ChatSyncState, error) {
	if afterSeq < 0 || generation < 1 || limit < 1 {
		return nil, false, ChatSyncState{}, errors.New("invalid chat change pagination")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, false, ChatSyncState{}, err
	}
	defer tx.Rollback()
	syncState, err := chatSyncState(tx)
	if err != nil {
		return nil, false, ChatSyncState{}, err
	}
	if generation != syncState.Generation || afterSeq > syncState.LatestChangeSeq {
		return nil, false, syncState, ErrChatCursorResetRequired
	}
	rows, err := tx.Query(`
SELECT c.seq, c.message_id, c.kind, c.created_at,
       m.id, m.author_key, m.author_tag, m.author_role, m.source_ip, m.body,
       m.created_at, m.withdrawn_at, m.deleted_at, m.deleted_by
FROM chat_changes c
JOIN chat_messages m ON m.id = c.message_id
WHERE c.seq > ?
ORDER BY c.seq ASC
LIMIT ?`, afterSeq, limit+1)
	if err != nil {
		return nil, false, ChatSyncState{}, err
	}
	defer rows.Close()
	changes := make([]ChatChange, 0, limit+1)
	for rows.Next() {
		var change ChatChange
		if err := rows.Scan(
			&change.Seq, &change.MessageID, &change.Kind, &change.CreatedAt,
			&change.Message.ID, &change.Message.AuthorKey, &change.Message.AuthorTag,
			&change.Message.AuthorRole, &change.Message.SourceIP, &change.Message.Body,
			&change.Message.CreatedAt, &change.Message.WithdrawnAt, &change.Message.DeletedAt,
			&change.Message.DeletedBy,
		); err != nil {
			return nil, false, ChatSyncState{}, err
		}
		changes = append(changes, change)
	}
	if err := rows.Err(); err != nil {
		return nil, false, ChatSyncState{}, err
	}
	if err := rows.Close(); err != nil {
		return nil, false, ChatSyncState{}, err
	}
	if !chatChangesAreContinuous(changes, afterSeq, syncState.LatestChangeSeq, limit) {
		return nil, false, syncState, ErrChatCursorResetRequired
	}
	hasMore := len(changes) > limit
	if hasMore {
		changes = changes[:limit]
	}
	if err := tx.Commit(); err != nil {
		return nil, false, ChatSyncState{}, err
	}
	return changes, hasMore, syncState, nil
}

func (s *Store) LatestChatChangeSeq() (int64, error) {
	state, err := s.CurrentChatSyncState()
	return state.LatestChangeSeq, err
}

func (s *Store) CurrentChatSyncState() (ChatSyncState, error) {
	return chatSyncState(s.DB)
}

func (s *Store) WithdrawChatMessage(id int64, authorKey, actorRole, actorIP string, now time.Time, window time.Duration) (ChatMessage, int64, error) {
	if id < 1 || window <= 0 {
		return ChatMessage{}, 0, ErrChatWithdrawForbidden
	}
	mutation, overlapped := s.beginChatMutation(id)
	defer s.endChatMutation(id, mutation)
	now = now.UTC()
	cutoff := now.Add(-window)
	tx, err := s.DB.Begin()
	if err != nil {
		return ChatMessage{}, 0, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`
UPDATE chat_messages
SET withdrawn_at = ?
WHERE id = ?
  AND author_key = ?
  AND author_role = 'user'
  AND ? = 'user'
  AND withdrawn_at IS NULL
  AND deleted_at IS NULL
  AND julianday(created_at) >= julianday(?)
  AND julianday(created_at) <= julianday(?)`, now, id, authorKey, actorRole, cutoff, now)
	if err != nil {
		return ChatMessage{}, 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return ChatMessage{}, 0, err
	}
	if affected != 1 {
		domainErr, resultName, err := classifyChatWithdrawTx(tx, id, authorKey, actorRole, cutoff, now)
		if err != nil {
			return ChatMessage{}, 0, err
		}
		if overlapped && errors.Is(domainErr, ErrChatStateConflict) {
			resultName = "concurrent_conflict"
		}
		if err := insertChatAuditTx(tx, "chat_withdraw_failed", actorIP, chatAuditDetail(id, actorRole, resultName), now); err != nil {
			return ChatMessage{}, 0, err
		}
		if err := tx.Commit(); err != nil {
			return ChatMessage{}, 0, err
		}
		s.bestEffortAuditMaintenanceAfterCommit()
		return ChatMessage{}, 0, domainErr
	}
	seq, err := insertChatChangeTx(tx, id, ChatChangeWithdraw, now)
	if err != nil {
		return ChatMessage{}, 0, err
	}
	if err := insertChatAuditTx(tx, "chat_withdraw", actorIP, chatAuditDetail(id, actorRole, "withdrawn"), now); err != nil {
		return ChatMessage{}, 0, err
	}
	message, err := queryChatMessageTx(tx, id)
	if err != nil {
		return ChatMessage{}, 0, err
	}
	if err := tx.Commit(); err != nil {
		return ChatMessage{}, 0, err
	}
	s.bestEffortAuditMaintenanceAfterCommit()
	return message, seq, nil
}

func (s *Store) DeleteChatMessage(id int64, deletedBy, actorIP string, now time.Time) (ChatMessage, int64, error) {
	if id < 1 {
		return ChatMessage{}, 0, ErrChatMessageNotFound
	}
	deletedBy = strings.TrimSpace(deletedBy)
	if deletedBy == "" || len(deletedBy) > 256 {
		return ChatMessage{}, 0, errors.New("invalid chat delete actor")
	}
	mutation, overlapped := s.beginChatMutation(id)
	defer s.endChatMutation(id, mutation)
	now = now.UTC()
	tx, err := s.DB.Begin()
	if err != nil {
		return ChatMessage{}, 0, err
	}
	defer tx.Rollback()
	allowWithdrawn := 1
	if overlapped {
		// If a withdraw operation entered concurrently and won first, the delete
		// must observe a conflict. A later, non-overlapping admin request may still
		// delete a previously withdrawn message as required by the product model.
		allowWithdrawn = 0
	}
	result, err := tx.Exec(`
UPDATE chat_messages
SET body = NULL, deleted_at = ?, deleted_by = ?
WHERE id = ?
  AND deleted_at IS NULL
  AND (withdrawn_at IS NULL OR ? = 1)`, now, deletedBy, id, allowWithdrawn)
	if err != nil {
		return ChatMessage{}, 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return ChatMessage{}, 0, err
	}
	if affected != 1 {
		domainErr, resultName, err := classifyChatDeleteTx(tx, id)
		if err != nil {
			return ChatMessage{}, 0, err
		}
		if overlapped && errors.Is(domainErr, ErrChatStateConflict) {
			resultName = "concurrent_conflict"
		}
		if err := insertChatAuditTx(tx, "chat_delete_failed", actorIP, chatAuditDetail(id, "admin", resultName), now); err != nil {
			return ChatMessage{}, 0, err
		}
		if err := tx.Commit(); err != nil {
			return ChatMessage{}, 0, err
		}
		s.bestEffortAuditMaintenanceAfterCommit()
		return ChatMessage{}, 0, domainErr
	}
	seq, err := insertChatChangeTx(tx, id, ChatChangeDelete, now)
	if err != nil {
		return ChatMessage{}, 0, err
	}
	if err := insertChatAuditTx(tx, "chat_delete", actorIP, chatAuditDetail(id, "admin", "deleted"), now); err != nil {
		return ChatMessage{}, 0, err
	}
	message, err := queryChatMessageTx(tx, id)
	if err != nil {
		return ChatMessage{}, 0, err
	}
	if err := tx.Commit(); err != nil {
		return ChatMessage{}, 0, err
	}
	s.bestEffortAuditMaintenanceAfterCommit()
	return message, seq, nil
}

func (s *Store) CleanupChat(now time.Time, retentionDays, maxMessages, batch int) (int, error) {
	if retentionDays < 1 || maxMessages < 1 || batch < 1 {
		return 0, errors.New("invalid chat cleanup policy")
	}
	cutoff := now.UTC().AddDate(0, 0, -retentionDays)
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	cleanupSelection := `
SELECT id FROM chat_messages
WHERE julianday(created_at) < julianday(?)
   OR id IN (
     SELECT id FROM chat_messages ORDER BY created_at DESC, id DESC LIMIT -1 OFFSET ?
   )
ORDER BY created_at ASC, id ASC
LIMIT ?`
	if _, err := tx.Exec(`DELETE FROM chat_changes WHERE message_id IN (`+cleanupSelection+`)`, cutoff, maxMessages, batch); err != nil {
		return 0, err
	}
	result, err := tx.Exec(`DELETE FROM chat_messages WHERE id IN (`+cleanupSelection+`)`, cutoff, maxMessages, batch)
	if err != nil {
		return 0, err
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if removed > 0 {
		result, err := tx.Exec(`UPDATE chat_sync_metadata SET generation = generation + 1 WHERE singleton = 1`)
		if err != nil {
			return 0, err
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		if updated != 1 {
			return 0, errors.New("chat sync metadata missing")
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(removed), nil
}

const chatMessageSelect = `
SELECT id, author_key, author_tag, author_role, source_ip, body,
       created_at, withdrawn_at, deleted_at, deleted_by
FROM chat_messages`

type chatScanner interface {
	Scan(dest ...any) error
}

type chatRowQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func chatSyncState(queryer chatRowQueryer) (ChatSyncState, error) {
	var state ChatSyncState
	err := queryer.QueryRow(`
SELECT generation,
       MAX(
         COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'chat_changes'), 0),
         COALESCE((SELECT MAX(seq) FROM chat_changes), 0)
       )
FROM chat_sync_metadata
WHERE singleton = 1`).Scan(&state.Generation, &state.LatestChangeSeq)
	return state, err
}

func chatChangesAreContinuous(changes []ChatChange, afterSeq, latestSeq int64, limit int) bool {
	if afterSeq == latestSeq {
		return len(changes) == 0
	}
	if len(changes) == 0 || changes[0].Seq != afterSeq+1 {
		return false
	}
	for index := 1; index < len(changes); index++ {
		if changes[index].Seq != changes[index-1].Seq+1 {
			return false
		}
	}
	// limit+1 rows prove that a later page exists. If the fetched tail is not
	// beyond the requested page, it must reach the durable high-water mark.
	return len(changes) > limit || changes[len(changes)-1].Seq == latestSeq
}

func scanChatMessage(scanner chatScanner) (ChatMessage, error) {
	var message ChatMessage
	err := scanner.Scan(
		&message.ID, &message.AuthorKey, &message.AuthorTag, &message.AuthorRole,
		&message.SourceIP, &message.Body, &message.CreatedAt, &message.WithdrawnAt,
		&message.DeletedAt, &message.DeletedBy,
	)
	return message, err
}

func queryChatMessageTx(tx *sql.Tx, id int64) (ChatMessage, error) {
	return scanChatMessage(tx.QueryRow(chatMessageSelect+` WHERE id = ?`, id))
}

func insertChatChangeTx(tx *sql.Tx, messageID int64, kind string, now time.Time) (int64, error) {
	result, err := tx.Exec(`INSERT INTO chat_changes(message_id, kind, created_at) VALUES(?, ?, ?)`, messageID, kind, now)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func insertChatAuditTx(tx *sql.Tx, action, ip, detail string, now time.Time) error {
	_, err := tx.Exec(
		`INSERT INTO audit_logs(action, ip, detail, created_at) VALUES(?, ?, ?, ?)`,
		action, ip, sanitizeAuditDetail(detail), now,
	)
	return err
}

func classifyChatWithdrawTx(tx *sql.Tx, id int64, authorKey, actorRole string, cutoff, now time.Time) (error, string, error) {
	var storedAuthorKey, storedRole string
	var createdAt time.Time
	var withdrawnAt, deletedAt sql.NullTime
	err := tx.QueryRow(`SELECT author_key, author_role, created_at, withdrawn_at, deleted_at FROM chat_messages WHERE id = ?`, id).
		Scan(&storedAuthorKey, &storedRole, &createdAt, &withdrawnAt, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrChatMessageNotFound, "not_found", nil
	}
	if err != nil {
		return nil, "", err
	}
	if actorRole != "user" || storedRole != "user" || storedAuthorKey != authorKey {
		return ErrChatWithdrawForbidden, "forbidden", nil
	}
	if deletedAt.Valid || withdrawnAt.Valid {
		return ErrChatStateConflict, "state_conflict", nil
	}
	if createdAt.Before(cutoff) || createdAt.After(now) {
		return ErrChatWithdrawExpired, "expired", nil
	}
	return ErrChatStateConflict, "state_conflict", nil
}

func classifyChatDeleteTx(tx *sql.Tx, id int64) (error, string, error) {
	var withdrawnAt, deletedAt sql.NullTime
	err := tx.QueryRow(`SELECT withdrawn_at, deleted_at FROM chat_messages WHERE id = ?`, id).Scan(&withdrawnAt, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrChatMessageNotFound, "not_found", nil
	}
	if err != nil {
		return nil, "", err
	}
	return ErrChatStateConflict, "state_conflict", nil
}

func chatAuditDetail(id int64, actorRole, result string) string {
	return fmt.Sprintf("message_id=%d actor_role=%s result=%s", id, sanitizeChatActor(actorRole), sanitizeChatActor(result))
}

func sanitizeChatActor(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "admin" || value == "user" {
		return value
	}
	if value == "withdrawn" || value == "deleted" || value == "forbidden" || value == "expired" || value == "not_found" || value == "state_conflict" || value == "concurrent_conflict" {
		return value
	}
	return "unknown"
}

func (s *Store) beginChatMutation(messageID int64) (*chatMutationState, bool) {
	s.chatMutationMu.Lock()
	if s.chatMutations == nil {
		s.chatMutations = make(map[int64]*chatMutationState)
	}
	state := s.chatMutations[messageID]
	if state == nil {
		state = &chatMutationState{}
		// Lock before publishing the new state. Otherwise a second caller can
		// observe it and win the gate between this mutex unlock and gate.Lock,
		// causing the original (non-overlapped) caller to run second.
		state.gate.Lock()
		state.active = 1
		s.chatMutations[messageID] = state
		s.chatMutationMu.Unlock()
		return state, false
	}
	state.active++
	s.chatMutationMu.Unlock()
	state.gate.Lock()
	return state, true
}

func (s *Store) endChatMutation(messageID int64, state *chatMutationState) {
	state.gate.Unlock()
	s.chatMutationMu.Lock()
	state.active--
	if state.active == 0 && s.chatMutations[messageID] == state {
		delete(s.chatMutations, messageID)
	}
	s.chatMutationMu.Unlock()
}

func (s *Store) chatMutationCount(messageID int64) int {
	s.chatMutationMu.Lock()
	defer s.chatMutationMu.Unlock()
	if state := s.chatMutations[messageID]; state != nil {
		return state.active
	}
	return 0
}
