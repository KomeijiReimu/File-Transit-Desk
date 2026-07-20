package server

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"filetrans-backend/internal/store"

	"github.com/gofiber/fiber/v2"
)

const (
	chatDefaultPageSize                 = 50
	chatMaxPageSize                     = 100
	chatMaxChangePage                   = 500
	chatMaxRequestBytes                 = 8192
	chatAdminDestructiveMaxRequestBytes = 4096

	chatActionPerSessionPerMinute = 120
	chatActionPerIPPerMinute      = 300
	chatActionGlobalPerMinute     = 1000
)

type chatMessageDTO struct {
	ID            int64      `json:"id"`
	AuthorTag     string     `json:"authorTag"`
	Role          string     `json:"role"`
	SourceIP      string     `json:"sourceIP"`
	Body          *string    `json:"body"`
	Status        string     `json:"status"`
	IsMine        bool       `json:"isMine"`
	CreatedAt     time.Time  `json:"createdAt"`
	WithdrawnAt   *time.Time `json:"withdrawnAt"`
	DeletedAt     *time.Time `json:"deletedAt"`
	CanWithdraw   bool       `json:"canWithdraw"`
	WithdrawUntil *time.Time `json:"withdrawUntil"`
}

type adminChatMessageDTO struct {
	ID            int64      `json:"id"`
	AuthorTag     string     `json:"authorTag"`
	Role          string     `json:"role"`
	SourceIP      string     `json:"sourceIP"`
	Body          *string    `json:"body"`
	Status        string     `json:"status"`
	IsMine        bool       `json:"isMine"`
	CreatedAt     time.Time  `json:"createdAt"`
	WithdrawnAt   *time.Time `json:"withdrawnAt"`
	DeletedAt     *time.Time `json:"deletedAt"`
	CanWithdraw   bool       `json:"canWithdraw"`
	WithdrawUntil *time.Time `json:"withdrawUntil"`
}

type chatChangeDTO[T any] struct {
	Seq       int64     `json:"seq"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"createdAt"`
	Message   T         `json:"message"`
}

type chatMessagesResponse[T any] struct {
	Messages        []T    `json:"messages"`
	NextBeforeID    *int64 `json:"nextBeforeId"`
	HasMore         bool   `json:"hasMore"`
	LatestChangeSeq int64  `json:"latestChangeSeq"`
	Generation      int64  `json:"generation"`
}

type chatChangesResponse[T any] struct {
	Changes         []chatChangeDTO[T] `json:"changes"`
	NextAfterSeq    int64              `json:"nextAfterSeq"`
	HasMore         bool               `json:"hasMore"`
	LatestChangeSeq int64              `json:"latestChangeSeq"`
	Generation      int64              `json:"generation"`
}

type chatMutationResponse[T any] struct {
	Message  T     `json:"message"`
	EventSeq int64 `json:"eventSeq"`
}

type chatBatchDeleteResponse[T any] struct {
	DeletedCount int                       `json:"deletedCount"`
	Mutations    []chatMutationResponse[T] `json:"mutations"`
}

type chatClearResponse struct {
	ClearedCount    int   `json:"clearedCount"`
	Generation      int64 `json:"generation"`
	LatestChangeSeq int64 `json:"latestChangeSeq"`
}

type chatCapabilitiesResponse struct {
	MaxMessageChars       int `json:"maxMessageChars"`
	MaxMessageBytes       int `json:"maxMessageBytes"`
	MaxRequestBytes       int `json:"maxRequestBytes"`
	WithdrawWindowSeconds int `json:"withdrawWindowSeconds"`
	HistoryDefaultLimit   int `json:"historyDefaultLimit"`
	HistoryMaxLimit       int `json:"historyMaxLimit"`
	ChangesDefaultLimit   int `json:"changesDefaultLimit"`
	ChangesMaxLimit       int `json:"changesMaxLimit"`
}

