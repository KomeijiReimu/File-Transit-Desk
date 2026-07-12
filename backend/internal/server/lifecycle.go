package server

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"filetrans-backend/internal/store"

	"github.com/gofiber/fiber/v2"
)

type Runtime struct {
	App    *fiber.App
	server *Server
	store  *store.Store
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	initialized atomic.Bool
	draining    atomic.Bool
	shutdown    sync.Once
	shutdownErr error
	activeMu    sync.Mutex
	activeCond  *sync.Cond
	active      int
}

func newRuntime(st *store.Store) *Runtime {
	ctx, cancel := context.WithCancel(context.Background())
	runtime := &Runtime{store: st, ctx: ctx, cancel: cancel}
	runtime.activeCond = sync.NewCond(&runtime.activeMu)
	return runtime
}

func (r *Runtime) BeginDrain() {
	if r != nil {
		r.draining.Store(true)
	}
}

func (r *Runtime) IsReady() bool {
	return r != nil && r.initialized.Load() && !r.draining.Load()
}

func (r *Runtime) Done() <-chan struct{} {
	if r == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return r.ctx.Done()
}

func (r *Runtime) Shutdown() error {
	if r == nil {
		return nil
	}
	r.shutdown.Do(func() {
		r.BeginDrain()
		r.waitForActiveRequests()
		if r.App != nil {
			r.shutdownErr = r.App.Shutdown()
		}
		r.cancel()
		r.wg.Wait()
		if r.store != nil && r.store.DB != nil {
			if err := r.store.DB.Close(); r.shutdownErr == nil {
				r.shutdownErr = err
			}
		}
	})
	return r.shutdownErr
}

func (r *Runtime) requestAdmission(c *fiber.Ctx) error {
	path := c.Path()
	if path == "/api/health" || path == "/api/health/live" || path == "/api/health/ready" {
		return c.Next()
	}
	r.activeMu.Lock()
	if r.draining.Load() {
		r.activeMu.Unlock()
		c.Set("Retry-After", "5")
		return newCodedAPIError(fiber.StatusServiceUnavailable, "server_draining", "服务正在安全停止，请稍后重试。")
	}
	r.active++
	r.activeMu.Unlock()
	defer func() {
		r.activeMu.Lock()
		r.active--
		if r.active == 0 {
			r.activeCond.Broadcast()
		}
		r.activeMu.Unlock()
	}()
	return c.Next()
}

func (r *Runtime) waitForActiveRequests() {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	for r.active > 0 {
		r.activeCond.Wait()
	}
}

func (r *Runtime) startMaintenance() {
	if r == nil || r.server == nil {
		return
	}
	r.server.maintenanceContext = func() context.Context { return r.ctx }
	r.server.maintenanceWG = &r.wg
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.runStartupMaintenance()
		r.server.triggerCurrentUploadTempCleanup(uploadCleanupSourceStartup)
		storeTicker := time.NewTicker(5 * time.Minute)
		defer storeTicker.Stop()
		interval := time.Duration(r.server.cfg().Storage.UploadTempCleanupIntervalSeconds) * time.Second
		var cleanupTicker *time.Ticker
		var cleanupC <-chan time.Time
		if interval > 0 {
			cleanupTicker = time.NewTicker(interval)
			cleanupC = cleanupTicker.C
			defer cleanupTicker.Stop()
		}
		for {
			select {
			case <-r.ctx.Done():
				return
			case <-storeTicker.C:
				r.runStoreMaintenance()
			case <-cleanupC:
				r.server.triggerCurrentUploadTempCleanup(uploadCleanupSourcePeriodic)
			}
		}
	}()
}

func (r *Runtime) runStartupMaintenance() {
	r.runStoreMaintenance()
}

func (r *Runtime) runStoreMaintenance() {
	if r.store == nil {
		return
	}
	operations := []func() error{
		func() error { return r.store.DeleteExpiredSessions(time.Now()) },
		func() error { return r.store.DeleteExpiredTokens(time.Now()) },
		func() error { return r.store.DeleteExpiredDownloadLeases(time.Now()) },
		func() error { return r.store.DeleteExpiredUploadLeases(time.Now()) },
		r.store.PruneAudit,
	}
	for _, operation := range operations {
		if err := r.ctx.Err(); err != nil {
			return
		}
		if err := operation(); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("[CRITICAL] event=maintenance_failed")
		}
	}
}

func (s *Server) healthLive(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"ok": true, "live": true})
}

func (s *Server) healthReady(c *fiber.Ctx) error {
	if s.runtime == nil || !s.runtime.IsReady() {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"ok": false, "ready": false})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.store.PingContext(ctx); err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"ok": false, "ready": false})
	}
	return c.JSON(fiber.Map{"ok": true, "ready": true})
}
