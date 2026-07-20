package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filetrans-backend/internal/config"
	"filetrans-backend/internal/store"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"
)

func TestChatAPIProjectionAuthorizationChangesAndAuditPrivacy(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	h := newChatAPIHarness(t, now, func(cfg *config.Config) {
		cfg.Server.TrustProxyHeaders = true
		cfg.Server.TrustedProxyCIDRs = []string{"0.0.0.0/0", "::/0"}
	})
	h.addSession(t, "user-a", "user")
	h.addSession(t, "user-b", "user")
	h.addSession(t, "admin-a", "admin")

	createOne := h.request(t, http.MethodPost, "/api/chat/messages", "user-a", []byte(`{"body":"  withdrawn secret  "}`), map[string]string{
		"Content-Type": "application/json", "X-Forwarded-For": "198.51.100.44",
	})
	assertChatResponse(t, createOne, http.StatusCreated, "")
	var first chatMutationResponse[chatMessageDTO]
	decodeChatResponse(t, createOne, &first)
	if first.Message.Body == nil || *first.Message.Body != "withdrawn secret" || first.Message.AuthorTag == "" || first.Message.AuthorTag == "访客-" || first.Message.Role != "user" || !first.Message.IsMine || !first.Message.CanWithdraw {
		t.Fatalf("unexpected created message: %+v", first)
	}
	if strings.Contains(first.Message.AuthorTag, h.sessionHash(t, "user-a")[:6]) {
		t.Fatalf("author tag exposes session hash prefix: %+v", first.Message)
	}

	wrongWithdraw := h.request(t, http.MethodPost, fmt.Sprintf("/api/chat/messages/%d/withdraw", first.Message.ID), "user-b", nil, map[string]string{"X-Forwarded-For": "198.51.100.44"})
	assertChatResponse(t, wrongWithdraw, http.StatusForbidden, "chat_withdraw_forbidden")
	adminWithdraw := h.request(t, http.MethodPost, fmt.Sprintf("/api/chat/messages/%d/withdraw", first.Message.ID), "admin-a", nil, nil)
	assertChatResponse(t, adminWithdraw, http.StatusForbidden, "chat_withdraw_forbidden")
	ownerWithdraw := h.request(t, http.MethodPost, fmt.Sprintf("/api/chat/messages/%d/withdraw", first.Message.ID), "user-a", nil, map[string]string{"X-Forwarded-For": "198.51.100.44"})
	assertChatResponse(t, ownerWithdraw, http.StatusOK, "")
	var withdrawn chatMutationResponse[chatMessageDTO]
	decodeChatResponse(t, ownerWithdraw, &withdrawn)
	if withdrawn.Message.Status != "withdrawn" || withdrawn.Message.Body != nil || withdrawn.Message.CanWithdraw {
		t.Fatalf("ordinary withdraw projection leaked body or action: %+v", withdrawn)
	}

	createTwo := h.request(t, http.MethodPost, "/api/chat/messages", "user-b", []byte(`{"body":"deleted secret"}`), map[string]string{
		"Content-Type": "application/json", "X-Forwarded-For": "198.51.100.44",
	})
	assertChatResponse(t, createTwo, http.StatusCreated, "")
	var second chatMutationResponse[chatMessageDTO]
	decodeChatResponse(t, createTwo, &second)
	deleteTwo := h.request(t, http.MethodDelete, fmt.Sprintf("/api/admin/chat/messages/%d", second.Message.ID), "admin-a", nil, map[string]string{"X-Forwarded-For": "203.0.113.9"})
	assertChatResponse(t, deleteTwo, http.StatusOK, "")
	var deleted chatMutationResponse[adminChatMessageDTO]
	decodeChatResponse(t, deleteTwo, &deleted)
	if deleted.Message.Status != "deleted" || deleted.Message.Body != nil || deleted.Message.SourceIP != "198.51.100.44" {
		t.Fatalf("unexpected admin delete projection: %+v", deleted)
	}

	adminSend := h.request(t, http.MethodPost, "/api/chat/messages", "admin-a", []byte(`{"body":"admin plain text"}`), map[string]string{"Content-Type": "application/json"})
	assertChatResponse(t, adminSend, http.StatusCreated, "")
	var adminCreated chatMutationResponse[chatMessageDTO]
	decodeChatResponse(t, adminSend, &adminCreated)
	if adminCreated.Message.AuthorTag != "管理员" || adminCreated.Message.Role != "admin" || adminCreated.Message.CanWithdraw {
		t.Fatalf("unexpected admin-created ordinary projection: %+v", adminCreated)
	}

	ordinaryList := h.request(t, http.MethodGet, "/api/chat/messages?limit=10", "user-a", nil, nil)
	assertChatResponse(t, ordinaryList, http.StatusOK, "")
	ordinaryRaw := ordinaryList.body
	var ordinary chatMessagesResponse[chatMessageDTO]
	decodeChatResponse(t, ordinaryList, &ordinary)
	ordinaryFirst := findOrdinaryChatMessage(t, ordinary.Messages, first.Message.ID)
	ordinarySecond := findOrdinaryChatMessage(t, ordinary.Messages, second.Message.ID)
	if ordinaryFirst.Status != "withdrawn" || ordinaryFirst.Body != nil || ordinarySecond.Status != "deleted" || ordinarySecond.Body != nil {
		t.Fatalf("ordinary list leaked tombstone body: first=%+v second=%+v", ordinaryFirst, ordinarySecond)
	}
	for _, forbidden := range []string{"sourceIP", "authorKey", "deletedBy", "withdrawn secret", "deleted secret", h.sessionHash(t, "user-a")} {
		if bytes.Contains(ordinaryRaw, []byte(forbidden)) {
			t.Fatalf("ordinary DTO leaked %q: %s", forbidden, ordinaryRaw)
		}
	}

	adminList := h.request(t, http.MethodGet, "/api/admin/chat/messages?limit=10", "admin-a", nil, nil)
	assertChatResponse(t, adminList, http.StatusOK, "")
	var adminPage chatMessagesResponse[adminChatMessageDTO]
	decodeChatResponse(t, adminList, &adminPage)
	adminFirst := findAdminChatMessage(t, adminPage.Messages, first.Message.ID)
	adminSecond := findAdminChatMessage(t, adminPage.Messages, second.Message.ID)
	if adminFirst.Status != "withdrawn" || adminFirst.Body == nil || *adminFirst.Body != "withdrawn secret" || adminFirst.SourceIP != "198.51.100.44" {
		t.Fatalf("admin did not receive withdrawn body/IP: %+v", adminFirst)
	}
	if adminSecond.Status != "deleted" || adminSecond.Body != nil {
		t.Fatalf("deleted body survived admin projection: %+v", adminSecond)
	}
	for _, forbidden := range []string{"authorKey", "deletedBy", h.sessionHash(t, "user-a"), h.sessionHash(t, "admin-a")} {
		if bytes.Contains(adminList.body, []byte(forbidden)) {
			t.Fatalf("admin DTO leaked raw ownership key %q: %s", forbidden, adminList.body)
		}
	}

	ordinaryChanges := h.request(t, http.MethodGet, fmt.Sprintf("/api/chat/changes?afterSeq=0&generation=%d&limit=100", ordinary.Generation), "user-a", nil, nil)
	assertChatResponse(t, ordinaryChanges, http.StatusOK, "")
	var ordinaryChangePage chatChangesResponse[chatMessageDTO]
	decodeChatResponse(t, ordinaryChanges, &ordinaryChangePage)
	if len(ordinaryChangePage.Changes) != 5 || ordinaryChangePage.LatestChangeSeq != 5 || ordinaryChangePage.NextAfterSeq != 5 {
		t.Fatalf("unexpected ordinary changes: %+v", ordinaryChangePage)
	}
	assertChatChangeKinds(t, ordinaryChangePage.Changes, []string{"create", "withdraw", "create", "delete", "create"})
	for _, change := range ordinaryChangePage.Changes {
		if change.Message.ID == first.Message.ID && change.Message.Body != nil {
			t.Fatalf("ordinary changes leaked withdrawn body: %+v", change)
		}
		if change.Message.ID == second.Message.ID && change.Message.Body != nil {
			t.Fatalf("ordinary changes leaked deleted body: %+v", change)
		}
	}

	adminChanges := h.request(t, http.MethodGet, fmt.Sprintf("/api/admin/chat/changes?afterSeq=0&generation=%d&limit=100", adminPage.Generation), "admin-a", nil, nil)
	assertChatResponse(t, adminChanges, http.StatusOK, "")
	var adminChangePage chatChangesResponse[adminChatMessageDTO]
	decodeChatResponse(t, adminChanges, &adminChangePage)
	seenWithdrawnBody := false
	for _, change := range adminChangePage.Changes {
		if change.Message.ID == first.Message.ID && change.Message.Body != nil && *change.Message.Body == "withdrawn secret" && change.Message.SourceIP == "198.51.100.44" {
			seenWithdrawnBody = true
		}
		if change.Message.ID == second.Message.ID && change.Message.Body != nil {
			t.Fatalf("admin changes leaked deleted body: %+v", change)
		}
	}
	if !seenWithdrawnBody {
		t.Fatalf("admin changes did not project withdrawn original: %+v", adminChangePage)
	}

	logs, err := h.store.AuditLogs(100)
	if err != nil {
		t.Fatalf("read chat audits: %v", err)
	}
	for _, log := range logs {
		if strings.Contains(log.Detail, "secret") || strings.Contains(log.Detail, h.sessionHash(t, "user-a")) || strings.Contains(log.Detail, h.sessionHash(t, "admin-a")) {
			t.Fatalf("chat audit leaked body or session hash: %+v", log)
		}
	}
}

