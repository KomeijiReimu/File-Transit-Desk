package server

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"filetrans-backend/internal/store"

	"github.com/gofiber/fiber/v2"
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
	probe, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback listener unavailable: %v", err)
	}
	_ = probe.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(t.TempDir())
	cfg.Server.Host = "::1"
	app, err := NewWithOptions(cfg, st, "", Options{DevFrontendPort: 5173})
	if err != nil {
		t.Fatalf("new IPv6 server: %v", err)
	}
	t.Cleanup(func() { _ = app.Shutdown() })

	started := make(chan fiber.ListenData, 1)
	app.Hooks().OnListen(func(data fiber.ListenData) error {
		started <- data
		return nil
	})
	listenErr := make(chan error, 1)
	go func() { listenErr <- app.Listen("[::1]:0") }()

	var data fiber.ListenData
	select {
	case data = <-started:
	case err := <-listenErr:
		t.Fatalf("IPv6 listen failed before startup: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatalf("IPv6 listener did not start")
	}
	connection, err := net.DialTimeout("tcp6", net.JoinHostPort("::1", data.Port), time.Second)
	if err != nil {
		t.Fatalf("dial real IPv6 listener: %v", err)
	}
	_ = connection.Close()
	if err := app.Shutdown(); err != nil {
		t.Fatalf("shutdown IPv6 listener: %v", err)
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
