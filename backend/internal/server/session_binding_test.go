package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filetrans-backend/internal/store"

	"github.com/gofiber/fiber/v2"
)

func TestSessionBindingIsStableOpaqueAndSessionSpecific(t *testing.T) {
	first := sessionBindingForID(strings.Repeat("a", 64))
	if first != sessionBindingForID(strings.Repeat("a", 64)) {
		t.Fatalf("session binding is not stable")
	}
	second := sessionBindingForID(strings.Repeat("b", 64))
	if first == second {
		t.Fatalf("different sessions received the same binding")
	}
	if len(first) != 32 {
		t.Fatalf("binding length=%d want 32 hex characters", len(first))
	}
	if first == strings.Repeat("a", 64) || strings.Contains(strings.Repeat("a", 64), first) {
		t.Fatalf("binding exposes the database session hash")
	}
}

func TestLoginMeHeartbeatAndRecoverableErrorReturnSessionBinding(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "binding.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(t.TempDir())
	cfg.Auth.TOTPSecret = ""
	cfg.Auth.DevAllowFixedCode = true
	app, err := NewWithOptions(cfg, st, "", Options{DevMode: true, DevFrontendPort: 5173})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	userLogin := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"code":"000000"}`))
	userLogin.Header.Set("Content-Type", "application/json")
	userResponse, err := app.Test(userLogin)
	if err != nil {
		t.Fatalf("user login: %v", err)
	}
	var userPayload map[string]any
	decodeJSON(t, userResponse, &userPayload)
	userBinding, _ := userPayload["sessionBinding"].(string)
	if userResponse.StatusCode != http.StatusOK || userBinding == "" {
		t.Fatalf("user login binding missing: status=%d payload=%v", userResponse.StatusCode, userPayload)
	}
	userCookie := responseCookie(t, userResponse, "sid")
	userSession, err := st.Session(userCookie.Value)
	if err != nil {
		t.Fatalf("load user session: %v", err)
	}
	if userBinding != sessionBindingForID(userSession.ID) || userBinding == userSession.ID {
		t.Fatalf("user binding is not the opaque stored-session projection")
	}

	meRequest := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	meRequest.AddCookie(userCookie)
	meResponse, err := app.Test(meRequest)
	if err != nil {
		t.Fatalf("me request: %v", err)
	}
	var mePayload map[string]any
	decodeJSON(t, meResponse, &mePayload)
	if mePayload["sessionBinding"] != userBinding {
		t.Fatalf("me binding=%v want=%q", mePayload["sessionBinding"], userBinding)
	}

	heartbeatRequest := httptest.NewRequest(http.MethodPost, "/api/auth/heartbeat", nil)
	heartbeatRequest.AddCookie(userCookie)
	heartbeatRequest.Header.Set(sessionBindingHeader, userBinding)
	heartbeatResponse, err := app.Test(heartbeatRequest)
	if err != nil {
		t.Fatalf("heartbeat request: %v", err)
	}
	var heartbeatPayload map[string]any
	decodeJSON(t, heartbeatResponse, &heartbeatPayload)
	if heartbeatPayload["sessionBinding"] != userBinding {
		t.Fatalf("heartbeat binding=%v want=%q", heartbeatPayload["sessionBinding"], userBinding)
	}

	adminLogin := httptest.NewRequest(http.MethodPost, "/api/auth/admin-login", strings.NewReader(`{"username":"admin","password":"secret"}`))
	adminLogin.Header.Set("Content-Type", "application/json")
	adminResponse, err := app.Test(adminLogin)
	if err != nil {
		t.Fatalf("admin login: %v", err)
	}
	var adminPayload map[string]any
	decodeJSON(t, adminResponse, &adminPayload)
	adminBinding, _ := adminPayload["sessionBinding"].(string)
	if adminResponse.StatusCode != http.StatusOK || adminBinding == "" || adminBinding == userBinding {
		t.Fatalf("admin login binding invalid: status=%d payload=%v", adminResponse.StatusCode, adminPayload)
	}

	now := time.Now()
	if err := st.CreateSessionWithIdle("recoverable-binding", now.Add(time.Hour), now.Add(-5*time.Second), "user", ""); err != nil {
		t.Fatalf("create recoverable session: %v", err)
	}
	recoverableSession, err := st.Session("recoverable-binding")
	if err != nil {
		t.Fatalf("load recoverable session: %v", err)
	}
	recoverableRequest := httptest.NewRequest(http.MethodGet, "/api/dirs", nil)
	recoverableRequest.AddCookie(&http.Cookie{Name: "sid", Value: "recoverable-binding"})
	recoverableResponse, err := app.Test(recoverableRequest)
	if err != nil {
		t.Fatalf("recoverable request: %v", err)
	}
	var recoverablePayload map[string]any
	decodeJSON(t, recoverableResponse, &recoverablePayload)
	if recoverableResponse.StatusCode != http.StatusUnauthorized || recoverablePayload["code"] != "session_idle_recoverable" || recoverablePayload["sessionBinding"] != sessionBindingForID(recoverableSession.ID) {
		t.Fatalf("recoverable binding missing: status=%d payload=%v", recoverableResponse.StatusCode, recoverablePayload)
	}
}

func TestAuthSessionBindingHeaderRejectsSubjectChangeBeforeHandler(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "binding-auth.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(t.TempDir())
	now := time.Now()
	if err := st.CreateSessionWithIdle("subject-a", now.Add(time.Hour), now.Add(time.Minute), "user", ""); err != nil {
		t.Fatalf("create session A: %v", err)
	}
	if err := st.CreateSessionWithIdle("subject-b", now.Add(time.Hour), now.Add(-5*time.Second), "admin", "admin"); err != nil {
		t.Fatalf("create session B: %v", err)
	}
	if err := st.CreateSessionWithIdle("expired-subject", now.Add(-time.Second), now.Add(-time.Second), "user", ""); err != nil {
		t.Fatalf("create expired session: %v", err)
	}
	sessionA, _ := st.Session("subject-a")
	sessionB, _ := st.Session("subject-b")
	bindingA := sessionBindingForID(sessionA.ID)
	bindingB := sessionBindingForID(sessionB.ID)

	s := &Server{config: cfg, store: st}
	executions := 0
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	app.Get("/protected", s.auth(func(c *fiber.Ctx) error {
		executions++
		return c.JSON(fiber.Map{"ok": true, "sessionBinding": c.Locals("sessionBinding")})
	}))
	app.Post("/api/auth/heartbeat", s.auth(s.heartbeat))

	request := func(cookie, binding string) (*http.Response, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/protected", nil)
		req.AddCookie(&http.Cookie{Name: "sid", Value: cookie})
		if binding != "" {
			req.Header.Set(sessionBindingHeader, binding)
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("protected request: %v", err)
		}
		var payload map[string]any
		decodeJSON(t, resp, &payload)
		return resp, payload
	}

	if resp, _ := request("subject-a", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("missing binding header must remain compatible, got %d", resp.StatusCode)
	}
	if resp, payload := request("subject-a", bindingA); resp.StatusCode != http.StatusOK || payload["sessionBinding"] != bindingA {
		t.Fatalf("matching binding rejected: status=%d payload=%v", resp.StatusCode, payload)
	}
	beforeRecoverable := executions
	if resp, payload := request("subject-b", ""); resp.StatusCode != http.StatusUnauthorized || payload["code"] != "session_idle_recoverable" || payload["sessionBinding"] != bindingB {
		t.Fatalf("admin recoverable binding response: status=%d payload=%v", resp.StatusCode, payload)
	}
	if executions != beforeRecoverable {
		t.Fatalf("protected handler executed for recoverable admin session")
	}
	beforeMismatch := executions
	if resp, payload := request("subject-a", bindingB); resp.StatusCode != http.StatusConflict || payload["code"] != "session_subject_changed" {
		t.Fatalf("mismatched binding response: status=%d payload=%v", resp.StatusCode, payload)
	}
	if executions != beforeMismatch {
		t.Fatalf("protected handler executed after binding mismatch")
	}
	if resp, payload := request("expired-subject", bindingA); resp.StatusCode != http.StatusUnauthorized || payload["code"] == "session_subject_changed" || payload["code"] == "session_idle_recoverable" {
		t.Fatalf("expired session was incorrectly treated as a subject change: status=%d payload=%v", resp.StatusCode, payload)
	}

	beforeHeartbeat, err := st.Session("subject-b")
	if err != nil {
		t.Fatalf("load heartbeat session: %v", err)
	}
	heartbeat := httptest.NewRequest(http.MethodPost, "/api/auth/heartbeat", nil)
	heartbeat.AddCookie(&http.Cookie{Name: "sid", Value: "subject-b"})
	heartbeat.Header.Set(sessionBindingHeader, bindingA)
	heartbeatResponse, err := app.Test(heartbeat)
	if err != nil {
		t.Fatalf("mismatched heartbeat: %v", err)
	}
	var heartbeatPayload map[string]any
	decodeJSON(t, heartbeatResponse, &heartbeatPayload)
	if heartbeatResponse.StatusCode != http.StatusConflict || heartbeatPayload["code"] != "session_subject_changed" {
		t.Fatalf("heartbeat subject mismatch: status=%d payload=%v", heartbeatResponse.StatusCode, heartbeatPayload)
	}
	afterHeartbeat, err := st.Session("subject-b")
	if err != nil {
		t.Fatalf("reload heartbeat session: %v", err)
	}
	if !afterHeartbeat.IdleExpiresAt.Equal(beforeHeartbeat.IdleExpiresAt) {
		t.Fatalf("mismatched heartbeat changed idle expiry")
	}
}

func responseCookie(t *testing.T, response *http.Response, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("response cookie %q missing", name)
	return nil
}