func TestChatAPIAuthenticationCSRFNoStoreAndPagination(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	h := newChatAPIHarness(t, now, nil)
	h.addSession(t, "user", "user")
	h.addSession(t, "admin", "admin")

	unauthenticated := h.request(t, http.MethodGet, "/api/chat/messages", "", nil, nil)
	assertChatResponse(t, unauthenticated, http.StatusUnauthorized, "")
	forbidden := h.request(t, http.MethodGet, "/api/admin/chat/messages", "user", nil, nil)
	assertChatResponse(t, forbidden, http.StatusForbidden, "")
	csrf := h.request(t, http.MethodPost, "/api/chat/messages", "user", []byte(`{"body":"must not persist"}`), map[string]string{
		"Content-Type": "application/json", "Origin": "https://attacker.example",
	})
	assertChatResponse(t, csrf, http.StatusForbidden, "")

	for index := 1; index <= 3; index++ {
		resp := h.request(t, http.MethodPost, "/api/chat/messages", "user", []byte(fmt.Sprintf(`{"body":"message-%d"}`, index)), map[string]string{"Content-Type": "application/json"})
		assertChatResponse(t, resp, http.StatusCreated, "")
	}
	pageOne := h.request(t, http.MethodGet, "/api/chat/messages?limit=2", "user", nil, nil)
	assertChatResponse(t, pageOne, http.StatusOK, "")
	var first chatMessagesResponse[chatMessageDTO]
	decodeChatResponse(t, pageOne, &first)
	if len(first.Messages) != 2 || !first.HasMore || first.NextBeforeID == nil || first.Messages[0].ID >= first.Messages[1].ID || first.LatestChangeSeq != 3 || first.Generation < 1 {
		t.Fatalf("unexpected first chat page: %+v", first)
	}
	pageTwo := h.request(t, http.MethodGet, fmt.Sprintf("/api/chat/messages?limit=2&beforeId=%d", *first.NextBeforeID), "user", nil, nil)
	assertChatResponse(t, pageTwo, http.StatusOK, "")
	var second chatMessagesResponse[chatMessageDTO]
	decodeChatResponse(t, pageTwo, &second)
	if len(second.Messages) != 1 || second.HasMore || second.NextBeforeID != nil || second.Messages[0].ID >= first.Messages[0].ID {
		t.Fatalf("unexpected second chat page: %+v", second)
	}
	invalidPage := h.request(t, http.MethodGet, "/api/chat/messages?beforeId=-1&limit=101", "user", nil, nil)
	assertChatResponse(t, invalidPage, http.StatusBadRequest, "chat_page_invalid")
	invalidChanges := h.request(t, http.MethodGet, fmt.Sprintf("/api/chat/changes?afterSeq=nope&generation=%d", first.Generation), "user", nil, nil)
	assertChatResponse(t, invalidChanges, http.StatusBadRequest, "chat_page_invalid")
	missingGeneration := h.request(t, http.MethodGet, "/api/chat/changes?afterSeq=0", "user", nil, nil)
	assertChatResponse(t, missingGeneration, http.StatusBadRequest, "chat_generation_invalid")
}