func (s *Server) chatMessages(c *fiber.Ctx) error {
	beforeID, limit, err := parseChatHistoryQuery(c)
	if err != nil {
		return err
	}
	messages, hasMore, syncState, err := s.store.ChatMessagesPage(beforeID, limit)
	if err != nil {
		return err
	}
	now := s.chatCurrentTime()
	viewerKey, viewerRole := chatViewer(c)
	items := make([]chatMessageDTO, 0, len(messages))
	for _, message := range messages {
		items = append(items, s.chatMessageProjection(message, viewerKey, viewerRole, now))
	}
	return c.JSON(chatMessagesResponse[chatMessageDTO]{
		Messages:        items,
		NextBeforeID:    nextChatBeforeID(messages, hasMore),
		HasMore:         hasMore,
		LatestChangeSeq: syncState.LatestChangeSeq,
		Generation:      syncState.Generation,
	})
}

func (s *Server) adminChatMessages(c *fiber.Ctx) error {
	beforeID, limit, err := parseChatHistoryQuery(c)
	if err != nil {
		return err
	}
	messages, hasMore, syncState, err := s.store.ChatMessagesPage(beforeID, limit)
	if err != nil {
		return err
	}
	now := s.chatCurrentTime()
	viewerKey, viewerRole := chatViewer(c)
	items := make([]adminChatMessageDTO, 0, len(messages))
	for _, message := range messages {
		items = append(items, s.adminChatMessageProjection(message, viewerKey, viewerRole, now))
	}
	return c.JSON(chatMessagesResponse[adminChatMessageDTO]{
		Messages:        items,
		NextBeforeID:    nextChatBeforeID(messages, hasMore),
		HasMore:         hasMore,
		LatestChangeSeq: syncState.LatestChangeSeq,
		Generation:      syncState.Generation,
	})
}

func (s *Server) chatChanges(c *fiber.Ctx) error {
	afterSeq, generation, limit, err := parseChatChangesQuery(c)
	if err != nil {
		return err
	}
	changes, hasMore, syncState, err := s.store.ChatChangesPage(afterSeq, generation, limit)
	if err != nil {
		return chatChangesStoreError(err)
	}
	now := s.chatCurrentTime()
	viewerKey, viewerRole := chatViewer(c)
	items := make([]chatChangeDTO[chatMessageDTO], 0, len(changes))
	nextSeq := afterSeq
	for _, change := range changes {
		items = append(items, chatChangeDTO[chatMessageDTO]{
			Seq: change.Seq, Kind: change.Kind, CreatedAt: change.CreatedAt,
			Message: s.chatMessageProjection(change.Message, viewerKey, viewerRole, now),
		})
		nextSeq = change.Seq
	}
	return c.JSON(chatChangesResponse[chatMessageDTO]{Changes: items, NextAfterSeq: nextSeq, HasMore: hasMore, LatestChangeSeq: syncState.LatestChangeSeq, Generation: syncState.Generation})
}

func (s *Server) adminChatChanges(c *fiber.Ctx) error {
	afterSeq, generation, limit, err := parseChatChangesQuery(c)
	if err != nil {
		return err
	}
	changes, hasMore, syncState, err := s.store.ChatChangesPage(afterSeq, generation, limit)
	if err != nil {
		return chatChangesStoreError(err)
	}
	now := s.chatCurrentTime()
	viewerKey, viewerRole := chatViewer(c)
	items := make([]chatChangeDTO[adminChatMessageDTO], 0, len(changes))
	nextSeq := afterSeq
	for _, change := range changes {
		items = append(items, chatChangeDTO[adminChatMessageDTO]{
			Seq: change.Seq, Kind: change.Kind, CreatedAt: change.CreatedAt,
			Message: s.adminChatMessageProjection(change.Message, viewerKey, viewerRole, now),
		})
		nextSeq = change.Seq
	}
	return c.JSON(chatChangesResponse[adminChatMessageDTO]{Changes: items, NextAfterSeq: nextSeq, HasMore: hasMore, LatestChangeSeq: syncState.LatestChangeSeq, Generation: syncState.Generation})
}

