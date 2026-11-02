package server

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"filetrans-backend/internal/config"
	"filetrans-backend/internal/security"
	"filetrans-backend/internal/store"
	"github.com/gofiber/fiber/v2"
)

// A fixed, non-authorizing PHC keeps missing/erased credentials on the same
// bounded verification path. Its verification result can never authorize login.
const dummyAccountPasswordHash = "$argon2id$v=19$m=65536,t=3,p=2$4haUFJXURhJtZKXnuF/agg$iSQV5tbkzMxH5JsjAJVbxhuoqOkcDGgoWNsxKX5r3cI"

func (s *Server) accountRoutes(app *fiber.App) {
	app.Get("/api/auth/options", s.authOptions)
	app.Post("/api/auth/register", s.registerAccount)
	app.Post("/api/auth/account-login", s.accountLogin)
	app.Post("/api/auth/change-password", s.auth(s.changePassword))
	app.Get("/api/admin/users", s.adminOnly(s.listAccounts))
	app.Post("/api/admin/users", s.adminOnly(s.createAccount))
	app.Put("/api/admin/users/:id", s.adminOnly(s.updateAccount))
	app.Post("/api/admin/users/:id/reset-password", s.adminOnly(s.resetAccountPassword))
	app.Delete("/api/admin/users/:id", s.adminOnly(s.deleteAccount))
	app.Put("/api/config/accounts", s.adminOnly(s.updateAccountPolicy))
}

func (s *Server) authOptions(c *fiber.Ctx) error {
	p := s.cfg().Accounts
	return c.JSON(fiber.Map{"accountLoginEnabled": true, "registrationEnabled": p.RegistrationEnabled, "registrationApprovalRequired": p.RegistrationApprovalRequired, "totpEnabled": p.TOTPEnabled})
}

// The same non-waiting bounded pool covers administrator verification and every
// account hash/verification. Expensive Argon2 work never holds a SQLite transaction.
func (s *Server) acquirePasswordSlot(c *fiber.Ctx) (func(), error) {
	if s.adminVerifySlots == nil {
		return func() {}, nil
	}
	select {
	case s.adminVerifySlots <- struct{}{}:
		return func() { <-s.adminVerifySlots }, nil
	default:
		c.Set("Retry-After", "1")
		return nil, newCodedAPIError(fiber.StatusServiceUnavailable, "auth_capacity_exhausted", "密码处理繁忙，请稍后重试。")
	}
}

func validateAccountPassword(password string) error {
	if len(password) < 8 || len(password) > 1024 || !utf8.ValidString(password) {
		return fiber.NewError(400, "密码长度须为 8 至 1024 字节。")
	}
	return nil
}

type accountCreateRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"displayName"`
	Status      string `json:"status"`
}

func parseAccountCreate(c *fiber.Ctx) (accountCreateRequest, error) {
	var in accountCreateRequest
	if err := protectAdminLoginBody(c); err != nil {
		return in, err
	}
	if err := c.BodyParser(&in); err != nil {
		return in, fiber.ErrBadRequest
	}
	in.Username = strings.ToLower(strings.TrimSpace(in.Username))
	if len(in.Username) < 3 || len(in.Username) > 64 {
		return in, fiber.NewError(400, "用户名须为 3 至 64 位字母、数字、点、下划线或短横线。")
	}
	for _, r := range in.Username {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return in, fiber.NewError(400, "用户名只能包含字母、数字、点、下划线或短横线。")
		}
	}
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	if utf8.RuneCountInString(in.DisplayName) > 80 {
		return in, fiber.NewError(400, "显示名称不能超过 80 个字符。")
	}
	if in.DisplayName == "" {
		in.DisplayName = in.Username
	}
	return in, validateAccountPassword(in.Password)
}

func accountStoreError(err error) error {
	if errors.Is(err, store.ErrUsernameTaken) {
		return fiber.NewError(409, err.Error())
	}
	if errors.Is(err, store.ErrAccountChanged) {
		return newCodedAPIError(409, "account_changed", err.Error())
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fiber.NewError(404, "账号不存在。")
	}
	return err
}

func (s *Server) registerAccount(c *fiber.Ctx) error {
	if !s.cfg().Accounts.RegistrationEnabled {
		return fiber.NewError(403, "当前未开放注册。")
	}
	if err := s.checkLoginAdmission(c, "register", s.clientIP(c)); err != nil {
		return err
	}
	in, err := parseAccountCreate(c)
	if err != nil {
		return err
	}
	if strings.EqualFold(in.Username, s.cfg().Auth.Admin.Username) {
		return fiber.NewError(409, "该用户名不可注册。")
	}
	release, err := s.acquirePasswordSlot(c)
	if err != nil {
		return err
	}
	defer release()
	hash, err := security.Hash([]byte(in.Password))
	if err != nil {
		return err
	}
	status := "active"
	if s.cfg().Accounts.RegistrationApprovalRequired {
		status = "pending"
	}
	_, err = s.store.CreateAccount(in.Username, in.DisplayName, hash, status)
	if err != nil {
		return accountStoreError(err)
	}
	s.recordLoginFailure("register", s.clientIP(c))
	s.criticalAudit("account_register", s.clientIP(c), "注册账号："+in.Username)
	return c.Status(201).JSON(fiber.Map{"status": status})
}

func (s *Server) accountLogin(c *fiber.Ctx) error {
	ip := s.clientIP(c)
	if err := s.checkLoginAdmission(c, "account", ip); err != nil {
		return err
	}
	if err := protectAdminLoginBody(c); err != nil {
		return err
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.BodyParser(&in); err != nil {
		return fiber.ErrBadRequest
	}
	if len(in.Username) > 64 || len(in.Password) > 1024 {
		return fiber.ErrBadRequest
	}
	u, err := s.store.AccountByUsername(strings.TrimSpace(in.Username))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	credentialsUnavailable := errors.Is(err, sql.ErrNoRows) || u.Status == "deleted" && u.PasswordHash == ""
	passwordHash := u.PasswordHash
	if credentialsUnavailable {
		passwordHash = dummyAccountPasswordHash
	}
	release, err := s.acquirePasswordSlot(c)
	if err != nil {
		return err
	}
	ok, err := security.Verify(passwordHash, []byte(in.Password))
	release()
	if err != nil {
		return err
	}
	if !ok || credentialsUnavailable {
		s.recordLoginFailure("account", ip)
		return fiber.NewError(401, "用户名或密码不正确。")
	}
	if u.Status != "active" {
		return fiber.NewError(403, "账号尚未通过审核、已停用或已删除。")
	}
	id, _, err := security.NewToken()
	if err != nil {
		return err
	}
	now := time.Now()
	expires := now.Add(time.Duration(s.cfg().Auth.SessionTTLSeconds) * time.Second)
	idle := s.sessionIdleExpiresAt(now, expires)
	if err := s.store.CreateAccountSession(id, expires, idle, u); err != nil {
		return accountStoreError(err)
	}
	sess, err := s.store.Session(id)
	if err != nil {
		return accountStoreError(err)
	}
	s.setSessionCookie(c, id, expires)
	s.loginLimiter.reset("account:" + ip)
	s.criticalAudit("login_success", ip, "账号登录："+u.Username)
	return c.JSON(fiber.Map{"authenticated": true, "role": "user", "userId": u.ID, "username": u.Username, "name": u.DisplayName, "authMethod": "account", "expiresAt": expires, "idleExpiresAt": idle, "sessionBinding": sessionBindingForID(sess.ID)})
}

func (s *Server) changePassword(c *fiber.Ctx) error {
	id, _ := c.Locals("userID").(int64)
	if id <= 0 {
		return fiber.NewError(403, "此登录方式不支持自行修改密码，请联系管理员。")
	}
	if err := protectAdminLoginBody(c); err != nil {
		return err
	}
	var in struct {
		Current string `json:"currentPassword"`
		New     string `json:"newPassword"`
	}
	if err := c.BodyParser(&in); err != nil {
		return fiber.ErrBadRequest
	}
	if len(in.Current) > 1024 {
		return fiber.ErrBadRequest
	}
	if err := validateAccountPassword(in.New); err != nil {
		return err
	}
	u, err := s.store.Account(id)
	if err != nil {
		return accountStoreError(err)
	}
	version, _ := c.Locals("authVersion").(int64)
	if u.AuthVersion != version || u.Status != "active" {
		return accountStoreError(store.ErrAccountChanged)
	}
	release, err := s.acquirePasswordSlot(c)
	if err != nil {
		return err
	}
	defer release()
	ok, err := security.Verify(u.PasswordHash, []byte(in.Current))
	if err != nil {
		return err
	}
	if !ok {
		return newCodedAPIError(fiber.StatusBadRequest, "current_password_incorrect", "当前密码不正确。")
	}
	hash, err := security.Hash([]byte(in.New))
	if err != nil {
		return err
	}
	if _, err := s.store.UpdateAccount(id, nil, nil, hash, &u.AuthVersion); err != nil {
		return accountStoreError(err)
	}
	s.clearSessionCookie(c)
	s.criticalAudit("account_password_change", s.clientIP(c), "账号："+u.Username)
	return c.JSON(fiber.Map{"ok": true})
}

func (s *Server) listAccounts(c *fiber.Ctx) error {
	page, size := c.QueryInt("page", 1), c.QueryInt("pageSize", 20)
	status := c.Query("status")
	if page < 1 || page > 1000000 || size < 1 || size > 100 || status != "" && status != "pending" && status != "active" && status != "disabled" && status != "deleted" {
		return fiber.ErrBadRequest
	}
	users, total, err := s.store.Accounts(strings.TrimSpace(c.Query("query")), status, page, size)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"users": users, "total": total, "page": page, "pageSize": size})
}

func (s *Server) createAccount(c *fiber.Ctx) error {
	in, err := parseAccountCreate(c)
	if err != nil {
		return err
	}
	if strings.EqualFold(in.Username, s.cfg().Auth.Admin.Username) {
		return fiber.NewError(409, "该用户名为管理员保留。")
	}
	if in.Status == "" {
		in.Status = "active"
	}
	if in.Status != "active" && in.Status != "pending" {
		return fiber.ErrBadRequest
	}
	release, err := s.acquirePasswordSlot(c)
	if err != nil {
		return err
	}
	defer release()
	hash, err := security.Hash([]byte(in.Password))
	if err != nil {
		return err
	}
	u, err := s.store.CreateAccount(in.Username, in.DisplayName, hash, in.Status)
	if err != nil {
		return accountStoreError(err)
	}
	s.criticalAudit("account_create", s.clientIP(c), "创建账号："+u.Username)
	return c.Status(201).JSON(u)
}

func accountID(c *fiber.Ctx) (int64, error) {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, fiber.ErrBadRequest
	}
	return id, nil
}

func (s *Server) updateAccount(c *fiber.Ctx) error {
	id, err := accountID(c)
	if err != nil {
		return err
	}
	var in struct {
		DisplayName *string `json:"displayName"`
		Status      *string `json:"status"`
	}
	if err := protectAdminLoginBody(c); err != nil {
		return err
	}
	if err := c.BodyParser(&in); err != nil {
		return fiber.ErrBadRequest
	}
	if in.DisplayName != nil {
		*in.DisplayName = strings.TrimSpace(*in.DisplayName)
		if utf8.RuneCountInString(*in.DisplayName) > 80 {
			return fiber.ErrBadRequest
		}
	}
	if in.Status != nil && *in.Status != "active" && *in.Status != "pending" && *in.Status != "disabled" {
		return fiber.ErrBadRequest
	}
	u, err := s.store.UpdateAccount(id, in.DisplayName, in.Status, "", nil)
	if err != nil {
		return accountStoreError(err)
	}
	s.criticalAudit("account_update", s.clientIP(c), "更新账号："+u.Username+"，状态："+u.Status)
	return c.JSON(u)
}

func (s *Server) resetAccountPassword(c *fiber.Ctx) error {
	id, err := accountID(c)
	if err != nil {
		return err
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := protectAdminLoginBody(c); err != nil {
		return err
	}
	if err := c.BodyParser(&in); err != nil {
		return fiber.ErrBadRequest
	}
	if err := validateAccountPassword(in.Password); err != nil {
		return err
	}
	release, err := s.acquirePasswordSlot(c)
	if err != nil {
		return err
	}
	defer release()
	hash, err := security.Hash([]byte(in.Password))
	if err != nil {
		return err
	}
	if _, err := s.store.UpdateAccount(id, nil, nil, hash, nil); err != nil {
		return accountStoreError(err)
	}
	s.criticalAudit("account_password_reset", s.clientIP(c), "账号 ID："+strconv.FormatInt(id, 10))
	return c.JSON(fiber.Map{"ok": true})
}

func (s *Server) deleteAccount(c *fiber.Ctx) error {
	id, err := accountID(c)
	if err != nil {
		return err
	}
	status := "deleted"
	if _, err := s.store.UpdateAccount(id, nil, &status, "", nil); err != nil {
		return accountStoreError(err)
	}
	s.criticalAudit("account_delete", s.clientIP(c), "账号 ID："+strconv.FormatInt(id, 10))
	return c.JSON(fiber.Map{"ok": true})
}

func (s *Server) updateAccountPolicy(c *fiber.Ctx) error {
	var in config.AccountPolicy
	if err := c.BodyParser(&in); err != nil {
		return fiber.ErrBadRequest
	}
	if err := s.updateConfig(func(next *config.Config) error { next.Accounts = in; return nil }); err != nil {
		return err
	}
	s.criticalAudit("config_accounts_update", s.clientIP(c), "更新注册与登录策略")
	return c.JSON(s.cfg().Accounts)
}
