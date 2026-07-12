package server

import (
	"errors"
	"strconv"
	"sync"
	"time"

	"filetrans-backend/internal/store"

	"github.com/gofiber/fiber/v2"
)

func (s *Server) windowRateLimiter() *windowLimiter {
	s.limiterMu.Lock()
	defer s.limiterMu.Unlock()
	if s.rateLimiter == nil {
		s.rateLimiter = newWindowLimiter()
	}
	if s.loginLimiter == nil {
		s.loginLimiter = newLoginLimiter()
	}
	return s.rateLimiter
}

func retryAfterSeconds(duration time.Duration) string {
	seconds := int((duration + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return strconv.Itoa(seconds)
}

func (s *Server) checkLoginAdmission(c *fiber.Ctx, kind, ip string) error {
	cfg := s.cfg().Abuse.Login
	s.windowRateLimiter()
	blocked, retry := s.loginLimiter.blocked(kind+":"+ip, cfg.IPMaxFailures, time.Duration(cfg.WindowSeconds)*time.Second, time.Duration(cfg.BlockSeconds)*time.Second)
	if blocked {
		c.Set("Retry-After", retryAfterSeconds(retry))
		s.sampledAudit("login_rate_limited", ip, "login:"+kind, "登录速率受限")
		return newCodedAPIError(fiber.StatusTooManyRequests, "login_rate_limited", "登录请求过于频繁，请稍后重试。")
	}
	if allowed, retry := s.rateLimiter.Allow("login-global:"+kind, cfg.GlobalPerMinute, time.Minute); !allowed {
		c.Set("Retry-After", retryAfterSeconds(retry))
		s.sampledAudit("login_rate_limited", ip, "login:"+kind, "登录速率受限")
		return newCodedAPIError(fiber.StatusTooManyRequests, "login_rate_limited", "登录请求过于频繁，请稍后重试。")
	}
	return nil
}

func (s *Server) recordLoginFailure(kind, ip string) {
	cfg := s.cfg().Abuse.Login
	s.loginLimiter.failure(kind+":"+ip, cfg.IPMaxFailures, time.Duration(cfg.WindowSeconds)*time.Second, time.Duration(cfg.BlockSeconds)*time.Second)
}

func (s *Server) checkTokenCreationRate(c *fiber.Ctx, sessionID string) error {
	cfg := s.cfg().Abuse.Creation
	limiter := s.windowRateLimiter()
	if allowed, retry := limiter.AllowMany([]limitSpec{{Key: "token-global", Limit: cfg.TokenGlobalPerMinute, Window: time.Minute}, {Key: "token-session:" + sessionID, Limit: cfg.TokenPerSessionPerMinute, Window: time.Minute}}); !allowed {
		return creationRateError(c, retry, "token_create_rate_limited", "令牌创建过于频繁，请稍后重试。")
	}
	return nil
}

func (s *Server) checkLeaseCreationRate(c *fiber.Ctx, owner string, public bool) error {
	cfg := s.cfg().Abuse.Creation
	limiter := s.windowRateLimiter()
	specs := []limitSpec{{Key: "lease-global", Limit: cfg.LeaseGlobalPerMinute, Window: time.Minute}, {Key: "lease-owner:" + owner, Limit: cfg.LeasePerOwnerPerMinute, Window: time.Minute}}
	if public {
		specs = append(specs, limitSpec{Key: "lease-public-ip:" + s.clientIP(c), Limit: cfg.PublicLeasePerIPPerMinute, Window: time.Minute})
	}
	if allowed, retry := limiter.AllowMany(specs); !allowed {
		return creationRateError(c, retry, "lease_create_rate_limited", "票据创建过于频繁，请稍后重试。")
	}
	return nil
}

func creationRateError(c *fiber.Ctx, retry time.Duration, code, message string) error {
	c.Set("Retry-After", retryAfterSeconds(retry))
	return newCodedAPIError(fiber.StatusTooManyRequests, code, message)
}

func outstandingLeaseError(c *fiber.Ctx) error {
	c.Set("Retry-After", "30")
	return newCodedAPIError(fiber.StatusTooManyRequests, "outstanding_lease_limit", "待使用票据数量已达上限，请稍后重试。")
}

func (s *Server) creationStoreError(c *fiber.Ctx, err error) error {
	if errors.Is(err, store.ErrOutstandingLeaseLimit) {
		return outstandingLeaseError(c)
	}
	if errors.Is(err, store.ErrActiveTokenLimitReached) {
		return newCodedAPIError(fiber.StatusForbidden, "active_token_limit_reached", "活跃令牌数量已达上限。")
	}
	return err
}

type windowLimitEntry struct {
	count     int
	expiresAt time.Time
	window    time.Duration
}

type limitSpec struct {
	Key    string
	Limit  int
	Window time.Duration
}

type windowLimiter struct {
	mu      sync.Mutex
	entries map[string]windowLimitEntry
	now     func() time.Time
}

func newWindowLimiter() *windowLimiter {
	return &windowLimiter{entries: map[string]windowLimitEntry{}, now: time.Now}
}

func (l *windowLimiter) Allow(key string, limit int, window time.Duration) (bool, time.Duration) {
	return l.AllowMany([]limitSpec{{Key: key, Limit: limit, Window: window}})
}

func (l *windowLimiter) AllowMany(specs []limitSpec) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.cleanupLocked(now)
	normalized := make(map[string]limitSpec, len(specs))
	disabled := make(map[string]bool, len(specs))
	for _, spec := range specs {
		if spec.Key == "" {
			continue
		}
		if spec.Limit <= 0 || spec.Window <= 0 {
			disabled[spec.Key] = true
			continue
		}
		if existing, ok := normalized[spec.Key]; ok {
			if spec.Limit < existing.Limit {
				existing.Limit = spec.Limit
			}
			if spec.Window > existing.Window {
				existing.Window = spec.Window
			}
			normalized[spec.Key] = existing
		} else {
			normalized[spec.Key] = spec
		}
	}
	for key := range disabled {
		if _, enabled := normalized[key]; !enabled {
			delete(l.entries, key)
		}
	}
	var retryAfter time.Duration
	for key, spec := range normalized {
		entry, ok := l.entries[key]
		if ok && entry.window != spec.Window {
			delete(l.entries, key)
			ok = false
		}
		if ok && entry.count >= spec.Limit {
			retry := entry.expiresAt.Sub(now)
			if retry < time.Second {
				retry = time.Second
			}
			if retry > retryAfter {
				retryAfter = retry
			}
		}
	}
	if retryAfter > 0 {
		return false, retryAfter
	}
	for key, spec := range normalized {
		entry, ok := l.entries[key]
		if !ok {
			entry = windowLimitEntry{expiresAt: now.Add(spec.Window), window: spec.Window}
		}
		entry.count++
		l.entries[key] = entry
	}
	return true, 0
}

func (l *windowLimiter) cleanupLocked(now time.Time) {
	for key, entry := range l.entries {
		if !now.Before(entry.expiresAt) {
			delete(l.entries, key)
		}
	}
}

func (l *windowLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

func (l *windowLimiter) count(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.entries[key].count
}

func (l *loginLimiter) blocked(key string, maxFailures int, window, block time.Duration) (bool, time.Duration) {
	if maxFailures <= 0 || window <= 0 || block <= 0 {
		return false, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.cleanupConfiguredLocked(now, window)
	attempt := l.attempts[key]
	if !attempt.blockedTil.IsZero() && now.Before(attempt.blockedTil) {
		return true, attempt.blockedTil.Sub(now)
	}
	return false, 0
}

func (l *loginLimiter) failure(key string, maxFailures int, window, block time.Duration) {
	if maxFailures <= 0 || window <= 0 || block <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.cleanupConfiguredLocked(now, window)
	attempt := l.attempts[key]
	if attempt.windowFrom.IsZero() || now.Sub(attempt.windowFrom) >= window {
		attempt = loginAttempt{windowFrom: now}
	}
	attempt.count++
	if attempt.count >= maxFailures {
		attempt.blockedTil = now.Add(block)
		attempt.count = 0
		attempt.windowFrom = now
	}
	l.attempts[key] = attempt
}

func (l *loginLimiter) cleanupConfiguredLocked(now time.Time, window time.Duration) {
	for key, attempt := range l.attempts {
		if !attempt.blockedTil.IsZero() && now.Before(attempt.blockedTil) {
			continue
		}
		if now.Sub(attempt.windowFrom) >= window {
			delete(l.attempts, key)
		}
	}
}
