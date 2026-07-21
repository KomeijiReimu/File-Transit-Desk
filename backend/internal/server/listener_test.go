package server

import (
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"filetrans-backend/internal/store"
)

func TestResolveListenEndpointNormalizesHostAndSelectsNetwork(t *testing.T) {
	for _, tc := range []struct {
		name, host, normalized, network, family, mode, address string
	}{
		{name: "IPv4 wildcard", host: "0.0.0.0", normalized: "0.0.0.0", network: "tcp4", family: "ipv4", mode: "wildcard", address: "0.0.0.0:17878"},
		{name: "IPv4 specific", host: "127.0.0.1", normalized: "127.0.0.1", network: "tcp4", family: "ipv4", mode: "specific", address: "127.0.0.1:17878"},
		{name: "IPv6 wildcard", host: "::", normalized: "::", network: "tcp6", family: "ipv6", mode: "wildcard", address: "[::]:17878"},
		{name: "bracketed IPv6 wildcard", host: "[::]", normalized: "::", network: "tcp6", family: "ipv6", mode: "wildcard", address: "[::]:17878"},
		{name: "IPv6 specific", host: "2001:db8::7", normalized: "2001:db8::7", network: "tcp6", family: "ipv6", mode: "specific", address: "[2001:db8::7]:17878"},
		{name: "bracketed IPv6 specific", host: "[2001:db8::7]", normalized: "2001:db8::7", network: "tcp6", family: "ipv6", mode: "specific", address: "[2001:db8::7]:17878"},
		{name: "hostname resolver policy", host: "LOCALHOST.", normalized: "localhost", network: "tcp", family: "unknown", mode: "hostname", address: "localhost:17878"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, err := ResolveListenEndpoint(tc.host, 17878)
			if err != nil {
				t.Fatalf("resolve endpoint: %v", err)
			}
			if endpoint.Host != tc.normalized || endpoint.Network != tc.network || endpoint.Family != tc.family || endpoint.Mode != tc.mode || endpoint.Address() != tc.address {
				t.Fatalf("endpoint=%+v address=%q", endpoint, endpoint.Address())
			}
		})
	}

	for _, tc := range []struct {
		host string
		port int
	}{
		{host: "", port: 17878},
		{host: "[::1", port: 17878},
		{host: "::1]", port: 17878},
		{host: "[127.0.0.1]", port: 17878},
		{host: "::ffff:127.0.0.1", port: 17878},
		{host: "example.test:1234", port: 17878},
		{host: "bad/host", port: 17878},
		{host: "127.0.0.1", port: 0},
		{host: "127.0.0.1", port: 65536},
	} {
		if endpoint, err := ResolveListenEndpoint(tc.host, tc.port); err == nil {
			t.Fatalf("invalid listen input host=%q port=%d produced %+v", tc.host, tc.port, endpoint)
		}
	}
}

func TestNewWithOptionsSetsFiberNetworkFromListenHost(t *testing.T) {
	for _, tc := range []struct {
		host, network string
	}{
		{host: "0.0.0.0", network: "tcp4"},
		{host: "192.0.2.9", network: "tcp4"},
		{host: "::", network: "tcp6"},
		{host: "2001:db8::9", network: "tcp6"},
		{host: "localhost", network: "tcp"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			defer st.DB.Close()
			cfg := testConfig(t.TempDir())
			cfg.Server.Host = tc.host
			app, err := NewWithOptions(cfg, st, "", Options{DevFrontendPort: 5173})
			if err != nil {
				t.Fatalf("new server: %v", err)
			}
			if got := app.Config().Network; got != tc.network {
				t.Fatalf("Fiber network=%q want=%q", got, tc.network)
			}
			if err := app.Shutdown(); err != nil {
				t.Fatalf("shutdown app: %v", err)
			}
		})
	}
}

func TestFiberCanStartRealIPv6ListenerWhenAvailable(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback listener unavailable: %v", err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		_ = listener.Close()
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.DB.Close() })
	cfg := testConfig(t.TempDir())
	cfg.Server.Host = "::1"
	app, err := NewWithOptions(cfg, st, "", Options{DevFrontendPort: 5173})
	if err != nil {
		_ = listener.Close()
		t.Fatalf("new IPv6 server: %v", err)
	}
	t.Cleanup(func() {
		_ = app.Shutdown()
		_ = listener.Close()
	})

	listenErr := make(chan error, 1)
	go func() { listenErr <- app.Listener(listener) }()

	client := &http.Client{
		Transport: &http.Transport{Proxy: nil},
		Timeout:   3 * time.Second,
	}
	response, err := client.Get("http://" + listener.Addr().String() + "/api/health/live")
	if err != nil {
		t.Fatalf("request real IPv6 listener: %v", err)
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		t.Fatalf("read IPv6 health response: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close IPv6 health response: %v", closeErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("IPv6 health status=%d want=%d", response.StatusCode, http.StatusOK)
	}

	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- app.Shutdown() }()
	select {
	case err := <-shutdownErr:
		if err != nil {
			t.Fatalf("shutdown IPv6 listener: %v", err)
		}
	case <-time.After(3 * time.Second):
		_ = listener.Close()
		t.Fatalf("shutdown IPv6 listener timed out")
	}

	select {
	case err := <-listenErr:
		if err != nil {
			t.Fatalf("IPv6 listener returned error after shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("IPv6 listener did not stop")
	}
}