func TestChatMessageInputValidationBoundaries(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)

	t.Run("character boundaries and controls", func(t *testing.T) {
		h := newChatAPIHarness(t, now, nil)
		h.addSession(t, "user", "user")
		for _, tc := range []struct {
			name        string
			contentType string
			body        []byte
			status      int
			code        string
		}{
			{name: "2000 characters", contentType: "application/json; charset=utf-8", body: jsonChatBody(strings.Repeat("a", 2000)), status: http.StatusCreated},
			{name: "2001 characters", contentType: "application/json", body: jsonChatBody(strings.Repeat("a", 2001)), status: http.StatusRequestEntityTooLarge, code: "chat_message_too_large"},
			{name: "trimmed empty", contentType: "application/json", body: jsonChatBody(" \n\t "), status: http.StatusBadRequest, code: "chat_message_empty"},
			{name: "NUL", contentType: "application/json", body: []byte(`{"body":"x\u0000y"}`), status: http.StatusBadRequest, code: "chat_message_control_character"},
			{name: "vertical tab", contentType: "application/json", body: []byte(`{"body":"\u000btext"}`), status: http.StatusBadRequest, code: "chat_message_control_character"},
			{name: "bidi override", contentType: "application/json", body: []byte(`{"body":"safe\u202etxt"}`), status: http.StatusBadRequest, code: "chat_message_control_character"},
			{name: "newlines and tabs", contentType: "application/json", body: jsonChatBody("line one\n\tline two"), status: http.StatusCreated},
			{name: "unknown field", contentType: "application/json", body: []byte(`{"body":"ok","attachment":"no"}`), status: http.StatusBadRequest, code: "chat_request_invalid"},
			{name: "malformed JSON", contentType: "application/json", body: []byte(`{"body":`), status: http.StatusBadRequest, code: "chat_request_invalid"},
			{name: "trailing JSON", contentType: "application/json", body: []byte(`{"body":"ok"}{}`), status: http.StatusBadRequest, code: "chat_request_invalid"},
			{name: "wrong content type", contentType: "text/plain", body: []byte(`{"body":"ok"}`), status: http.StatusUnsupportedMediaType, code: "chat_content_type_invalid"},
			{name: "invalid UTF-8", contentType: "application/json", body: []byte{'{', '"', 'b', 'o', 'd', 'y', '"', ':', '"', 0xff, '"', '}'}, status: http.StatusBadRequest, code: "chat_request_invalid"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				resp := h.request(t, http.MethodPost, "/api/chat/messages", "user", tc.body, map[string]string{"Content-Type": tc.contentType})
				assertChatResponse(t, resp, tc.status, tc.code)
			})
		}
	})

	t.Run("decoded byte limit remains enforced below raw limit", func(t *testing.T) {
		h := newChatAPIHarness(t, now, func(cfg *config.Config) {
			cfg.Chat.MaxMessageChars = 100
			cfg.Chat.MaxMessageBytes = 100
		})
		h.addSession(t, "user", "user")
		over := h.request(t, http.MethodPost, "/api/chat/messages", "user", jsonChatBody(strings.Repeat("😀", 30)), map[string]string{"Content-Type": "application/json"})
		assertChatResponse(t, over, http.StatusRequestEntityTooLarge, "chat_message_too_large")
	})

	t.Run("decoded character limit remains enforced below raw limit", func(t *testing.T) {
		h := newChatAPIHarness(t, now, func(cfg *config.Config) {
			cfg.Chat.MaxMessageChars = 10
			cfg.Chat.MaxMessageBytes = 100
		})
		h.addSession(t, "user", "user")
		over := h.request(t, http.MethodPost, "/api/chat/messages", "user", jsonChatBody(strings.Repeat("x", 11)), map[string]string{"Content-Type": "application/json"})
		assertChatResponse(t, over, http.StatusRequestEntityTooLarge, "chat_message_too_large")
	})
}