func (s *Server) chatCapabilities(c *fiber.Ctx) error {
	cfg := s.cfg().Chat
	return c.JSON(chatCapabilitiesResponse{
		MaxMessageChars:       cfg.MaxMessageChars,
		MaxMessageBytes:       cfg.MaxMessageBytes,
		MaxRequestBytes:       chatMaxRequestBytes,
		WithdrawWindowSeconds: cfg.WithdrawWindowSeconds,
		HistoryDefaultLimit:   chatDefaultPageSize,
		HistoryMaxLimit:       chatMaxPageSize,
		ChangesDefaultLimit:   chatDefaultPageSize,
		ChangesMaxLimit:       chatMaxChangePage,
	})
}

func (s *Server) createChatMessage(c *fiber.Ctx) error {
	chatCfg := s.cfg().Chat
	body, err := decodeChatCreateBody(c)
	if err != nil {
		return err
	}
	body, err = validateChatText(body, chatCfg.MaxMessageChars, chatCfg.MaxMessageBytes)
	if err != nil {
		return err
	}
	authorKey, role := chatViewer(c)
	if authorKey == "" || role != "user" && role != "admin" {
		return fiber.ErrUnauthorized
	}
	ip := s.clientIP(c)
	if err := s.checkChatSendRate(c, authorKey, ip); err != nil {
		return err
	}
	now := s.chatCurrentTime()
	message, seq, err := s.store.CreateChatMessage(store.ChatCreateInput{
		AuthorKey: authorKey, AuthorTag: chatAuthorTag(authorKey, role), AuthorRole: role,
		SourceIP: ip, Body: body, CreatedAt: now,
	})
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(chatMutationResponse[chatMessageDTO]{
		Message: s.chatMessageProjection(message, authorKey, role, now), EventSeq: seq,
	})
}

func (s *Server) withdrawChatMessage(c *fiber.Ctx) error {
	id, err := parseChatMessageID(c.Params("id"))
	if err != nil {
		return err
	}
	authorKey, role := chatViewer(c)
	if err := s.checkChatActionRate(c, authorKey, s.clientIP(c), false); err != nil {
		return err
	}
	now := s.chatCurrentTime()
	message, seq, err := s.store.WithdrawChatMessage(
		id, authorKey, role, s.clientIP(c), now,
		time.Duration(s.cfg().Chat.WithdrawWindowSeconds)*time.Second,
	)
	if err != nil {
		return chatMutationStoreError(err)
	}
	return c.JSON(chatMutationResponse[chatMessageDTO]{Message: s.chatMessageProjection(message, authorKey, role, now), EventSeq: seq})
}

func (s *Server) deleteChatMessage(c *fiber.Ctx) error {
	id, err := parseChatMessageID(c.Params("id"))
	if err != nil {
		return err
	}
	actorKey, role := chatViewer(c)
	ip := s.clientIP(c)
	if err := s.checkChatActionRate(c, actorKey, ip, true); err != nil {
		return err
	}
	now := s.chatCurrentTime()
	message, seq, err := s.store.DeleteChatMessage(id, actorKey, ip, now)
	if err != nil {
		return chatMutationStoreError(err)
	}
	return c.JSON(chatMutationResponse[adminChatMessageDTO]{Message: s.adminChatMessageProjection(message, actorKey, role, now), EventSeq: seq})
}

