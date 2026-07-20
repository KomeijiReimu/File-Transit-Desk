package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChatMigrationIsIdempotentAndDoesNotReferenceSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE sessions(id TEXT PRIMARY KEY, expires_at DATETIME NOT NULL, created_at DATETIME NOT NULL)`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO sessions(id, expires_at, created_at) VALUES('legacy-session', ?, ?)`, time.Now().Add(time.Hour), time.Now()); err != nil {
		t.Fatalf("insert legacy data: %v", err)
	}
	_ = legacy.Close()

	st, err := Open(path, 100)
	if err != nil {
		t.Fatalf("migrate legacy database: %v", err)
	}
	defer st.DB.Close()
	if err := st.Migrate(); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	var legacyCount int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id='legacy-session'`).Scan(&legacyCount); err != nil || legacyCount != 1 {
		t.Fatalf("chat migration lost legacy data: count=%d err=%v", legacyCount, err)
	}
	for _, table := range []string{"chat_messages", "chat_changes", "chat_sync_metadata"} {
		var name string
		if err := st.DB.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil || name != table {
			t.Fatalf("missing migrated table %q: name=%q err=%v", table, name, err)
		}
	}
	state, err := st.CurrentChatSyncState()
	if err != nil || state.Generation != 1 || state.LatestChangeSeq != 0 {
		t.Fatalf("fresh chat sync state=%+v err=%v", state, err)
	}
	if messages, more, pageState, err := st.ChatMessagesPage(0, 50); err != nil || len(messages) != 0 || more || pageState != state {
		t.Fatalf("fresh chat page messages=%+v more=%v state=%+v err=%v", messages, more, pageState, err)
	}
	if _, err := st.DB.Exec(`UPDATE chat_sync_metadata SET generation = 7 WHERE singleton = 1`); err != nil {
		t.Fatalf("set stable generation: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("repeat migration after generation update: %v", err)
	}
	state, err = st.CurrentChatSyncState()
	if err != nil || state.Generation != 7 {
		t.Fatalf("idempotent migration reset generation: state=%+v err=%v", state, err)
	}
	rows, err := st.DB.Query(`PRAGMA foreign_key_list(chat_messages)`)
	if err != nil {
		t.Fatalf("read chat foreign keys: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, seq int
		var table, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			t.Fatalf("scan foreign key: %v", err)
		}
		if table == "sessions" {
			t.Fatalf("chat ownership must not cascade with sessions")
		}
	}
}

func TestChatCreatePaginationChangesAndStateProjection(t *testing.T) {
	st := openChatStore(t)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	created := make([]ChatMessage, 0, 5)
	for index := 1; index <= 5; index++ {
		message, seq, err := st.CreateChatMessage(ChatCreateInput{
			AuthorKey: "session-a", AuthorTag: "访客-ABC123", AuthorRole: "user",
			SourceIP: "192.0.2.1", Body: "message-" + string(rune('0'+index)), CreatedAt: now,
		})
		if err != nil {
			t.Fatalf("create message %d: %v", index, err)
		}
		if seq != int64(index) {
			t.Fatalf("create seq=%d want=%d", seq, index)
		}
		created = append(created, message)
	}

	firstPage, more, err := st.ChatMessages(0, 2)
	if err != nil {
		t.Fatalf("latest page: %v", err)
	}
	assertChatIDs(t, firstPage, []int64{4, 5})
	if !more {
		t.Fatalf("latest page should have older messages")
	}
	secondPage, more, err := st.ChatMessages(firstPage[0].ID, 2)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	assertChatIDs(t, secondPage, []int64{2, 3})
	if !more {
		t.Fatalf("second page should have older messages")
	}
	thirdPage, more, err := st.ChatMessages(secondPage[0].ID, 2)
	if err != nil {
		t.Fatalf("third page: %v", err)
	}
	assertChatIDs(t, thirdPage, []int64{1})
	if more {
		t.Fatalf("third page unexpectedly has more")
	}

	syncState := mustChatSyncState(t, st)
	changes, more, err := st.ChatChanges(0, syncState.Generation, 2)
	if err != nil {
		t.Fatalf("first change page: %v", err)
	}
	if len(changes) != 2 || changes[0].Seq != 1 || changes[1].Seq != 2 || !more {
		t.Fatalf("unexpected first change page: %+v more=%v", changes, more)
	}
	changes, more, err = st.ChatChanges(changes[1].Seq, syncState.Generation, 10)
	if err != nil {
		t.Fatalf("remaining changes: %v", err)
	}
	if len(changes) != 3 || more {
		t.Fatalf("unexpected remaining changes: %+v more=%v", changes, more)
	}
	latest, err := st.LatestChatChangeSeq()
	if err != nil || latest != 5 {
		t.Fatalf("latest seq=%d err=%v", latest, err)
	}

	withdrawn, withdrawSeq, err := st.WithdrawChatMessage(created[0].ID, "session-a", "user", "192.0.2.1", now.Add(time.Minute), 5*time.Minute)
	if err != nil {
		t.Fatalf("withdraw message: %v", err)
	}
	if withdrawn.Status() != ChatStatusWithdrawn || !withdrawn.Body.Valid || withdrawSeq != 6 {
		t.Fatalf("unexpected withdrawn message=%+v seq=%d", withdrawn, withdrawSeq)
	}
	deleted, deleteSeq, err := st.DeleteChatMessage(created[0].ID, "admin", "198.51.100.2", now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("delete withdrawn message: %v", err)
	}
	if deleted.Status() != ChatStatusDeleted || deleted.Body.Valid || deleteSeq != 7 {
		t.Fatalf("unexpected deleted message=%+v seq=%d", deleted, deleteSeq)
	}
	changes, _, err = st.ChatChanges(5, syncState.Generation, 10)
	if err != nil {
		t.Fatalf("state changes: %v", err)
	}
	if len(changes) != 2 || changes[0].Kind != ChatChangeWithdraw || changes[1].Kind != ChatChangeDelete {
		t.Fatalf("missing state changes: %+v", changes)
	}
	for _, change := range changes {
		if change.Message.Status() != ChatStatusDeleted || change.Message.Body.Valid {
			t.Fatalf("changes must project current deleted state: %+v", change)
		}
	}
}

func TestChatWithdrawAuthorizationWindowAndTransactionalAudit(t *testing.T) {
	st := openChatStore(t)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	create := func(body string, at time.Time) ChatMessage {
		t.Helper()
		message, _, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner-a", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "203.0.113.5", Body: body, CreatedAt: at})
		if err != nil {
			t.Fatalf("create chat message: %v", err)
		}
		return message
	}

	if _, _, err := st.WithdrawChatMessage(9999, "owner-a", "user", "203.0.113.5", now, 5*time.Minute); !errors.Is(err, ErrChatMessageNotFound) {
		t.Fatalf("missing message withdraw error=%v", err)
	}
	wrongOwner := create("same-ip-secret", now)
	if _, _, err := st.WithdrawChatMessage(wrongOwner.ID, "owner-b", "user", "203.0.113.5", now.Add(time.Minute), 5*time.Minute); !errors.Is(err, ErrChatWithdrawForbidden) {
		t.Fatalf("same IP different session withdrew message: %v", err)
	}
	adminAttempt := create("admin-attempt-secret", now)
	if _, _, err := st.WithdrawChatMessage(adminAttempt.ID, "owner-a", "admin", "203.0.113.5", now.Add(time.Minute), 5*time.Minute); !errors.Is(err, ErrChatWithdrawForbidden) {
		t.Fatalf("admin used ordinary withdraw: %v", err)
	}
	expired := create("expired-secret", now.Add(-10*time.Minute))
	if _, _, err := st.WithdrawChatMessage(expired.ID, "owner-a", "user", "203.0.113.5", now, 5*time.Minute); !errors.Is(err, ErrChatWithdrawExpired) {
		t.Fatalf("expired message withdraw error=%v", err)
	}
	owned := create("owned-secret", now)
	if _, _, err := st.WithdrawChatMessage(owned.ID, "owner-a", "user", "203.0.113.5", now.Add(5*time.Minute), 5*time.Minute); err != nil {
		t.Fatalf("withdraw at inclusive boundary: %v", err)
	}
	if _, _, err := st.WithdrawChatMessage(owned.ID, "owner-a", "user", "203.0.113.5", now.Add(5*time.Minute), 5*time.Minute); !errors.Is(err, ErrChatStateConflict) {
		t.Fatalf("repeat withdraw error=%v", err)
	}

	logs, err := st.AuditLogs(100)
	if err != nil {
		t.Fatalf("read audit logs: %v", err)
	}
	if len(logs) < 6 {
		t.Fatalf("withdraw outcomes were not audited: %+v", logs)
	}
	successes := 0
	failures := 0
	failureResults := make(map[string]bool)
	for _, entry := range logs {
		if strings.Contains(entry.Detail, "secret") || strings.Contains(entry.Detail, "owner-a") || strings.Contains(entry.Detail, "owner-b") {
			t.Fatalf("audit leaked body or session key: %+v", entry)
		}
		switch entry.Action {
		case "chat_withdraw":
			successes++
		case "chat_withdraw_failed":
			failures++
			for _, result := range []string{"not_found", "forbidden", "expired", "state_conflict"} {
				if strings.Contains(entry.Detail, "result="+result) {
					failureResults[result] = true
				}
			}
		default:
			continue
		}
		if !strings.Contains(entry.Detail, "message_id=") {
			t.Fatalf("withdraw audit lacks message id: %+v", entry)
		}
	}
	if successes != 1 || failures != 5 {
		t.Fatalf("withdraw audit success/failure split successes=%d failures=%d logs=%+v", successes, failures, logs)
	}
	for _, result := range []string{"not_found", "forbidden", "expired", "state_conflict"} {
		if !failureResults[result] {
			t.Fatalf("withdraw failure audit missing result=%s: %+v", result, logs)
		}
	}
}

func TestConcurrentChatWithdrawAndDeleteAllowsOneTransition(t *testing.T) {
	st := openChatStore(t)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	message, _, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.1", Body: "race-body", CreatedAt: now})
	if err != nil {
		t.Fatalf("create race message: %v", err)
	}

	blocker, _ := st.beginChatMutation(message.ID)
	type mutationResult struct {
		kind string
		err  error
	}
	results := make(chan mutationResult, 2)
	go func() {
		_, _, err := st.WithdrawChatMessage(message.ID, "owner", "user", "192.0.2.1", now.Add(time.Minute), 5*time.Minute)
		results <- mutationResult{kind: "withdraw", err: err}
	}()
	go func() {
		_, _, err := st.DeleteChatMessage(message.ID, "admin", "198.51.100.1", now.Add(time.Minute))
		results <- mutationResult{kind: "delete", err: err}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for st.chatMutationCount(message.ID) < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if st.chatMutationCount(message.ID) != 3 {
		st.endChatMutation(message.ID, blocker)
		t.Fatalf("mutations did not overlap")
	}
	st.endChatMutation(message.ID, blocker)

	successes := 0
	for index := 0; index < 2; index++ {
		result := <-results
		if result.err == nil {
			successes++
		} else if !errors.Is(result.err, ErrChatStateConflict) {
			t.Fatalf("unexpected %s error: %v", result.kind, result.err)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent state transitions succeeded=%d want=1", successes)
	}
	state := mustChatSyncState(t, st)
	changes, _, err := st.ChatChanges(0, state.Generation, 10)
	if err != nil {
		t.Fatalf("read race changes: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("create plus exactly one state change expected: %+v", changes)
	}
}

func TestChatTransactionsDoNotDeadlockSingleConnection(t *testing.T) {
	st := openChatStore(t)
	now := time.Now().UTC()
	message, _, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.1", Body: "deadlock sentinel", CreatedAt: now})
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := st.WithdrawChatMessage(message.ID, "owner", "user", "192.0.2.1", now.Add(time.Second), 5*time.Minute)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("transactional withdraw: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("chat transaction deadlocked on single connection")
	}
}

func TestChatChangeAndAuditFailuresRollBackMessageState(t *testing.T) {
	t.Run("create change failure", func(t *testing.T) {
		st := openChatStore(t)
		if _, err := st.DB.Exec(`DROP TABLE chat_changes`); err != nil {
			t.Fatalf("drop chat changes: %v", err)
		}
		if _, _, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.1", Body: "must rollback", CreatedAt: time.Now()}); err == nil {
			t.Fatalf("create succeeded without change table")
		}
		var count int
		if err := st.DB.QueryRow(`SELECT COUNT(*) FROM chat_messages`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("message insert survived change failure: count=%d err=%v", count, err)
		}
	})

	t.Run("withdraw and delete audit failure", func(t *testing.T) {
		st := openChatStore(t)
		now := time.Now().UTC()
		first, _, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.1", Body: "withdraw rollback", CreatedAt: now})
		if err != nil {
			t.Fatalf("create withdraw rollback message: %v", err)
		}
		second, _, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.1", Body: "delete rollback", CreatedAt: now})
		if err != nil {
			t.Fatalf("create delete rollback message: %v", err)
		}
		if _, err := st.DB.Exec(`DROP TABLE audit_logs`); err != nil {
			t.Fatalf("drop audit logs: %v", err)
		}
		if _, _, err := st.WithdrawChatMessage(first.ID, "owner", "user", "192.0.2.1", now.Add(time.Second), 5*time.Minute); err == nil {
			t.Fatalf("withdraw succeeded without atomic audit")
		}
		if _, _, err := st.DeleteChatMessage(second.ID, "admin-session-hash", "198.51.100.1", now.Add(time.Second)); err == nil {
			t.Fatalf("delete succeeded without atomic audit")
		}
		for _, id := range []int64{first.ID, second.ID} {
			message, err := st.ChatMessage(id)
			if err != nil || message.Status() != ChatStatusActive || !message.Body.Valid {
				t.Fatalf("message %d changed despite audit rollback: %+v err=%v", id, message, err)
			}
		}
		state := mustChatSyncState(t, st)
		changes, _, err := st.ChatChanges(0, state.Generation, 10)
		if err != nil || len(changes) != 2 {
			t.Fatalf("state change survived audit rollback: changes=%+v err=%v", changes, err)
		}
	})
}

func TestChatCleanupAppliesAgeCountAndBatchLimits(t *testing.T) {
	st := openChatStore(t)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	for index := 0; index < 7; index++ {
		createdAt := now.Add(time.Duration(index) * time.Minute)
		if index < 3 {
			createdAt = now.AddDate(0, 0, -100).Add(time.Duration(index) * time.Minute)
		}
		if _, _, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.1", Body: "retained", CreatedAt: createdAt}); err != nil {
			t.Fatalf("create cleanup message: %v", err)
		}
	}
	initialState := mustChatSyncState(t, st)
	totalRemoved := 0
	nonEmptyBatches := 0
	for {
		removed, err := st.CleanupChat(now, 90, 3, 2)
		if err != nil {
			t.Fatalf("cleanup chat: %v", err)
		}
		if removed > 2 {
			t.Fatalf("cleanup exceeded batch: %d", removed)
		}
		totalRemoved += removed
		if removed == 0 {
			break
		}
		nonEmptyBatches++
	}
	if totalRemoved != 4 {
		t.Fatalf("cleanup removed=%d want=4", totalRemoved)
	}
	messages, more, err := st.ChatMessages(0, 10)
	if err != nil || more || len(messages) != 3 {
		t.Fatalf("retained messages=%+v more=%v err=%v", messages, more, err)
	}
	var retainedChanges int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM chat_changes`).Scan(&retainedChanges); err != nil || retainedChanges != 3 {
		t.Fatalf("retained changes=%d err=%v", retainedChanges, err)
	}
	state := mustChatSyncState(t, st)
	if state.LatestChangeSeq != 7 || state.Generation != initialState.Generation+int64(nonEmptyBatches) {
		t.Fatalf("cleanup sync state=%+v initial=%+v batches=%d", state, initialState, nonEmptyBatches)
	}
	changes, more, err := st.ChatChanges(state.LatestChangeSeq, state.Generation, 20)
	if err != nil || more || len(changes) != 0 {
		t.Fatalf("same-generation high-water request should be empty: changes=%+v more=%v err=%v", changes, more, err)
	}
	if _, _, err := st.ChatChanges(0, state.Generation, 20); !errors.Is(err, ErrChatCursorResetRequired) {
		t.Fatalf("retention gap did not require reset: %v", err)
	}
}