func TestChatRawRequestBodyStrict8192ByteLimit(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	h := newChatAPIHarness(t, now, nil)
	h.addSession(t, "user", "user")

	t.Run("content length is rejected before reading", func(t *testing.T) {
		app := fiber.New()
		var requestContext fasthttp.RequestCtx
		ctx := app.AcquireCtx(&requestContext)
		defer app.ReleaseCtx(ctx)
		reader := &chatTrackingReader{reader: strings.NewReader("must not be read")}
		ctx.Request().SetBodyStream(reader, chatMaxRequestBytes+1)
		if length := ctx.Request().Header.ContentLength(); length != chatMaxRequestBytes+1 {
			t.Fatalf("fixture Content-Length=%d", length)
		}
		_, err := readBoundedChatBody(ctx)
		assertCodedChatError(t, err, http.StatusRequestEntityTooLarge, "chat_request_too_large")
		if reader.readBytes != 0 {
			t.Fatalf("oversized Content-Length body was read: %d bytes", reader.readBytes)
		}
	})

	t.Run("unknown length stream reads at most 8193", func(t *testing.T) {
		app := fiber.New()
		var requestContext fasthttp.RequestCtx
		ctx := app.AcquireCtx(&requestContext)
		defer app.ReleaseCtx(ctx)
		reader := &chatTrackingReader{reader: bytes.NewReader(bytes.Repeat([]byte{'x'}, chatMaxRequestBytes+100))}
		ctx.Request().SetBodyStream(reader, -1)
		if length := ctx.Request().Header.ContentLength(); length >= 0 {
			t.Fatalf("unknown-length fixture unexpectedly has Content-Length=%d", length)
		}
		_, err := readBoundedChatBody(ctx)
		assertCodedChatError(t, err, http.StatusRequestEntityTooLarge, "chat_request_too_large")
		if reader.readBytes != chatMaxRequestBytes+1 {
			t.Fatalf("unknown-length body read=%d want=%d", reader.readBytes, chatMaxRequestBytes+1)
		}
	})

	t.Run("chunked 8193 byte request", func(t *testing.T) {
		payload := paddedChatJSONBody("ok", chatMaxRequestBytes+1)
		response := h.requestBodyStream(t, http.MethodPost, "/api/chat/messages", "user", payload, -1, map[string]string{"Content-Type": "application/json"})
		assertChatResponse(t, response, http.StatusRequestEntityTooLarge, "chat_request_too_large")
	})

	t.Run("small body with excessive JSON whitespace", func(t *testing.T) {
		payload := paddedChatJSONBody("ok", chatMaxRequestBytes+1)
		response := h.requestBodyStream(t, http.MethodPost, "/api/chat/messages", "user", payload, len(payload), map[string]string{"Content-Type": "application/json"})
		assertChatResponse(t, response, http.StatusRequestEntityTooLarge, "chat_request_too_large")
	})

	t.Run("unicode escapes exceed raw limit", func(t *testing.T) {
		payload := []byte(`{"body":"` + strings.Repeat(`\u0061`, 1400) + `"}`)
		if len(payload) <= chatMaxRequestBytes {
			t.Fatalf("unicode escape fixture is not oversized: %d", len(payload))
		}
		response := h.requestBodyStream(t, http.MethodPost, "/api/chat/messages", "user", payload, len(payload), map[string]string{"Content-Type": "application/json"})
		assertChatResponse(t, response, http.StatusRequestEntityTooLarge, "chat_request_too_large")
		if bytes.Contains(response.body, []byte(`\u0061`)) {
			t.Fatalf("oversized request content was echoed: %s", response.body)
		}
	})

	t.Run("exact 8192 byte valid request", func(t *testing.T) {
		payload := paddedChatJSONBody("boundary", chatMaxRequestBytes)
		response := h.request(t, http.MethodPost, "/api/chat/messages", "user", payload, map[string]string{"Content-Type": "application/json"})
		assertChatResponse(t, response, http.StatusCreated, "")
	})
}

func TestChatSendRateLimitsAndTrustedProxyIP(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)

	for _, tc := range []struct {
		name   string
		adjust func(*config.Config)
		first  chatRateRequest
		second chatRateRequest
	}{
		{
			name: "session", adjust: func(cfg *config.Config) { cfg.Chat.SessionMessagesPerMinute = 1 },
			first: chatRateRequest{sid: "a", ip: "198.51.100.1"}, second: chatRateRequest{sid: "a", ip: "198.51.100.1"},
		},
		{
			name: "IP", adjust: func(cfg *config.Config) { cfg.Chat.IPMessagesPerMinute = 1 },
			first: chatRateRequest{sid: "a", ip: "198.51.100.2"}, second: chatRateRequest{sid: "b", ip: "198.51.100.2"},
		},
		{
			name: "global", adjust: func(cfg *config.Config) { cfg.Chat.GlobalMessagesPerMinute = 1 },
			first: chatRateRequest{sid: "a", ip: "198.51.100.3"}, second: chatRateRequest{sid: "b", ip: "198.51.100.4"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newChatAPIHarness(t, now, func(cfg *config.Config) {
				cfg.Server.TrustProxyHeaders = true
				cfg.Server.TrustedProxyCIDRs = []string{"0.0.0.0/0", "::/0"}
				tc.adjust(cfg)
			})
			h.addSession(t, "a", "user")
			h.addSession(t, "b", "user")
			first := h.request(t, http.MethodPost, "/api/chat/messages", tc.first.sid, jsonChatBody("first"), map[string]string{"Content-Type": "application/json", "X-Forwarded-For": tc.first.ip})
			assertChatResponse(t, first, http.StatusCreated, "")
			second := h.request(t, http.MethodPost, "/api/chat/messages", tc.second.sid, jsonChatBody("second"), map[string]string{"Content-Type": "application/json", "X-Forwarded-For": tc.second.ip})
			assertChatResponse(t, second, http.StatusTooManyRequests, "chat_send_rate_limited")
			if second.header.Get("Retry-After") == "" {
				t.Fatalf("rate-limited response omitted Retry-After")
			}
		})
	}

	t.Run("trusted and untrusted source IP", func(t *testing.T) {
		trusted := newChatAPIHarness(t, now, func(cfg *config.Config) {
			cfg.Server.TrustProxyHeaders = true
			cfg.Server.TrustedProxyCIDRs = []string{"0.0.0.0/0", "::/0"}
		})
		trusted.addSession(t, "user", "user")
		trusted.addSession(t, "admin", "admin")
		created := trusted.request(t, http.MethodPost, "/api/chat/messages", "user", jsonChatBody("trusted IP"), map[string]string{"Content-Type": "application/json", "X-Forwarded-For": "198.51.100.77"})
		assertChatResponse(t, created, http.StatusCreated, "")
		list := trusted.request(t, http.MethodGet, "/api/admin/chat/messages", "admin", nil, nil)
		var page chatMessagesResponse[adminChatMessageDTO]
		decodeChatResponse(t, list, &page)
		if len(page.Messages) != 1 || page.Messages[0].SourceIP != "198.51.100.77" {
			t.Fatalf("trusted proxy IP not used: %+v", page)
		}

		untrusted := newChatAPIHarness(t, now, nil)
		untrusted.addSession(t, "user", "user")
		untrusted.addSession(t, "admin", "admin")
		created = untrusted.request(t, http.MethodPost, "/api/chat/messages", "user", jsonChatBody("untrusted IP"), map[string]string{"Content-Type": "application/json", "X-Forwarded-For": "198.51.100.88"})
		assertChatResponse(t, created, http.StatusCreated, "")
		list = untrusted.request(t, http.MethodGet, "/api/admin/chat/messages", "admin", nil, nil)
		decodeChatResponse(t, list, &page)
		if len(page.Messages) != 1 || page.Messages[0].SourceIP == "198.51.100.88" {
			t.Fatalf("untrusted X-Forwarded-For was accepted: %+v", page)
		}
	})
}