func (s *Server) batchDeleteChatMessages(c *fiber.Ctx) error {
	actorKey, role := chatViewer(c)
	ip := s.clientIP(c)
	ids, err := decodeChatBatchDeleteBody(c)
	if err != nil {
		s.bestEffortAudit("chat_batch_delete_failed", ip, chatBatchRequestAuditDetail(0, "invalid_request"))
		return err
	}
	if err := s.checkChatActionRateKind(c, actorKey, ip, "batch-delete"); err != nil {
		s.bestEffortAudit("chat_batch_delete_failed", ip, chatBatchRequestAuditDetail(len(ids), "rate_limited"))
		return err
	}
	mutations, err := s.store.BatchDeleteChatMessages(ids, actorKey, ip, s.chatCurrentTime())
	if err != nil {
		if errors.Is(err, store.ErrChatBatchDeleteConflict) {
			return newCodedAPIError(fiber.StatusConflict, "chat_batch_delete_conflict", "批量删除与当前消息状态冲突，请刷新后重试。")
		}
		return err
	}
	items := make([]chatMutationResponse[adminChatMessageDTO], 0, len(mutations))
	for _, mutation := range mutations {
		items = append(items, chatMutationResponse[adminChatMessageDTO]{
			Message:  s.adminChatMessageProjection(mutation.Message, actorKey, role, s.chatCurrentTime()),
			EventSeq: mutation.EventSeq,
		})
	}
	return c.JSON(chatBatchDeleteResponse[adminChatMessageDTO]{DeletedCount: len(items), Mutations: items})
}

func (s *Server) clearChatMessages(c *fiber.Ctx) error {
	actorKey, _ := chatViewer(c)
	ip := s.clientIP(c)
	expectedGeneration, expectedLatestChangeSeq, err := decodeChatClearBody(c)
	if err != nil {
		s.bestEffortAudit("chat_clear_failed", ip, chatClearRequestAuditDetail(0, "invalid_request"))
		return err
	}
	if err := s.checkChatActionRateKind(c, actorKey, ip, "clear"); err != nil {
		s.bestEffortAudit("chat_clear_failed", ip, chatClearRequestAuditDetail(expectedGeneration, "rate_limited"))
		return err
	}
	result, err := s.store.ClearChatMessages(expectedGeneration, expectedLatestChangeSeq, ip, s.chatCurrentTime())
	if err != nil {
		if errors.Is(err, store.ErrChatClearConflict) {
			return newCodedAPIError(fiber.StatusConflict, "chat_clear_conflict", "聊天清空条件已过期，请刷新后重新确认。")
		}
		return err
	}
	return c.JSON(chatClearResponse{ClearedCount: result.ClearedCount, Generation: result.Generation, LatestChangeSeq: result.LatestChangeSeq})
}

func (s *Server) chatMessageProjection(message store.ChatMessage, viewerKey, viewerRole string, now time.Time) chatMessageDTO {
	status := message.Status()
	body := chatProjectedBody(message, false)
	isMine := message.AuthorKey == viewerKey
	withdrawnAt := nullTimePointer(message.WithdrawnAt)
	deletedAt := nullTimePointer(message.DeletedAt)
	var withdrawUntil *time.Time
	canWithdraw := false
	if status == store.ChatStatusActive && isMine && viewerRole == "user" && message.AuthorRole == "user" {
		until := message.CreatedAt.Add(time.Duration(s.cfg().Chat.WithdrawWindowSeconds) * time.Second).UTC()
		withdrawUntil = &until
		canWithdraw = !now.Before(message.CreatedAt) && !now.After(until)
	}
	return chatMessageDTO{
		ID: message.ID, AuthorTag: message.AuthorTag, Role: message.AuthorRole, SourceIP: message.SourceIP, Body: body,
		Status: status, IsMine: isMine, CreatedAt: message.CreatedAt.UTC(), WithdrawnAt: withdrawnAt,
		DeletedAt: deletedAt, CanWithdraw: canWithdraw, WithdrawUntil: withdrawUntil,
	}
}