func TestChatCursorGenerationResetSemantics(t *testing.T) {
	st := openChatStore(t)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	first, firstSeq, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.1", Body: "first", CreatedAt: now.AddDate(0, 0, -2)})
	if err != nil || firstSeq != 1 {
		t.Fatalf("create first message=%+v seq=%d err=%v", first, firstSeq, err)
	}
	beforeCleanup := mustChatSyncState(t, st)
	removed, err := st.CleanupChat(now, 1, 50, 10)
	if err != nil || removed != 1 {
		t.Fatalf("cleanup without later event removed=%d err=%v", removed, err)
	}
	afterCleanup := mustChatSyncState(t, st)
	if afterCleanup.Generation != beforeCleanup.Generation+1 || afterCleanup.LatestChangeSeq != firstSeq {
		t.Fatalf("cleanup state before=%+v after=%+v", beforeCleanup, afterCleanup)
	}
	if _, _, err := st.ChatChanges(firstSeq, beforeCleanup.Generation, 10); !errors.Is(err, ErrChatCursorResetRequired) {
		t.Fatalf("stale generation without later event error=%v", err)
	}
	if changes, more, err := st.ChatChanges(firstSeq, afterCleanup.Generation, 10); err != nil || more || len(changes) != 0 {
		t.Fatalf("matching generation at high-water changes=%+v more=%v err=%v", changes, more, err)
	}
	if _, _, err := st.ChatChanges(firstSeq+1, afterCleanup.Generation, 10); !errors.Is(err, ErrChatCursorResetRequired) {
		t.Fatalf("cursor above high-water error=%v", err)
	}

	second, secondSeq, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.1", Body: "second", CreatedAt: now.Add(time.Minute)})
	if err != nil || secondSeq != 2 {
		t.Fatalf("create post-retention message=%+v seq=%d err=%v", second, secondSeq, err)
	}
	if _, _, err := st.ChatChanges(firstSeq, beforeCleanup.Generation, 10); !errors.Is(err, ErrChatCursorResetRequired) {
		t.Fatalf("stale generation with later event error=%v", err)
	}
	changes, more, err := st.ChatChanges(firstSeq, afterCleanup.Generation, 10)
	if err != nil || more || len(changes) != 1 || changes[0].Seq != secondSeq {
		t.Fatalf("post-retention changes=%+v more=%v err=%v", changes, more, err)
	}

	removed, err = st.CleanupChat(now.AddDate(0, 0, 3), 1, 50, 10)
	if err != nil || removed != 1 {
		t.Fatalf("cleanup all messages removed=%d err=%v", removed, err)
	}
	afterAllDeleted := mustChatSyncState(t, st)
	if afterAllDeleted.Generation != afterCleanup.Generation+1 || afterAllDeleted.LatestChangeSeq != secondSeq {
		t.Fatalf("all-deleted state=%+v", afterAllDeleted)
	}
	removed, err = st.CleanupChat(now.AddDate(0, 0, 3), 1, 50, 10)
	if err != nil || removed != 0 || mustChatSyncState(t, st).Generation != afterAllDeleted.Generation {
		t.Fatalf("empty cleanup changed generation removed=%d err=%v", removed, err)
	}
}