func TestChatActionLimiterIsFinite(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	h := newChatAPIHarness(t, now, nil)
	h.addSession(t, "user", "user")
	for index := 0; index < chatActionPerSessionPerMinute; index++ {
		resp := h.request(t, http.MethodPost, fmt.Sprintf("/api/chat/messages/%d/withdraw", index+1), "user", nil, nil)
		assertChatResponse(t, resp, http.StatusNotFound, "chat_message_not_found")
	}
	limited := h.request(t, http.MethodPost, "/api/chat/messages/9999/withdraw", "user", nil, nil)
	assertChatResponse(t, limited, http.StatusTooManyRequests, "chat_action_rate_limited")
	if limited.header.Get("Retry-After") == "" {
		t.Fatalf("chat action limiter omitted Retry-After")
	}
}

func TestChatCursorResetAPIForRetentionAndUntrustedHighWater(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	h := newChatAPIHarness(t, now, nil)
	h.addSession(t, "user", "user")
	h.addSession(t, "admin", "admin")

	emptyHistory := h.request(t, http.MethodGet, "/api/chat/messages", "user", nil, nil)
	assertChatResponse(t, emptyHistory, http.StatusOK, "")
	var empty chatMessagesResponse[chatMessageDTO]
	decodeChatResponse(t, emptyHistory, &empty)
	if empty.Generation < 1 || empty.LatestChangeSeq != 0 {
		t.Fatalf("unexpected empty history sync state: %+v", empty)
	}
	matchingEmpty := h.request(t, http.MethodGet, fmt.Sprintf("/api/chat/changes?afterSeq=0&generation=%d", empty.Generation), "user", nil, nil)
	assertChatResponse(t, matchingEmpty, http.StatusOK, "")
	var matchingPage chatChangesResponse[chatMessageDTO]
	decodeChatResponse(t, matchingEmpty, &matchingPage)
	if len(matchingPage.Changes) != 0 || matchingPage.NextAfterSeq != 0 || matchingPage.Generation != empty.Generation {
		t.Fatalf("matching-generation empty response: %+v", matchingPage)
	}
	for _, path := range []string{
		"/api/chat/changes?afterSeq=0",
		"/api/chat/changes?afterSeq=0&generation=0",
		"/api/chat/changes?afterSeq=0&generation=nope",
	} {
		response := h.request(t, http.MethodGet, path, "user", nil, nil)
		assertChatResponse(t, response, http.StatusBadRequest, "chat_generation_invalid")
	}
	highCursor := h.request(t, http.MethodGet, fmt.Sprintf("/api/chat/changes?afterSeq=1&generation=%d", empty.Generation), "user", nil, nil)
	assertChatResponse(t, highCursor, http.StatusConflict, "chat_cursor_reset_required")

	createdResponse := h.request(t, http.MethodPost, "/api/chat/messages", "user", jsonChatBody("retention cursor"), map[string]string{"Content-Type": "application/json"})
	assertChatResponse(t, createdResponse, http.StatusCreated, "")
	var created chatMutationResponse[chatMessageDTO]
	decodeChatResponse(t, createdResponse, &created)
	historyResponse := h.request(t, http.MethodGet, "/api/chat/messages", "user", nil, nil)
	var before chatMessagesResponse[chatMessageDTO]
	decodeChatResponse(t, historyResponse, &before)
	if removed, err := h.store.CleanupChat(now.AddDate(0, 0, 2), 1, 50000, 500); err != nil || removed != 1 {
		t.Fatalf("retention cleanup removed=%d err=%v", removed, err)
	}
	staleWithoutLaterEvent := h.request(t, http.MethodGet, fmt.Sprintf("/api/chat/changes?afterSeq=%d&generation=%d", created.EventSeq, before.Generation), "user", nil, nil)
	assertChatResponse(t, staleWithoutLaterEvent, http.StatusConflict, "chat_cursor_reset_required")
	adminStale := h.request(t, http.MethodGet, fmt.Sprintf("/api/admin/chat/changes?afterSeq=%d&generation=%d", created.EventSeq, before.Generation), "admin", nil, nil)
	assertChatResponse(t, adminStale, http.StatusConflict, "chat_cursor_reset_required")

	afterCleanupResponse := h.request(t, http.MethodGet, "/api/chat/messages", "user", nil, nil)
	var afterCleanup chatMessagesResponse[chatMessageDTO]
	decodeChatResponse(t, afterCleanupResponse, &afterCleanup)
	if afterCleanup.Generation == before.Generation || afterCleanup.LatestChangeSeq != created.EventSeq || len(afterCleanup.Messages) != 0 {
		t.Fatalf("history did not expose reset generation: before=%+v after=%+v", before, afterCleanup)
	}
	normalEmpty := h.request(t, http.MethodGet, fmt.Sprintf("/api/chat/changes?afterSeq=%d&generation=%d", afterCleanup.LatestChangeSeq, afterCleanup.Generation), "user", nil, nil)
	assertChatResponse(t, normalEmpty, http.StatusOK, "")

	laterResponse := h.request(t, http.MethodPost, "/api/chat/messages", "user", jsonChatBody("later event"), map[string]string{"Content-Type": "application/json"})
	assertChatResponse(t, laterResponse, http.StatusCreated, "")
	var later chatMutationResponse[chatMessageDTO]
	decodeChatResponse(t, laterResponse, &later)
	staleWithLaterEvent := h.request(t, http.MethodGet, fmt.Sprintf("/api/chat/changes?afterSeq=%d&generation=%d", created.EventSeq, before.Generation), "user", nil, nil)
	assertChatResponse(t, staleWithLaterEvent, http.StatusConflict, "chat_cursor_reset_required")
	currentChanges := h.request(t, http.MethodGet, fmt.Sprintf("/api/chat/changes?afterSeq=%d&generation=%d", created.EventSeq, afterCleanup.Generation), "user", nil, nil)
	assertChatResponse(t, currentChanges, http.StatusOK, "")
	var currentPage chatChangesResponse[chatMessageDTO]
	decodeChatResponse(t, currentChanges, &currentPage)
	if len(currentPage.Changes) != 1 || currentPage.Changes[0].Seq != later.EventSeq || currentPage.Generation != afterCleanup.Generation {
		t.Fatalf("later event after retention: %+v", currentPage)
	}
}

