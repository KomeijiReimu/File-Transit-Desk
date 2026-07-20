package server

import (
	"net"
	"net/netip"
	"testing"

	"filetrans-backend/internal/config"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"
)

func TestProxyResolverIgnoresUntrustedForwardingHeaders(t *testing.T) {
	resolver := mustProxyResolver(t, "172.28.0.0/24")
	got := resolveIPRequest(t, resolver, "203.0.113.7:4321", "198.51.100.10", "198.51.100.11")
	if got != "203.0.113.7" {
		t.Fatalf("untrusted remote must ignore spoofed headers, got %q", got)
	}
}

func TestProxyResolverTrustedChainsFallbackAndAddressForms(t *testing.T) {
	resolver := mustProxyResolver(t, "172.28.0.0/24", "10.0.0.0/8", "2001:db8:ffff::/48")
	cases := []struct {
		name, remote, xff, realIP, want string
	}{
		{"single", "172.28.0.2:1234", "198.51.100.1", "", "198.51.100.1"},
		{"multi", "172.28.0.2:1234", "198.51.100.2, 10.1.2.3", "", "198.51.100.2"},
		{"invalid fallback", "172.28.0.2:1234", "198.51.100.2, bad", "203.0.113.8", "203.0.113.8"},
		{"invalid fallback ipv6 port", "172.28.0.2:1234", "bad", "[2001:db8::8]:443", "2001:db8::8"},
		{"invalid fallback remote", "172.28.0.2:1234", "bad", "also-bad", "172.28.0.2"},
		{"mapped remote", "[::ffff:172.28.0.2]:1234", "[2001:db8::5]:4321", "", "2001:db8::5"},
		{"ipv6 proxy", "[2001:db8:ffff::2]:1234", "2001:db8::6", "", "2001:db8::6"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveIPRequest(t, resolver, tc.remote, tc.xff, tc.realIP); got != tc.want {
				t.Fatalf("resolved IP=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestProxyResolverRejectsOversizedForwardedChain(t *testing.T) {
	resolver := mustProxyResolver(t, "172.28.0.0/24")
	xff := "198.51.100.1"
	for i := 1; i < 33; i++ {
		xff += ",198.51.100.1"
	}
	if got := resolveIPRequest(t, resolver, "172.28.0.2:80", xff, "203.0.113.9"); got != "203.0.113.9" {
		t.Fatalf("oversized chain must fall back to X-Real-IP, got %q", got)
	}
}

func TestComposeProxyPreservesDistinctClientIPs(t *testing.T) {
	resolver := mustProxyResolver(t, "172.28.0.0/24")
	first := resolveIPRequest(t, resolver, "172.28.0.3:80", "198.51.100.20", "198.51.100.20")
	second := resolveIPRequest(t, resolver, "172.28.0.3:80", "198.51.100.21", "198.51.100.21")
	if first == second || first != "198.51.100.20" || second != "198.51.100.21" {
		t.Fatalf("expected distinct proxy client IPs, first=%q second=%q", first, second)
	}
}

func TestRequestOriginOnlyUsesForwardedHeadersFromTrustedProxy(t *testing.T) {
	resolver := mustProxyResolver(t, "172.28.0.0/24")
	s := &Server{proxyResolver: resolver}
	cases := []struct {
		name, remote, host, proto, forwardedHost, want string
	}{
		{"untrusted spoof", "203.0.113.7:1", "direct.example:17878", "https", "public.example", "http://direct.example:17878"},
		{"trusted", "172.28.0.3:1", "backend:17878", "https", "public.example:8443", "https://public.example:8443"},
		{"trusted ipv6 host", "172.28.0.3:1", "backend:17878", "https", "[2001:db8::1]:8443", "https://[2001:db8::1]:8443"},
		{"invalid forwarded values", "172.28.0.3:1", "backend:17878", "ftp", "bad/host", "http://backend:17878"},
		{"invalid forwarded port", "172.28.0.3:1", "backend:17878", "https", "public.example:65536", "https://backend:17878"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			remote, err := parseProxyIP(tc.remote)
			if err != nil {
				t.Fatalf("parse remote: %v", err)
			}
			if got := s.resolveRequestOrigin(remote, "http", tc.host, tc.proto, tc.forwardedHost); got != tc.want {
				t.Fatalf("origin=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestProxyResolverConstructionRejectsTrustAllPrefixes(t *testing.T) {
	for _, cidr := range []string{"0.0.0.0/0", "::/0", "::ffff:0.0.0.0/96", "::fffe:0:0/95"} {
		t.Run(cidr, func(t *testing.T) {
			if _, err := newProxyResolver(config.ServerConfig{TrustProxyHeaders: true, TrustedProxyCIDRs: []string{cidr}}); err == nil {
				t.Fatalf("resolver accepted trust-all CIDR %q", cidr)
			}
		})
	}
}

func TestDevelopmentClientIPHeaderIsLoopbackDevOnlyAndStrict(t *testing.T) {
	resolver, err := newProxyResolver(config.ServerConfig{})
	if err != nil {
		t.Fatalf("new direct resolver: %v", err)
	}
	cases := []struct {
		name    string
		devMode bool
		remote  string
		headers []string
		want    string
	}{
		{name: "IPv4 loopback", devMode: true, remote: "127.9.8.7", headers: []string{"198.51.100.7"}, want: "198.51.100.7"},
		{name: "IPv6 loopback", devMode: true, remote: "::1", headers: []string{"2001:db8::7"}, want: "2001:db8::7"},
		{name: "mapped loopback", devMode: true, remote: "::ffff:127.0.0.8", headers: []string{"::ffff:192.0.2.8"}, want: "192.0.2.8"},
		{name: "zone removed", devMode: true, remote: "127.0.0.1", headers: []string{"fe80::1%eth0"}, want: "fe80::1"},
		{name: "production ignores", devMode: false, remote: "127.0.0.1", headers: []string{"198.51.100.8"}, want: "127.0.0.1"},
		{name: "non-loopback ignores", devMode: true, remote: "192.0.2.20", headers: []string{"198.51.100.9"}, want: "192.0.2.20"},
		{name: "invalid ignores", devMode: true, remote: "127.0.0.1", headers: []string{"not-an-ip"}, want: "127.0.0.1"},
		{name: "port ignores", devMode: true, remote: "127.0.0.1", headers: []string{"198.51.100.9:80"}, want: "127.0.0.1"},
		{name: "comma list ignores", devMode: true, remote: "127.0.0.1", headers: []string{"198.51.100.9, 203.0.113.9"}, want: "127.0.0.1"},
		{name: "duplicate ignores", devMode: true, remote: "127.0.0.1", headers: []string{"198.51.100.9", "203.0.113.9"}, want: "127.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, release := proxyTestContext(t, tc.remote)
			defer release()
			for _, value := range tc.headers {
				ctx.Context().Request.Header.Add(devClientIPHeader, value)
			}
			s := &Server{devMode: tc.devMode, proxyResolver: resolver}
			if got := s.clientIP(ctx); got != tc.want {
				t.Fatalf("client IP=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestDevelopmentClientIPHeaderFallsBackToExistingResolverAndDoesNotTrustOrigin(t *testing.T) {
	resolver := mustProxyResolver(t, "127.0.0.0/8")
	ctx, release := proxyTestContext(t, "127.0.0.1")
	defer release()
	ctx.Context().Request.Header.Set(devClientIPHeader, "invalid")
	ctx.Context().Request.Header.Set("X-Forwarded-For", "198.51.100.33")
	ctx.Context().Request.Header.Set("X-Forwarded-Proto", "https")
	ctx.Context().Request.Header.Set("X-Forwarded-Host", "forwarded.example")
	s := &Server{devMode: true, proxyResolver: resolver}
	if got := s.clientIP(ctx); got != "198.51.100.33" {
		t.Fatalf("invalid development header did not fall back to resolver: %q", got)
	}

	directResolver, err := newProxyResolver(config.ServerConfig{})
	if err != nil {
		t.Fatalf("new direct resolver: %v", err)
	}
	s.proxyResolver = directResolver
	ctx.Context().Request.Header.Set(devClientIPHeader, "203.0.113.44")
	if got := s.clientIP(ctx); got != "203.0.113.44" {
		t.Fatalf("valid development client IP=%q", got)
	}
	remote := netip.MustParseAddr("127.0.0.1")
	if origin := s.resolveRequestOrigin(remote, "http", "direct.example:17878", "https", "forwarded.example"); origin != "http://direct.example:17878" {
		t.Fatalf("development client header widened forwarded origin trust: %q", origin)
	}
}

func proxyTestContext(t *testing.T, remote string) (*fiber.Ctx, func()) {
	t.Helper()
	address := net.ParseIP(remote)
	if address == nil {
		t.Fatalf("parse test remote %q", remote)
	}
	app := fiber.New()
	requestContext := &fasthttp.RequestCtx{}
	requestContext.SetRemoteAddr(&net.TCPAddr{IP: address, Port: 12345})
	ctx := app.AcquireCtx(requestContext)
	return ctx, func() { app.ReleaseCtx(ctx) }
}

func mustProxyResolver(t *testing.T, cidrs ...string) *proxyResolver {
	t.Helper()
	resolver, err := newProxyResolver(config.ServerConfig{TrustProxyHeaders: true, TrustedProxyCIDRs: cidrs})
	if err != nil {
		t.Fatalf("new resolver: %v", err)
	}
	return resolver
}

func resolveIPRequest(t *testing.T, resolver *proxyResolver, remote, xff, realIP string) string {
	t.Helper()
	remoteAddress, err := parseProxyIP(remote)
	if err != nil {
		t.Fatalf("parse remote: %v", err)
	}
	return resolver.resolve(remoteAddress, xff, realIP)
}