func TestChatDeleteFailureAuditClassificationAndFiltering(t *testing.T) {
	st := openChatStore(t)
	now := time.Now().UTC()
	if _, _, err := st.DeleteChatMessage(9999, "admin", "198.51.100.1", now); !errors.Is(err, ErrChatMessageNotFound) {
		t.Fatalf("missing delete error=%v", err)
	}
	message, _, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.1", Body: "delete audit secret", CreatedAt: now})
	if err != nil {
		t.Fatalf("create delete audit message: %v", err)
	}
	if _, _, err := st.DeleteChatMessage(message.ID, "admin", "198.51.100.1", now.Add(time.Second)); err != nil {
		t.Fatalf("successful delete: %v", err)
	}
	if _, _, err := st.DeleteChatMessage(message.ID, "admin", "198.51.100.1", now.Add(2*time.Second)); !errors.Is(err, ErrChatStateConflict) {
		t.Fatalf("repeat delete error=%v", err)
	}
	failed, total, err := st.AuditLogsPageFiltered(20, 0, AuditLogFilter{Status: "failed"})
	if err != nil || total != 2 || len(failed) != 2 {
		t.Fatalf("failed delete audit page total=%d logs=%+v err=%v", total, failed, err)
	}
	failureResults := make(map[string]bool)
	for _, entry := range failed {
		if entry.Action != "chat_delete_failed" || strings.Contains(entry.Detail, "secret") {
			t.Fatalf("unexpected failed delete audit: %+v", entry)
		}
		for _, result := range []string{"not_found", "state_conflict"} {
			if strings.Contains(entry.Detail, "result="+result) {
				failureResults[result] = true
			}
		}
	}
	for _, result := range []string{"not_found", "state_conflict"} {
		if !failureResults[result] {
			t.Fatalf("delete failure audit missing result=%s: %+v", result, failed)
		}
	}
	ok, total, err := st.AuditLogsPageFiltered(20, 0, AuditLogFilter{Status: "ok"})
	if err != nil || total != 1 || len(ok) != 1 || ok[0].Action != "chat_delete" {
		t.Fatalf("successful delete audit page total=%d logs=%+v err=%v", total, ok, err)
	}
}

