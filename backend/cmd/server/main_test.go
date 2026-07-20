package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"filetrans-backend/internal/config"
	serverpkg "filetrans-backend/internal/server"
	"filetrans-backend/internal/store"

	"github.com/gofiber/fiber/v2"
)

func TestListenerAddressUsesNormalizedJoinHostPort(t *testing.T) {
	for _, tc := range []struct {
		host string
		want string
	}{
		{host: "0.0.0.0", want: "0.0.0.0:17878"},
		{host: "127.0.0.1", want: "127.0.0.1:17878"},
		{host: "::", want: "[::]:17878"},
		{host: "[::]", want: "[::]:17878"},
		{host: "2001:db8::1", want: "[2001:db8::1]:17878"},
		{host: "[2001:db8::1]", want: "[2001:db8::1]:17878"},
	} {
		endpoint, err := serverpkg.ResolveListenEndpoint(tc.host, 17878)
		if err != nil {
			t.Fatalf("resolve %q: %v", tc.host, err)
		}
		if got := listenerAddress(endpoint); got != tc.want {
			t.Fatalf("listenerAddress(%q)=%q want=%q", tc.host, got, tc.want)
		}
		if got := listenerAddress(endpoint); got == ":::17878" {
			t.Fatalf("listenerAddress generated malformed IPv6 address")
		}
	}
}

func TestRunServerListenerErrorDrainsActiveHandlerBeforeNonZeroExit(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg := config.Default()
	runtime, err := serverpkg.NewRuntimeWithOptions(cfg, st, "", serverpkg.Options{DevFrontendPort: 5173})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	runtime.App.Get("/test/listener-error-block", func(c *fiber.Ctx) error {
		close(started)
		<-release
		return c.SendStatus(http.StatusNoContent)
	})
	requestDone := make(chan error, 1)
	go func() {
		resp, err := runtime.App.Test(httptest.NewRequest(http.MethodGet, "/test/listener-error-block", nil), -1)
		if resp != nil {
			resp.Body.Close()
		}
		requestDone <- err
	}()
	<-started
	signals := make(chan os.Signal, 1)
	exitCode := make(chan int, 1)
	go func() { exitCode <- runServer(runtime, func() error { return errors.New("listener failed") }, signals) }()
	select {
	case code := <-exitCode:
		t.Fatalf("run loop exited before active handler: %d", code)
	case <-time.After(100 * time.Millisecond):
	}
	if err := st.PingContext(context.Background()); err != nil {
		t.Fatalf("store closed before handler completion: %v", err)
	}
	close(release)
	if err := <-requestDone; err != nil {
		t.Fatalf("active request: %v", err)
	}
	if code := <-exitCode; code == 0 {
		t.Fatalf("listener failure returned success")
	}
	select {
	case <-runtime.Done():
	default:
		t.Fatalf("maintenance context was not stopped")
	}
	if err := st.PingContext(context.Background()); err == nil {
		t.Fatalf("store remained open after listener-error shutdown")
	}
}

type blockingRuntime struct {
	beginOnce sync.Once
	begin     chan struct{}
	shutdown  chan struct{}
}

func newBlockingRuntime() *blockingRuntime {
	return &blockingRuntime{begin: make(chan struct{}), shutdown: make(chan struct{})}
}

func (r *blockingRuntime) BeginDrain() { r.beginOnce.Do(func() { close(r.begin) }) }
func (r *blockingRuntime) Shutdown() error {
	r.BeginDrain()
	<-r.shutdown
	return nil
}

func TestRunServerForceSignalThreshold(t *testing.T) {
	t.Run("listener clean exit is still unexpected", func(t *testing.T) {
		runtime := newBlockingRuntime()
		close(runtime.shutdown)
		if code := runServer(runtime, func() error { return nil }, make(chan os.Signal)); code == 0 {
			t.Fatalf("unexpected listener exit returned success")
		}
		select {
		case <-runtime.begin:
		default:
			t.Fatalf("unexpected listener exit did not begin drain")
		}
	})
	t.Run("listener error uses first signal", func(t *testing.T) {
		runtime := newBlockingRuntime()
		signals := make(chan os.Signal, 1)
		done := make(chan int, 1)
		go func() { done <- runServer(runtime, func() error { return errors.New("failed") }, signals) }()
		<-runtime.begin
		signals <- os.Interrupt
		if code := <-done; code != 2 {
			t.Fatalf("expected force exit code 2, got %d", code)
		}
		close(runtime.shutdown)
	})
	t.Run("signal drain uses second signal", func(t *testing.T) {
		runtime := newBlockingRuntime()
		signals := make(chan os.Signal, 2)
		listenRelease := make(chan struct{})
		done := make(chan int, 1)
		go func() {
			done <- runServer(runtime, func() error {
				<-listenRelease
				return nil
			}, signals)
		}()
		signals <- os.Interrupt
		<-runtime.begin
		select {
		case code := <-done:
			t.Fatalf("first signal forced exit: %d", code)
		case <-time.After(50 * time.Millisecond):
		}
		signals <- os.Interrupt
		if code := <-done; code != 2 {
			t.Fatalf("expected second signal force code 2, got %d", code)
		}
		close(runtime.shutdown)
		close(listenRelease)
	})
}