func TestChatCapabilitiesDefaultCustomHotUpdateAndAuthentication(t *testing.T) {
	now := time.Now().UTC()
	defaults := newChatAPIHarness(t, now, nil)
	defaults.addSession(t, "default-user", "user")
	defaultResponse := defaults.request(t, http.MethodGet, "/api/chat/capabilities", "default-user", nil, nil)
	assertChatResponse(t, defaultResponse, http.StatusOK, "")
	var defaultCapabilities chatCapabilitiesResponse
	decodeChatResponse(t, defaultResponse, &defaultCapabilities)
	if defaultCapabilities.MaxMessageChars != 2000 || defaultCapabilities.MaxMessageBytes != 8192 || defaultCapabilities.MaxRequestBytes != chatMaxRequestBytes || defaultCapabilities.WithdrawWindowSeconds != 300 || defaultCapabilities.HistoryDefaultLimit != 50 || defaultCapabilities.HistoryMaxLimit != 100 || defaultCapabilities.ChangesDefaultLimit != 50 || defaultCapabilities.ChangesMaxLimit != 500 {
		t.Fatalf("default capabilities: %+v", defaultCapabilities)
	}

	h := newChatAPIHarness(t, now, func(cfg *config.Config) {
		cfg.Chat.MaxMessageChars = 1234
		cfg.Chat.MaxMessageBytes = 5678
		cfg.Chat.WithdrawWindowSeconds = 456
	})
	h.addSession(t, "user", "user")
	h.addSession(t, "admin", "admin")
	unauthenticated := h.request(t, http.MethodGet, "/api/chat/capabilities", "", nil, nil)
	assertChatResponse(t, unauthenticated, http.StatusUnauthorized, "")
	for _, sid := range []string{"user", "admin"} {
		response := h.request(t, http.MethodGet, "/api/chat/capabilities", sid, nil, nil)
		assertChatResponse(t, response, http.StatusOK, "")
		var capabilities chatCapabilitiesResponse
		decodeChatResponse(t, response, &capabilities)
		if capabilities.MaxMessageChars != 1234 || capabilities.MaxMessageBytes != 5678 || capabilities.MaxRequestBytes != chatMaxRequestBytes || capabilities.WithdrawWindowSeconds != 456 || capabilities.HistoryDefaultLimit != 50 || capabilities.HistoryMaxLimit != 100 || capabilities.ChangesDefaultLimit != 50 || capabilities.ChangesMaxLimit != 500 {
			t.Fatalf("custom capabilities for %s: %+v", sid, capabilities)
		}
	}
	next := *h.server.cfg()
	next.Chat.MaxMessageChars = 2000
	next.Chat.MaxMessageBytes = 8192
	next.Chat.WithdrawWindowSeconds = 300
	h.server.replaceConfig(&next)
	updatedResponse := h.request(t, http.MethodGet, "/api/chat/capabilities", "user", nil, nil)
	assertChatResponse(t, updatedResponse, http.StatusOK, "")
	var updated chatCapabilitiesResponse
	decodeChatResponse(t, updatedResponse, &updated)
	if updated.MaxMessageChars != 2000 || updated.MaxMessageBytes != 8192 || updated.MaxRequestBytes != chatMaxRequestBytes || updated.WithdrawWindowSeconds != 300 {
		t.Fatalf("capabilities did not hot-read config: %+v", updated)
	}
}

