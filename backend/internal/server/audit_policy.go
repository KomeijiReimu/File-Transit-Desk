package server

import (
	"errors"
	"log"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"filetrans-backend/internal/store"
)

func (s *Server) sampledAudit(action, ip, routeTemplate, detail string) {
	cfg := s.cfg().Audit
	s.limiterMu.Lock()
	if s.auditLimiter == nil {
		s.auditLimiter = newWindowLimiter()
	}
	limiter := s.auditLimiter
	s.limiterMu.Unlock()
	specs := []limitSpec{
		{Key: "audit-sample:" + action + ":" + ip + ":" + routeTemplate, Limit: 1, Window: time.Duration(cfg.UnauthorizedSampleSeconds) * time.Second},
		{Key: "audit-global:" + action, Limit: cfg.UnauthorizedGlobalPerMinute, Window: time.Minute},
	}
	if allowed, _ := limiter.AllowMany(specs); allowed {
		_ = s.store.Audit(action, ip, detail)
	}
}

func (s *Server) criticalAudit(action, ip, detail string) {
	if err := s.store.Audit(action, ip, detail); err != nil {
		if errors.Is(err, store.ErrAuditMaintenance) {
			log.Printf("[CRITICAL] event=audit_maintenance_failed action=%s", action)
			return
		}
		log.Printf("[CRITICAL] event=audit_write_failed action=%s", action)
	}
}

func (s *Server) bestEffortAudit(action, ip, detail string) {
	_ = s.store.Audit(action, ip, detail)
}

func (s *Server) sampledRequestAudit(c *fiber.Ctx, action, routeTemplate, detail string) {
	if strings.TrimSpace(routeTemplate) == "" {
		routeTemplate = "authenticated_api"
		if route := c.Route(); route != nil && strings.TrimSpace(route.Path) != "" {
			routeTemplate = route.Method + " " + route.Path
		}
	}
	s.sampledAudit(action, s.clientIP(c), routeTemplate, detail)
}
