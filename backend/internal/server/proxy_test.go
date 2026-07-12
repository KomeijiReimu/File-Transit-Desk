package server

import (
	"testing"

	"filetrans-backend/internal/config"
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