func TestChatMutationEventSeqIsNotGlobalCursor(t *testing.T) {
	now := time.Now().UTC()
	h := newChatAPIHarness(t, now, nil)
	h.addSession(t, "first", "user")
	h.addSession(t, "second", "user")
	historyResponse := h.request(t, http.MethodGet, "/api/chat/messages", "first", nil, nil)
	var snapshot chatMessagesResponse[chatMessageDTO]
	decodeChatResponse(t, historyResponse, &snapshot)
	interleaved := h.request(t, http.MethodPost, "/api/chat/messages", "second", jsonChatBody("interleaved"), map[string]string{"Content-Type": "application/json"})
	assertChatResponse(t, interleaved, http.StatusCreated, "")
	mutation := h.request(t, http.MethodPost, "/api/chat/messages", "first", jsonChatBody("own mutation"), map[string]string{"Content-Type": "application/json"})
	assertChatResponse(t, mutation, http.StatusCreated, "")
	if bytes.Contains(mutation.body, []byte("changeSeq")) || !bytes.Contains(mutation.body, []byte("eventSeq")) {
		t.Fatalf("mutation sequence contract is ambiguous: %s", mutation.body)
	}
	var result chatMutationResponse[chatMessageDTO]
	decodeChatResponse(t, mutation, &result)
	changesResponse := h.request(t, http.MethodGet, fmt.Sprintf("/api/chat/changes?afterSeq=%d&generation=%d", snapshot.LatestChangeSeq, snapshot.Generation), "first", nil, nil)
	assertChatResponse(t, changesResponse, http.StatusOK, "")
	var changes chatChangesResponse[chatMessageDTO]
	decodeChatResponse(t, changesResponse, &changes)
	if len(changes.Changes) != 2 || changes.Changes[0].Seq >= result.EventSeq || changes.NextAfterSeq != result.EventSeq {
		t.Fatalf("eventSeq was not distinct from paged cursor semantics: mutation=%+v changes=%+v", result, changes)
	}
}

func TestChatFailureAuditDTOClassification(t *testing.T) {
	h := newChatAPIHarness(t, time.Now().UTC(), nil)
	h.addSession(t, "user", "user")
	h.addSession(t, "admin", "admin")
	missing := h.request(t, http.MethodPost, "/api/chat/messages/999/withdraw", "user", nil, nil)
	assertChatResponse(t, missing, http.StatusNotFound, "chat_message_not_found")
	createdResponse := h.request(t, http.MethodPost, "/api/chat/messages", "user", jsonChatBody("audit body must stay private"), map[string]string{"Content-Type": "application/json"})
	assertChatResponse(t, createdResponse, http.StatusCreated, "")
	var created chatMutationResponse[chatMessageDTO]
	decodeChatResponse(t, createdResponse, &created)
	success := h.request(t, http.MethodPost, fmt.Sprintf("/api/chat/messages/%d/withdraw", created.Message.ID), "user", nil, nil)
	assertChatResponse(t, success, http.StatusOK, "")

	requestAudit := func(status string) auditPageDTO {
		response := h.request(t, http.MethodGet, "/api/admin/audit?page=1&pageSize=20&status="+status, "admin", nil, nil)
		var page auditPageDTO
		decodeChatResponse(t, response, &page)
		return page
	}
	failed := requestAudit("failed")
	if failed.Total != 1 || len(failed.Logs) != 1 || failed.Logs[0].Action != "chat_withdraw_failed" || failed.Logs[0].Status != "failed" || failed.Logs[0].Success || failed.Logs[0].ActionLabel != "撤回聊天消息失败" || strings.Contains(failed.Logs[0].Detail, "audit body") {
		t.Fatalf("failed chat audit DTO: %+v", failed)
	}
	ok := requestAudit("ok")
	found := false
	for _, entry := range ok.Logs {
		if entry.Action == "chat_withdraw" {
			found = entry.Success && entry.Status == "ok" && entry.ActionLabel == "撤回聊天消息"
		}
	}
	if !found {
		t.Fatalf("successful chat audit DTO missing: %+v", ok)
	}
}

func TestChatMaintenanceUsesConfiguredRetentionBatch(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	st, err := store.Open(filepath.Join(t.TempDir(), "chat-maintenance.db"), 1000)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(t.TempDir())
	cfg.Chat.RetentionDays = 1
	cfg.Chat.MaxMessages = 2
	cfg.Chat.CleanupBatch = 1
	for index := 0; index < 4; index++ {
		if _, _, err := st.CreateChatMessage(store.ChatCreateInput{AuthorKey: "owner", AuthorTag: "访客-AAAAAA", AuthorRole: "user", SourceIP: "192.0.2.1", Body: "maintenance", CreatedAt: now.Add(-time.Duration(4-index) * time.Minute)}); err != nil {
			t.Fatalf("create maintenance message: %v", err)
		}
	}
	runtime := newRuntime(st)
	defer runtime.cancel()
	runtime.server = &Server{config: cfg}
	runtime.runStoreMaintenance()
	messages, _, err := st.ChatMessages(0, 10)
	if err != nil || len(messages) != 2 {
		t.Fatalf("maintenance did not catch up across batches messages=%d err=%v", len(messages), err)
	}
}

type chatRateRequest struct {
	sid string
	ip  string
}

type chatAPIHarness struct {
	app     *fiber.App
	server  *Server
	runtime *Runtime
	store   *store.Store
	now     time.Time
}

type capturedChatResponse struct {
	status int
	header http.Header
	body   []byte
}