func TestChatAuditMaintenanceAfterCommitIsBestEffort(t *testing.T) {
	st := openChatStore(t)
	st.SetAuditPolicy(1, 1)
	now := time.Now().UTC()
	for index := 0; index < 2; index++ {
		if _, err := st.DB.Exec(`INSERT INTO audit_logs(action, ip, detail, created_at) VALUES('seed', '', 'seed', ?)`, now.Add(-time.Duration(index+1)*time.Minute)); err != nil {
			t.Fatalf("seed audit: %v", err)
		}
	}
	message, _, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.1", Body: "best effort secret", CreatedAt: now})
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	if _, err := st.DB.Exec(`CREATE TRIGGER reject_audit_prune BEFORE DELETE ON audit_logs BEGIN SELECT RAISE(ABORT, 'reject prune'); END`); err != nil {
		t.Fatalf("create prune failure trigger: %v", err)
	}
	withdrawn, _, err := st.WithdrawChatMessage(message.ID, "owner", "user", "192.0.2.1", now.Add(time.Second), 5*time.Minute)
	if err != nil || withdrawn.Status() != ChatStatusWithdrawn {
		t.Fatalf("prune failure changed withdraw result: message=%+v err=%v", withdrawn, err)
	}
	if count := st.auditWriteCount.Load(); count != 1 {
		t.Fatalf("chat audit did not increment maintenance counter: %d", count)
	}
	if _, err := st.DB.Exec(`DROP TRIGGER reject_audit_prune`); err != nil {
		t.Fatalf("drop prune failure trigger: %v", err)
	}
	deleted, _, err := st.DeleteChatMessage(message.ID, "admin", "198.51.100.1", now.Add(2*time.Second))
	if err != nil || deleted.Status() != ChatStatusDeleted {
		t.Fatalf("delete after prune recovery: message=%+v err=%v", deleted, err)
	}
	if count := st.auditWriteCount.Load(); count != 2 {
		t.Fatalf("second chat audit did not increment counter: %d", count)
	}
	var retained int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM audit_logs`).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("successful best-effort prune retained=%d err=%v", retained, err)
	}
}

func TestChatBatchDeleteIsAtomicSortedAndEmitsIndependentEvents(t *testing.T) {
	st := openChatStore(t)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	messages := make([]ChatMessage, 0, 3)
	for index := 0; index < 3; index++ {
		message, _, err := st.CreateChatMessage(ChatCreateInput{
			AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user",
			SourceIP: "192.0.2.77", Body: "batch secret", CreatedAt: now.Add(time.Duration(index) * time.Second),
		})
		if err != nil {
			t.Fatalf("create batch message %d: %v", index, err)
		}
		messages = append(messages, message)
	}
	if _, seq, err := st.WithdrawChatMessage(messages[1].ID, "owner", "user", "192.0.2.77", now.Add(time.Minute), 5*time.Minute); err != nil || seq != 4 {
		t.Fatalf("withdraw batch fixture seq=%d err=%v", seq, err)
	}

	mutations, err := st.BatchDeleteChatMessages([]int64{messages[2].ID, messages[0].ID, messages[1].ID}, "admin-session-hash", "198.51.100.7", now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("batch delete: %v", err)
	}
	if len(mutations) != 3 {
		t.Fatalf("batch mutations=%d want=3", len(mutations))
	}
	for index, mutation := range mutations {
		wantID := messages[index].ID
		wantSeq := int64(5 + index)
		if mutation.Message.ID != wantID || mutation.EventSeq != wantSeq || mutation.Message.Status() != ChatStatusDeleted || mutation.Message.Body.Valid || mutation.Message.SourceIP != "192.0.2.77" || !mutation.Message.DeletedBy.Valid {
			t.Fatalf("mutation[%d]=%+v want id=%d seq=%d", index, mutation, wantID, wantSeq)
		}
	}
	state := mustChatSyncState(t, st)
	if state.Generation != 1 || state.LatestChangeSeq != 7 {
		t.Fatalf("batch sync state=%+v", state)
	}
	changes, more, err := st.ChatChanges(4, state.Generation, 10)
	if err != nil || more || len(changes) != 3 {
		t.Fatalf("batch changes=%+v more=%v err=%v", changes, more, err)
	}
	for index, change := range changes {
		if change.Kind != ChatChangeDelete || change.MessageID != messages[index].ID || change.Seq != int64(5+index) {
			t.Fatalf("batch change[%d]=%+v", index, change)
		}
	}
	logs, err := st.AuditLogs(20)
	if err != nil {
		t.Fatalf("batch audit logs: %v", err)
	}
	found := false
	for _, entry := range logs {
		if entry.Action != "chat_batch_delete" {
			continue
		}
		found = true
		if !strings.Contains(entry.Detail, "count=3") || strings.Contains(entry.Detail, "batch secret") || strings.Contains(entry.Detail, "admin-session-hash") {
			t.Fatalf("unsafe batch audit: %+v", entry)
		}
	}
	if !found {
		t.Fatalf("batch success audit missing: %+v", logs)
	}
}

func TestChatBatchDeleteConflictHasNoPartialMutation(t *testing.T) {
	st := openChatStore(t)
	now := time.Now().UTC()
	first, _, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.1", Body: "first secret", CreatedAt: now})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, _, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.2", Body: "second secret", CreatedAt: now})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if _, _, err := st.DeleteChatMessage(second.ID, "admin", "198.51.100.1", now.Add(time.Second)); err != nil {
		t.Fatalf("delete conflict fixture: %v", err)
	}
	before := mustChatSyncState(t, st)
	if _, err := st.BatchDeleteChatMessages([]int64{first.ID, second.ID}, "admin", "198.51.100.1", now.Add(2*time.Second)); !errors.Is(err, ErrChatBatchDeleteConflict) {
		t.Fatalf("already-deleted batch conflict=%v", err)
	}
	stored, err := st.ChatMessage(first.ID)
	if err != nil || stored.Status() != ChatStatusActive || !stored.Body.Valid || stored.Body.String != "first secret" {
		t.Fatalf("batch partially deleted first message: %+v err=%v", stored, err)
	}
	after := mustChatSyncState(t, st)
	if after != before {
		t.Fatalf("batch conflict changed sync state before=%+v after=%+v", before, after)
	}
	if _, err := st.BatchDeleteChatMessages([]int64{first.ID, 99999}, "admin", "198.51.100.1", now.Add(3*time.Second)); !errors.Is(err, ErrChatBatchDeleteConflict) {
		t.Fatalf("missing-id batch conflict=%v", err)
	}
	stored, err = st.ChatMessage(first.ID)
	if err != nil || stored.Status() != ChatStatusActive || !stored.Body.Valid {
		t.Fatalf("missing-id conflict partially deleted first message: %+v err=%v", stored, err)
	}
}

func TestChatBatchDeleteRollsBackOnLaterChangeFailure(t *testing.T) {
	st := openChatStore(t)
	now := time.Now().UTC()
	first, _, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.1", Body: "first", CreatedAt: now})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, _, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.2", Body: "second", CreatedAt: now})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if _, err := st.DB.Exec(fmt.Sprintf(`CREATE TRIGGER reject_second_batch_change BEFORE INSERT ON chat_changes WHEN NEW.kind = 'delete' AND NEW.message_id = %d BEGIN SELECT RAISE(ABORT, 'reject batch change'); END`, second.ID)); err != nil {
		t.Fatalf("create batch rollback trigger: %v", err)
	}
	before := mustChatSyncState(t, st)
	if _, err := st.BatchDeleteChatMessages([]int64{first.ID, second.ID}, "admin", "198.51.100.1", now.Add(time.Second)); err == nil {
		t.Fatalf("batch delete succeeded despite change failure")
	}
	for _, id := range []int64{first.ID, second.ID} {
		message, err := st.ChatMessage(id)
		if err != nil || message.Status() != ChatStatusActive || !message.Body.Valid {
			t.Fatalf("message %d changed despite batch rollback: %+v err=%v", id, message, err)
		}
	}
	if after := mustChatSyncState(t, st); after != before {
		t.Fatalf("batch rollback changed sync state before=%+v after=%+v", before, after)
	}
}

func TestOverlappingReverseOrderChatBatchesDoNotDeadlock(t *testing.T) {
	st := openChatStore(t)
	now := time.Now().UTC()
	first, _, _ := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.1", Body: "first", CreatedAt: now})
	second, _, _ := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.2", Body: "second", CreatedAt: now})
	blocker, _ := st.beginChatMutation(second.ID)
	results := make(chan error, 2)
	go func() {
		_, err := st.BatchDeleteChatMessages([]int64{second.ID, first.ID}, "admin-one", "198.51.100.1", now.Add(time.Second))
		results <- err
	}()
	waitForChatMutationCount(t, st, second.ID, 2)
	go func() {
		_, err := st.BatchDeleteChatMessages([]int64{first.ID, second.ID}, "admin-two", "198.51.100.2", now.Add(time.Second))
		results <- err
	}()
	waitForChatMutationCount(t, st, first.ID, 2)
	st.endChatMutation(second.ID, blocker)
	for index := 0; index < 2; index++ {
		select {
		case err := <-results:
			if !errors.Is(err, ErrChatBatchDeleteConflict) {
				t.Fatalf("overlapping batch error=%v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("overlapping reverse-order batches deadlocked")
		}
	}
	for _, id := range []int64{first.ID, second.ID} {
		message, err := st.ChatMessage(id)
		if err != nil || message.Status() != ChatStatusActive {
			t.Fatalf("conflicting batch changed message %d: %+v err=%v", id, message, err)
		}
	}
}

func TestChatBatchWithdrawAndSingleDeleteConcurrencyCompletes(t *testing.T) {
	st := openChatStore(t)
	now := time.Now().UTC()
	first, _, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.1", Body: "first", CreatedAt: now})
	if err != nil {
		t.Fatalf("create first concurrency fixture: %v", err)
	}
	second, _, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.2", Body: "second", CreatedAt: now})
	if err != nil {
		t.Fatalf("create second concurrency fixture: %v", err)
	}
	start := make(chan struct{})
	results := make(chan error, 3)
	go func() {
		<-start
		_, err := st.BatchDeleteChatMessages([]int64{second.ID, first.ID}, "batch-admin", "198.51.100.1", now.Add(time.Second))
		results <- err
	}()
	go func() {
		<-start
		_, _, err := st.WithdrawChatMessage(first.ID, "owner", "user", "192.0.2.1", now.Add(time.Second), 5*time.Minute)
		results <- err
	}()
	go func() {
		<-start
		_, _, err := st.DeleteChatMessage(second.ID, "single-admin", "198.51.100.2", now.Add(time.Second))
		results <- err
	}()
	close(start)
	for index := 0; index < 3; index++ {
		select {
		case err := <-results:
			if err != nil && !errors.Is(err, ErrChatBatchDeleteConflict) && !errors.Is(err, ErrChatStateConflict) && !errors.Is(err, ErrChatMessageNotFound) {
				t.Fatalf("unexpected concurrent chat mutation error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("batch/withdraw/single-delete concurrency deadlocked")
		}
	}
	for _, id := range []int64{first.ID, second.ID} {
		message, err := st.ChatMessage(id)
		if err != nil {
			t.Fatalf("load concurrent message %d: %v", id, err)
		}
		if status := message.Status(); status != ChatStatusActive && status != ChatStatusWithdrawn && status != ChatStatusDeleted {
			t.Fatalf("invalid concurrent message state: %+v", message)
		}
		if message.Status() == ChatStatusDeleted && message.Body.Valid {
			t.Fatalf("deleted concurrent message retained body: %+v", message)
		}
	}
}

func TestChatClearAllGenerationHighWaterSequencesAndReplay(t *testing.T) {
	st := openChatStore(t)
	now := time.Now().UTC()
	first, firstSeq, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.1", Body: "first", CreatedAt: now})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, secondSeq, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.2", Body: "second", CreatedAt: now})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if _, withdrawSeq, err := st.WithdrawChatMessage(first.ID, "owner", "user", "192.0.2.1", now.Add(time.Second), 5*time.Minute); err != nil || withdrawSeq != 3 {
		t.Fatalf("withdraw fixture seq=%d err=%v", withdrawSeq, err)
	}
	before := mustChatSyncState(t, st)
	result, err := st.ClearChatMessages(before.Generation, before.LatestChangeSeq, "198.51.100.1", now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("clear chat: %v", err)
	}
	if result.ClearedCount != 2 || result.Generation != before.Generation+1 || result.LatestChangeSeq != before.LatestChangeSeq {
		t.Fatalf("clear result=%+v before=%+v", result, before)
	}
	for _, table := range []string{"chat_messages", "chat_changes"} {
		var count int
		if err := st.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("clear left %s count=%d err=%v", table, count, err)
		}
	}
	if _, _, err := st.ChatChanges(before.LatestChangeSeq, before.Generation, 10); !errors.Is(err, ErrChatCursorResetRequired) {
		t.Fatalf("old generation survived clear: %v", err)
	}
	if _, err := st.ClearChatMessages(before.Generation, before.LatestChangeSeq, "198.51.100.1", now.Add(3*time.Second)); !errors.Is(err, ErrChatClearConflict) {
		t.Fatalf("clear replay error=%v", err)
	}
	later, laterSeq, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.3", Body: "later", CreatedAt: now.Add(4 * time.Second)})
	if err != nil {
		t.Fatalf("create after clear: %v", err)
	}
	if later.ID <= second.ID || laterSeq <= before.LatestChangeSeq || firstSeq != 1 || secondSeq != 2 {
		t.Fatalf("sequences reused after clear: first=%d second=%d laterID=%d laterSeq=%d before=%+v", firstSeq, secondSeq, later.ID, laterSeq, before)
	}
}

func TestChatClearEmptyNoopAndOrphanChanges(t *testing.T) {
	t.Run("empty noop", func(t *testing.T) {
		st := openChatStore(t)
		before := mustChatSyncState(t, st)
		result, err := st.ClearChatMessages(before.Generation, before.LatestChangeSeq, "198.51.100.1", time.Now())
		if err != nil || result.ClearedCount != 0 || result.Generation != before.Generation || result.LatestChangeSeq != before.LatestChangeSeq {
			t.Fatalf("empty clear result=%+v err=%v before=%+v", result, err, before)
		}
	})

	t.Run("orphan change advances generation", func(t *testing.T) {
		st := openChatStore(t)
		if _, err := st.DB.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
			t.Fatalf("disable foreign keys: %v", err)
		}
		if _, err := st.DB.Exec(`INSERT INTO chat_changes(message_id, kind, created_at) VALUES(99999, 'delete', ?)`, time.Now()); err != nil {
			t.Fatalf("insert orphan change: %v", err)
		}
		if _, err := st.DB.Exec(`PRAGMA foreign_keys = ON`); err != nil {
			t.Fatalf("restore foreign keys: %v", err)
		}
		before := mustChatSyncState(t, st)
		result, err := st.ClearChatMessages(before.Generation, before.LatestChangeSeq, "198.51.100.1", time.Now())
		if err != nil || result.ClearedCount != 0 || result.Generation != before.Generation+1 || result.LatestChangeSeq != before.LatestChangeSeq {
			t.Fatalf("orphan clear result=%+v err=%v before=%+v", result, err, before)
		}
		var count int
		if err := st.DB.QueryRow(`SELECT COUNT(*) FROM chat_changes`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("orphan changes remained count=%d err=%v", count, err)
		}
	})
}

func TestChatClearFailuresRollBackEverything(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trigger string
	}{
		{name: "message delete", trigger: `CREATE TRIGGER reject_chat_clear_delete BEFORE DELETE ON chat_messages BEGIN SELECT RAISE(ABORT, 'reject clear delete'); END`},
		{name: "generation", trigger: `CREATE TRIGGER reject_chat_clear_generation BEFORE UPDATE ON chat_sync_metadata BEGIN SELECT RAISE(ABORT, 'reject clear generation'); END`},
		{name: "audit", trigger: `CREATE TRIGGER reject_chat_clear_audit BEFORE INSERT ON audit_logs WHEN NEW.action = 'chat_clear' BEGIN SELECT RAISE(ABORT, 'reject clear audit'); END`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openChatStore(t)
			now := time.Now().UTC()
			message, _, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.1", Body: "must survive", CreatedAt: now})
			if err != nil {
				t.Fatalf("create clear rollback fixture: %v", err)
			}
			before := mustChatSyncState(t, st)
			if _, err := st.DB.Exec(tc.trigger); err != nil {
				t.Fatalf("create clear failure trigger: %v", err)
			}
			if _, err := st.ClearChatMessages(before.Generation, before.LatestChangeSeq, "198.51.100.1", now.Add(time.Second)); err == nil {
				t.Fatalf("clear succeeded despite %s failure", tc.name)
			}
			stored, err := st.ChatMessage(message.ID)
			if err != nil || stored.Status() != ChatStatusActive || !stored.Body.Valid || stored.Body.String != "must survive" {
				t.Fatalf("clear %s failure changed message: %+v err=%v", tc.name, stored, err)
			}
			if after := mustChatSyncState(t, st); after != before {
				t.Fatalf("clear %s failure changed sync state before=%+v after=%+v", tc.name, before, after)
			}
			var changes int
			if err := st.DB.QueryRow(`SELECT COUNT(*) FROM chat_changes`).Scan(&changes); err != nil || changes != 1 {
				t.Fatalf("clear %s failure changed events count=%d err=%v", tc.name, changes, err)
			}
		})
	}
}

func TestChatClearCreateAndRetentionConcurrencyCompletes(t *testing.T) {
	for iteration := 0; iteration < 10; iteration++ {
		st := openChatStore(t)
		now := time.Now().UTC()
		for index := 0; index < 4; index++ {
			if _, _, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.1", Body: "old", CreatedAt: now.AddDate(0, 0, -2)}); err != nil {
				t.Fatalf("iteration %d create old message: %v", iteration, err)
			}
		}
		before := mustChatSyncState(t, st)
		start := make(chan struct{})
		results := make(chan error, 3)
		go func() {
			<-start
			_, err := st.ClearChatMessages(before.Generation, before.LatestChangeSeq, "198.51.100.1", now)
			results <- err
		}()
		go func() {
			<-start
			_, _, err := st.CreateChatMessage(ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.2", Body: "concurrent create", CreatedAt: now})
			results <- err
		}()
		go func() {
			<-start
			_, err := st.CleanupChat(now, 1, 50000, 2)
			results <- err
		}()
		close(start)
		for index := 0; index < 3; index++ {
			select {
			case err := <-results:
				if err != nil && !errors.Is(err, ErrChatClearConflict) {
					t.Fatalf("iteration %d concurrent clear/create/retention error: %v", iteration, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("iteration %d clear/create/retention deadlocked", iteration)
			}
		}
		var integrity string
		if err := st.DB.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
			t.Fatalf("iteration %d database integrity=%q err=%v", iteration, integrity, err)
		}
		if _, err := st.CurrentChatSyncState(); err != nil {
			t.Fatalf("iteration %d read sync state: %v", iteration, err)
		}
		if err := st.DB.Close(); err != nil {
			t.Fatalf("iteration %d close store: %v", iteration, err)
		}
	}
}

func waitForChatMutationCount(t *testing.T, st *Store, id int64, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for st.chatMutationCount(id) < count && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := st.chatMutationCount(id); got < count {
		t.Fatalf("message %d mutation count=%d want at least %d", id, got, count)
	}
}

func openChatStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "chat.db"), 1000)
	if err != nil {
		t.Fatalf("open chat store: %v", err)
	}
	t.Cleanup(func() { _ = st.DB.Close() })
	return st
}

func assertChatIDs(t *testing.T, messages []ChatMessage, want []int64) {
	t.Helper()
	if len(messages) != len(want) {
		t.Fatalf("message ids length=%d want=%d: %+v", len(messages), len(want), messages)
	}
	for index := range want {
		if messages[index].ID != want[index] {
			t.Fatalf("message ids[%d]=%d want=%d", index, messages[index].ID, want[index])
		}
	}
}

func mustChatSyncState(t *testing.T, st *Store) ChatSyncState {
	t.Helper()
	state, err := st.CurrentChatSyncState()
	if err != nil {
		t.Fatalf("read chat sync state: %v", err)
	}
	return state
}