func (s *Server) adminChatMessageProjection(message store.ChatMessage, viewerKey, viewerRole string, now time.Time) adminChatMessageDTO {
	ordinary := s.chatMessageProjection(message, viewerKey, viewerRole, now)
	return adminChatMessageDTO{
		ID: ordinary.ID, AuthorTag: ordinary.AuthorTag, Role: ordinary.Role, SourceIP: message.SourceIP,
		Body: chatProjectedBody(message, true), Status: ordinary.Status, IsMine: ordinary.IsMine,
		CreatedAt: ordinary.CreatedAt, WithdrawnAt: ordinary.WithdrawnAt, DeletedAt: ordinary.DeletedAt,
		CanWithdraw: ordinary.CanWithdraw, WithdrawUntil: ordinary.WithdrawUntil,
	}
}

func chatProjectedBody(message store.ChatMessage, admin bool) *string {
	if message.Status() == store.ChatStatusDeleted || !message.Body.Valid {
		return nil
	}
	if message.Status() == store.ChatStatusWithdrawn && !admin {
		return nil
	}
	body := message.Body.String
	return &body
}

func nullTimePointer(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	timestamp := value.Time.UTC()
	return &timestamp
}

func chatViewer(c *fiber.Ctx) (string, string) {
	key, _ := c.Locals("sessionID").(string)
	role, _ := c.Locals("role").(string)
	return key, role
}

func chatAuthorTag(authorKey, role string) string {
	if role == "admin" {
		return "管理员"
	}
	// authorKey is the stored SHA-256 session identifier. A second,
	// domain-separated one-way derivation gives the UI a stable pseudonym
	// without exposing a prefix or reversible encoding of that ownership key.
	sum := sha256.Sum256([]byte("filetrans-chat-author-tag-v1\x00" + authorKey))
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	return "访客-" + encoded[:6]
}

func (s *Server) chatCurrentTime() time.Time {
	if s.chatNow != nil {
		return s.chatNow().UTC()
	}
	return time.Now().UTC()
}

func parseChatHistoryQuery(c *fiber.Ctx) (int64, int, error) {
	beforeID, err := parseOptionalNonNegativeInt64(c.Query("beforeId"))
	if err != nil {
		return 0, 0, chatPaginationError()
	}
	limit, err := parseChatLimit(c.Query("limit"), chatDefaultPageSize, chatMaxPageSize)
	if err != nil {
		return 0, 0, chatPaginationError()
	}
	return beforeID, limit, nil
}

func parseChatChangesQuery(c *fiber.Ctx) (int64, int64, int, error) {
	generation, err := parseRequiredPositiveInt64(c.Query("generation"))
	if err != nil {
		return 0, 0, 0, newCodedAPIError(fiber.StatusBadRequest, "chat_generation_invalid", "聊天同步 generation 参数无效。")
	}
	afterSeq, err := parseOptionalNonNegativeInt64(c.Query("afterSeq"))
	if err != nil {
		return 0, 0, 0, chatPaginationError()
	}
	limit, err := parseChatLimit(c.Query("limit"), chatDefaultPageSize, chatMaxChangePage)
	if err != nil {
		return 0, 0, 0, chatPaginationError()
	}
	return afterSeq, generation, limit, nil
}

func parseOptionalNonNegativeInt64(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, errors.New("invalid cursor")
	}
	return parsed, nil
}

func parseRequiredPositiveInt64(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("missing positive integer")
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 1 {
		return 0, errors.New("invalid positive integer")
	}
	return parsed, nil
}

func parseChatLimit(value string, fallback, maximum int) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 || parsed > maximum {
		return 0, errors.New("invalid limit")
	}
	return parsed, nil
}

func parseChatMessageID(value string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || id < 1 {
		return 0, newCodedAPIError(fiber.StatusBadRequest, "chat_message_id_invalid", "消息编号无效。")
	}
	return id, nil
}

func chatPaginationError() error {
	return newCodedAPIError(fiber.StatusBadRequest, "chat_page_invalid", "聊天分页参数无效。")
}

func nextChatBeforeID(messages []store.ChatMessage, hasMore bool) *int64 {
	if !hasMore || len(messages) == 0 {
		return nil
	}
	id := messages[0].ID
	return &id
}