type chatTrackingReader struct {
	reader    io.Reader
	readBytes int
}

func (reader *chatTrackingReader) Read(buffer []byte) (int, error) {
	read, err := reader.reader.Read(buffer)
	reader.readBytes += read
	return read, err
}

func newChatAPIHarness(t *testing.T, now time.Time, adjust func(*config.Config)) *chatAPIHarness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "chat-api.db"), 1000)
	if err != nil {
		t.Fatalf("open chat API store: %v", err)
	}
	cfg := testConfig(t.TempDir())
	if adjust != nil {
		adjust(cfg)
	}
	runtime, err := NewRuntimeWithOptions(cfg, st, "", Options{DevFrontendPort: 5173, chatNow: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("new chat API server: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Shutdown() })
	return &chatAPIHarness{app: runtime.App, server: runtime.server, runtime: runtime, store: st, now: now}
}

func (h *chatAPIHarness) addSession(t *testing.T, sid, role string) {
	t.Helper()
	name := ""
	if role == "admin" {
		name = "admin"
	}
	if err := h.store.CreateSessionWithIdle(sid, time.Now().Add(time.Hour), time.Now().Add(time.Hour), role, name); err != nil {
		t.Fatalf("create %s chat session: %v", role, err)
	}
}

func (h *chatAPIHarness) sessionHash(t *testing.T, sid string) string {
	t.Helper()
	session, err := h.store.Session(sid)
	if err != nil {
		t.Fatalf("load chat session: %v", err)
	}
	return session.ID
}

func (h *chatAPIHarness) request(t *testing.T, method, path, sid string, body []byte, headers map[string]string) capturedChatResponse {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	if sid != "" {
		request.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := h.app.Test(request)
	if err != nil {
		t.Fatalf("chat request %s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read chat response: %v", err)
	}
	return capturedChatResponse{status: response.StatusCode, header: response.Header.Clone(), body: content}
}

func (h *chatAPIHarness) requestBodyStream(t *testing.T, method, path, sid string, body []byte, bodySize int, headers map[string]string) capturedChatResponse {
	t.Helper()
	var requestContext fasthttp.RequestCtx
	requestContext.Request.Header.SetMethod(method)
	requestContext.Request.SetRequestURI(path)
	if sid != "" {
		requestContext.Request.Header.SetCookie("sid", sid)
	}
	for name, value := range headers {
		requestContext.Request.Header.Set(name, value)
	}
	requestContext.Request.SetBodyStream(bytes.NewReader(body), bodySize)
	h.app.Handler()(&requestContext)
	header := make(http.Header)
	requestContext.Response.Header.VisitAll(func(key, value []byte) {
		header.Add(string(key), string(value))
	})
	return capturedChatResponse{
		status: requestContext.Response.StatusCode(),
		header: header,
		body:   append([]byte(nil), requestContext.Response.Body()...),
	}
}

func assertChatResponse(t *testing.T, response capturedChatResponse, status int, code string) {
	t.Helper()
	if response.status != status {
		t.Fatalf("status=%d want=%d body=%s", response.status, status, response.body)
	}
	if response.header.Get("Cache-Control") != "no-store" || response.header.Get("Pragma") != "no-cache" {
		t.Fatalf("chat response lacks no-store headers: %+v", response.header)
	}
	if code != "" {
		var payload map[string]any
		if err := json.Unmarshal(response.body, &payload); err != nil {
			t.Fatalf("decode chat error: %v body=%s", err, response.body)
		}
		if payload["code"] != code {
			t.Fatalf("error code=%v want=%q body=%s", payload["code"], code, response.body)
		}
	}
}

func decodeChatResponse(t *testing.T, response capturedChatResponse, target any) {
	t.Helper()
	if err := json.Unmarshal(response.body, target); err != nil {
		t.Fatalf("decode chat response: %v body=%s", err, response.body)
	}
}

func jsonChatBody(body string) []byte {
	payload, _ := json.Marshal(map[string]string{"body": body})
	return payload
}

func paddedChatJSONBody(body string, totalBytes int) []byte {
	payload := jsonChatBody(body)
	if len(payload) > totalBytes {
		panic("chat JSON fixture exceeds requested size")
	}
	return append(payload, bytes.Repeat([]byte{' '}, totalBytes-len(payload))...)
}

func assertCodedChatError(t *testing.T, err error, status int, code string) {
	t.Helper()
	apiErr, ok := err.(*codedAPIError)
	if !ok || apiErr.status != status || apiErr.code != code {
		t.Fatalf("chat error=%T %v want status=%d code=%q", err, err, status, code)
	}
}

func findOrdinaryChatMessage(t *testing.T, messages []chatMessageDTO, id int64) chatMessageDTO {
	t.Helper()
	for _, message := range messages {
		if message.ID == id {
			return message
		}
	}
	t.Fatalf("ordinary message %d not found: %+v", id, messages)
	return chatMessageDTO{}
}

func findAdminChatMessage(t *testing.T, messages []adminChatMessageDTO, id int64) adminChatMessageDTO {
	t.Helper()
	for _, message := range messages {
		if message.ID == id {
			return message
		}
	}
	t.Fatalf("admin message %d not found: %+v", id, messages)
	return adminChatMessageDTO{}
}

func assertChatChangeKinds(t *testing.T, changes []chatChangeDTO[chatMessageDTO], want []string) {
	t.Helper()
	if len(changes) != len(want) {
		t.Fatalf("change count=%d want=%d", len(changes), len(want))
	}
	for index := range want {
		if changes[index].Kind != want[index] {
			t.Fatalf("change[%d].kind=%q want=%q", index, changes[index].Kind, want[index])
		}
	}
}
