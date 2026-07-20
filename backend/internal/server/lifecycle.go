package server

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"filetrans-backend/internal/config"
	"filetrans-backend/internal/store"

	"github.com/gofiber/fiber/v2"
)

const (
	defaultStoreMaintenanceInterval = 5 * time.Minute
	defaultChatCleanupBudget        = 250 * time.Millisecond
	defaultChatCleanupRetryDelay    = time.Second
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

	storeMaintenanceInterval time.Duration
	chatCleanupBudget        time.Duration
	chatCleanupRetryDelay    time.Duration
}

func newRuntime(st *store.Store) *Runtime {
	ctx, cancel := context.WithCancel(context.Background())
	runtime := &Runtime{
		store:                    st,
		ctx:                      ctx,
		cancel:                   cancel,
		storeMaintenanceInterval: defaultStoreMaintenanceInterval,
		chatCleanupBudget:        defaultChatCleanupBudget,
		chatCleanupRetryDelay:    defaultChatCleanupRetryDelay,
	}
	runtime.activeCond = sync.NewCond(&runtime.activeMu)
	return runtime
}

func (r *Runtime) BeginDrain() {
	if r != nil {
		r.draining.Store(true)
		// Maintenance loops only use this context and check it between bounded
		// transactions, so drain cancellation stops catch-up promptly without
		// interrupting admitted request handlers.
		r.cancel()
	}
}

func (r *Runtime) IsReady() bool {
	return r != nil && r.initialized.Load() && !r.draining.Load() && r.ctx.Err() == nil
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
		r.server.triggerCurrentUploadTempCleanup(uploadCleanupSourceStartup)
		storeInterval := r.storeMaintenanceInterval
		if storeInterval <= 0 {
			storeInterval = defaultStoreMaintenanceInterval
		}
		storeTicker := time.NewTicker(storeInterval)
		defer storeTicker.Stop()
		interval := time.Duration(r.server.cfg().Storage.UploadTempCleanupIntervalSeconds) * time.Second
		var cleanupTicker *time.Ticker
		var cleanupC <-chan time.Time
		if interval > 0 {
			cleanupTicker = time.NewTicker(interval)
			cleanupC = cleanupTicker.C
			defer cleanupTicker.Stop()
		}
		var retryTimer *time.Timer
		var retryC <-chan time.Time
		stopRetry := func() {
			if retryTimer != nil && !retryTimer.Stop() {
				select {
				case <-retryTimer.C:
				default:
				}
			}
			retryC = nil
		}
		defer stopRetry()
		scheduleRetry := func() {
			delay := r.chatCleanupRetryDelay
			if delay <= 0 || delay > time.Second {
				delay = defaultChatCleanupRetryDelay
			}
			if retryTimer == nil {
				retryTimer = time.NewTimer(delay)
			} else {
				stopRetry()
				retryTimer.Reset(delay)
			}
			retryC = retryTimer.C
		}
		if r.runPeriodicChatMaintenance() {
			scheduleRetry()
		}
		for {
			select {
			case <-r.ctx.Done():
				return
			case <-storeTicker.C:
				if r.runStoreMaintenance() {
					scheduleRetry()
				} else {
					stopRetry()
				}
			case <-retryC:
				retryC = nil
				if r.runPeriodicChatMaintenance() {
					scheduleRetry()
				}
			case <-cleanupC:
				r.server.triggerCurrentUploadTempCleanup(uploadCleanupSourcePeriodic)
			}
		}
	}()
}

func (r *Runtime) runStartupMaintenance() error {
	if r.store == nil {
		return nil
	}
	now := r.maintenanceTime()
	r.runNonChatStoreMaintenance(now)
	if r.server == nil {
		return nil
	}
	chatCfg := r.server.cfg().Chat
	_, err := r.cleanupChatBatches(r.ctx, now, chatCfg, time.Time{})
	return err
}

func (r *Runtime) runStoreMaintenance() bool {
	if r.store == nil {
		return false
	}
	now := r.maintenanceTime()
	r.runNonChatStoreMaintenance(now)
	return r.runPeriodicChatMaintenanceAt(now)
}

func (r *Runtime) runNonChatStoreMaintenance(now time.Time) {
	idleGrace := time.Duration(0)
	if r.server != nil {
		idleGrace = time.Duration(r.server.cfg().Auth.IdleGraceSeconds) * time.Second
	}
	operations := []func() error{
		func() error { return r.store.DeleteExpiredSessionsWithIdleGrace(now, idleGrace) },
		func() error { return r.store.DeleteExpiredTokens(now) },
		func() error { return r.store.DeleteExpiredDownloadLeases(now) },
		func() error { return r.store.DeleteExpiredUploadLeases(now) },
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

func (r *Runtime) runPeriodicChatMaintenance() bool {
	return r.runPeriodicChatMaintenanceAt(r.maintenanceTime())
}

func (r *Runtime) runPeriodicChatMaintenanceAt(now time.Time) bool {
	if r.store == nil || r.server == nil || r.ctx.Err() != nil {
		return false
	}
	// Copy the current policy once. Every short transaction in this round uses
	// the same snapshot; a hot config replacement takes effect next round.
	chatCfg := r.server.cfg().Chat
	budget := r.chatCleanupBudget
	if budget <= 0 {
		budget = defaultChatCleanupBudget
	}
	deadline := time.Now().Add(budget)
	caughtUp, err := r.cleanupChatBatches(r.ctx, now, chatCfg, deadline)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			log.Printf("[CRITICAL] event=chat_retention_cleanup_failed")
			return true
		}
		return false
	}
	return !caughtUp
}

func (r *Runtime) cleanupChatBatches(ctx context.Context, now time.Time, chatCfg config.ChatConfig, deadline time.Time) (bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		removed, err := r.store.CleanupChat(now, chatCfg.RetentionDays, chatCfg.MaxMessages, chatCfg.CleanupBatch)
		if err != nil {
			return false, err
		}
		if removed < chatCfg.CleanupBatch {
			return true, nil
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return false, nil
		}
	}
}

func (r *Runtime) maintenanceTime() time.Time {
	if r.server != nil && r.server.maintenanceNow != nil {
		return r.server.maintenanceNow()
	}
	return time.Now()
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