func decodeChatCreateBody(c *fiber.Ctx) (string, error) {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(c.Get(fiber.HeaderContentType)))
	if err != nil || !strings.EqualFold(mediaType, fiber.MIMEApplicationJSON) {
		return "", newCodedAPIError(fiber.StatusUnsupportedMediaType, "chat_content_type_invalid", "聊天消息仅接受 JSON。")
	}
	body, err := readBoundedChatBody(c)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(body) {
		return "", newCodedAPIError(fiber.StatusBadRequest, "chat_request_invalid", "聊天消息请求无效。")
	}
	var input struct {
		Body *string `json:"body"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || input.Body == nil {
		return "", newCodedAPIError(fiber.StatusBadRequest, "chat_request_invalid", "聊天消息请求无效。")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", newCodedAPIError(fiber.StatusBadRequest, "chat_request_invalid", "聊天消息请求无效。")
	}
	return *input.Body, nil
}

func decodeChatBatchDeleteBody(c *fiber.Ctx) ([]int64, error) {
	if !chatJSONContentType(c) {
		return nil, newCodedAPIError(fiber.StatusUnsupportedMediaType, "chat_batch_delete_content_type_invalid", "批量删除仅接受 JSON。")
	}
	body, err := readBoundedChatAdminBody(c, "chat_batch_delete")
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(body) {
		return nil, newCodedAPIError(fiber.StatusBadRequest, "chat_batch_delete_request_invalid", "批量删除请求无效。")
	}
	var input struct {
		IDs *[]int64 `json:"ids"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || input.IDs == nil {
		return nil, newCodedAPIError(fiber.StatusBadRequest, "chat_batch_delete_request_invalid", "批量删除请求无效。")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, newCodedAPIError(fiber.StatusBadRequest, "chat_batch_delete_request_invalid", "批量删除请求无效。")
	}
	ids := append([]int64(nil), (*input.IDs)...)
	if len(ids) < 1 || len(ids) > 100 {
		return nil, newCodedAPIError(fiber.StatusBadRequest, "chat_batch_delete_request_invalid", "批量删除必须包含 1 到 100 个消息编号。")
	}
	for _, id := range ids {
		if id < 1 {
			return nil, newCodedAPIError(fiber.StatusBadRequest, "chat_batch_delete_request_invalid", "批量删除消息编号无效。")
		}
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	for index := 1; index < len(ids); index++ {
		if ids[index] == ids[index-1] {
			return nil, newCodedAPIError(fiber.StatusBadRequest, "chat_batch_delete_request_invalid", "批量删除消息编号不能重复。")
		}
	}
	return ids, nil
}

func decodeChatClearBody(c *fiber.Ctx) (int64, int64, error) {
	if !chatJSONContentType(c) {
		return 0, 0, newCodedAPIError(fiber.StatusUnsupportedMediaType, "chat_clear_content_type_invalid", "清空聊天仅接受 JSON。")
	}
	body, err := readBoundedChatAdminBody(c, "chat_clear")
	if err != nil {
		return 0, 0, err
	}
	if !utf8.Valid(body) {
		return 0, 0, newCodedAPIError(fiber.StatusBadRequest, "chat_clear_request_invalid", "清空聊天请求无效。")
	}
	var input struct {
		Confirm                 *string `json:"confirm"`
		ExpectedGeneration      *int64  `json:"expectedGeneration"`
		ExpectedLatestChangeSeq *int64  `json:"expectedLatestChangeSeq"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || input.Confirm == nil || input.ExpectedGeneration == nil || input.ExpectedLatestChangeSeq == nil {
		return 0, 0, newCodedAPIError(fiber.StatusBadRequest, "chat_clear_request_invalid", "清空聊天请求无效。")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return 0, 0, newCodedAPIError(fiber.StatusBadRequest, "chat_clear_request_invalid", "清空聊天请求无效。")
	}
	if *input.Confirm != "CLEAR_ALL_MESSAGES" || *input.ExpectedGeneration < 1 || *input.ExpectedLatestChangeSeq < 0 {
		return 0, 0, newCodedAPIError(fiber.StatusBadRequest, "chat_clear_request_invalid", "清空聊天确认参数无效。")
	}
	return *input.ExpectedGeneration, *input.ExpectedLatestChangeSeq, nil
}

func chatJSONContentType(c *fiber.Ctx) bool {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(c.Get(fiber.HeaderContentType)))
	return err == nil && strings.EqualFold(mediaType, fiber.MIMEApplicationJSON)
}

func readBoundedChatAdminBody(c *fiber.Ctx, operation string) ([]byte, error) {
	invalidCode := operation + "_request_invalid"
	tooLargeCode := operation + "_request_too_large"
	invalidMessage := "聊天管理请求无效。"
	tooLargeMessage := "聊天管理请求过大。"
	rejectLarge := func() error {
		return newCodedAPIError(fiber.StatusRequestEntityTooLarge, tooLargeCode, tooLargeMessage)
	}
	if contentLength := c.Request().Header.ContentLength(); contentLength > chatAdminDestructiveMaxRequestBytes {
		return nil, rejectLarge()
	}
	if stream := c.Request().BodyStream(); stream != nil {
		body, err := io.ReadAll(io.LimitReader(stream, chatAdminDestructiveMaxRequestBytes+1))
		if err != nil {
			return nil, newCodedAPIError(fiber.StatusBadRequest, invalidCode, invalidMessage)
		}
		if len(body) > chatAdminDestructiveMaxRequestBytes {
			return nil, rejectLarge()
		}
		return body, nil
	}
	body := c.Body()
	if len(body) > chatAdminDestructiveMaxRequestBytes {
		return nil, rejectLarge()
	}
	return body, nil
}

func readBoundedChatBody(c *fiber.Ctx) ([]byte, error) {
	rejectLarge := func() error {
		return newCodedAPIError(fiber.StatusRequestEntityTooLarge, "chat_request_too_large", "聊天消息请求过大。")
	}
	if contentLength := c.Request().Header.ContentLength(); contentLength > chatMaxRequestBytes {
		return nil, rejectLarge()
	}
	if stream := c.Request().BodyStream(); stream != nil {
		body, err := io.ReadAll(io.LimitReader(stream, chatMaxRequestBytes+1))
		if err != nil {
			return nil, newCodedAPIError(fiber.StatusBadRequest, "chat_request_invalid", "聊天消息请求无效。")
		}
		if len(body) > chatMaxRequestBytes {
			return nil, rejectLarge()
		}
		return body, nil
	}
	body := c.Body()
	if len(body) > chatMaxRequestBytes {
		return nil, rejectLarge()
	}
	return body, nil
}

func validateChatText(value string, maxChars, maxBytes int) (string, error) {
	if !utf8.ValidString(value) {
		return "", newCodedAPIError(fiber.StatusBadRequest, "chat_message_invalid", "聊天消息正文无效。")
	}
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	for _, r := range value {
		if r == '\t' || r == '\n' {
			continue
		}
		if r == 0 || unicode.IsControl(r) || dangerousChatFormatControl(r) {
			return "", newCodedAPIError(fiber.StatusBadRequest, "chat_message_control_character", "聊天消息包含不安全控制字符。")
		}
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", newCodedAPIError(fiber.StatusBadRequest, "chat_message_empty", "聊天消息不能为空。")
	}
	if len(value) > maxBytes || utf8.RuneCountInString(value) > maxChars {
		return "", newCodedAPIError(fiber.StatusRequestEntityTooLarge, "chat_message_too_large", "聊天消息超过长度限制。")
	}
	return value, nil
}

func dangerousChatFormatControl(r rune) bool {
	if r == '\uFEFF' || r == '\u061C' || r == '\u200E' || r == '\u200F' {
		return true
	}
	return r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069
}

func (s *Server) chatWindowLimiter() *windowLimiter {
	s.limiterMu.Lock()
	defer s.limiterMu.Unlock()
	if s.chatLimiter == nil {
		s.chatLimiter = newWindowLimiterWithMaxEntries(4096)
	}
	return s.chatLimiter
}

func (s *Server) checkChatSendRate(c *fiber.Ctx, sessionID, ip string) error {
	cfg := s.cfg().Chat
	allowed, retry := s.chatWindowLimiter().AllowMany([]limitSpec{
		{Key: "chat-send-global", Limit: cfg.GlobalMessagesPerMinute, Window: time.Minute},
		{Key: "chat-send-session:" + sessionID, Limit: cfg.SessionMessagesPerMinute, Window: time.Minute},
		{Key: "chat-send-ip:" + ip, Limit: cfg.IPMessagesPerMinute, Window: time.Minute},
	})
	if allowed {
		return nil
	}
	c.Set(fiber.HeaderRetryAfter, retryAfterSeconds(retry))
	return newCodedAPIError(fiber.StatusTooManyRequests, "chat_send_rate_limited", "发送消息过于频繁，请稍后重试。")
}

func (s *Server) checkChatActionRate(c *fiber.Ctx, sessionID, ip string, admin bool) error {
	kind := "withdraw"
	if admin {
		kind = "delete"
	}
	return s.checkChatActionRateKind(c, sessionID, ip, kind)
}

func (s *Server) checkChatActionRateKind(c *fiber.Ctx, sessionID, ip, kind string) error {
	allowed, retry := s.chatWindowLimiter().AllowMany([]limitSpec{
		{Key: "chat-action-global:" + kind, Limit: chatActionGlobalPerMinute, Window: time.Minute},
		{Key: "chat-action-session:" + kind + ":" + sessionID, Limit: chatActionPerSessionPerMinute, Window: time.Minute},
		{Key: "chat-action-ip:" + kind + ":" + ip, Limit: chatActionPerIPPerMinute, Window: time.Minute},
	})
	if allowed {
		return nil
	}
	c.Set(fiber.HeaderRetryAfter, retryAfterSeconds(retry))
	return newCodedAPIError(fiber.StatusTooManyRequests, "chat_action_rate_limited", "聊天操作过于频繁，请稍后重试。")
}

func chatBatchRequestAuditDetail(count int, result string) string {
	if count < 0 {
		count = 0
	}
	return fmt.Sprintf("count=%d result=%s", count, result)
}

func chatClearRequestAuditDetail(generation int64, result string) string {
	if generation < 0 {
		generation = 0
	}
	return fmt.Sprintf("count=0 result=%s generation=%d", result, generation)
}

func chatMutationStoreError(err error) error {
	switch {
	case errors.Is(err, store.ErrChatMessageNotFound):
		return newCodedAPIError(fiber.StatusNotFound, "chat_message_not_found", "聊天消息不存在。")
	case errors.Is(err, store.ErrChatWithdrawForbidden):
		return newCodedAPIError(fiber.StatusForbidden, "chat_withdraw_forbidden", "无权撤回该消息。")
	case errors.Is(err, store.ErrChatWithdrawExpired):
		return newCodedAPIError(fiber.StatusConflict, "chat_withdraw_expired", "消息已超过可撤回时间。")
	case errors.Is(err, store.ErrChatStateConflict):
		return newCodedAPIError(fiber.StatusConflict, "chat_state_conflict", "消息状态已变化，请刷新后重试。")
	default:
		return err
	}
}

func chatChangesStoreError(err error) error {
	if errors.Is(err, store.ErrChatCursorResetRequired) {
		return newCodedAPIError(fiber.StatusConflict, "chat_cursor_reset_required", "聊天同步游标已失效，请清空本地消息并重新加载历史。")
	}
	return err
}
