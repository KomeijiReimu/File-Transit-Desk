package server

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filetrans-backend/internal/store"
)

func TestNormalizeShareOriginStrictlyAcceptsOnlyOrigins(t *testing.T) {
	valid := map[string]string{
		"http://Example.COM/":               "http://example.com",
		"https://example.com:443":           "https://example.com",
		"HTTP://127.0.0.1:080/":             "http://127.0.0.1",
		"https://[2001:db8::1]:8443/":       "https://[2001:db8::1]:8443",
		"  https://share.example.test:9443": "https://share.example.test:9443",
	}
	for input, want := range valid {
		got, ok := normalizeShareOrigin(input)
		if !ok || got.Origin != want {
			t.Fatalf("normalizeShareOrigin(%q)=(%+v,%v), want origin %q", input, got, ok, want)
		}
	}

	invalid := []string{
		"",
		"//example.com",
		"ftp://example.com",
		"javascript:alert(1)",
		"http://user@example.com",
		"http://user:password@example.com",
		"http://example.com/path",
		"http://example.com/%2F",
		"http://example.com?query=1",
		"http://example.com?",
		"http://example.com#fragment",
		"http://example.com#",
		"http://example.com:",
		"http://example.com:0",
		"http://example.com:65536",
		"http://[2001:db8::1",
		"http://[fe80::1%25eth0]",
		"http://example.com\\@attacker.example",
	}
	for _, input := range invalid {
		if got, ok := normalizeShareOrigin(input); ok {
			t.Fatalf("normalizeShareOrigin(%q) unexpectedly accepted %+v", input, got)
		}
	}
}

func TestShareOriginCandidatesRespectListenBindingAndAddressFamily(t *testing.T) {
	addresses := []shareInterfaceAddress{
		{Interface: "lo", Address: netip.MustParseAddr("127.0.0.1")},
		{Interface: "eth0", Address: netip.MustParseAddr("192.168.20.8")},
		{Interface: "eth1", Address: netip.MustParseAddr("192.168.20.8")},
		{Interface: "wlan0", Address: netip.MustParseAddr("10.0.0.9")},
		{Interface: "lo", Address: netip.MustParseAddr("::1")},
		{Interface: "eth0", Address: netip.MustParseAddr("2001:db8::8")},
	}

	t.Run("loopback listener stays diagnostic under HTTPS frontend", func(t *testing.T) {
		current := mustNormalizeShareOrigin(t, "https://admin.example")
		endpoint := mustResolveListenEndpoint(t, "127.0.0.1", 17878)
		items := buildShareOriginCandidates(endpoint, current, addresses)
		if len(items) != 1 || items[0].Origin != "https://admin.example" || items[0].Source != "current" {
			t.Fatalf("loopback listener polluted access origins: %+v", items)
		}
		assertCandidateOriginsExclude(t, items, "127.0.0.1", "192.168.20.8", "10.0.0.9", "2001:db8::8")
		if items[0].ListenMatchStatus != shareListenMatchUnknown || items[0].ListenMatch != nil {
			t.Fatalf("DNS current origin must have unknown listen match: %+v", items[0])
		}
		assertListenDiagnostic(t, items[0], "tcp4", "ipv4", "specific", "127.0.0.1:17878")
	})

	t.Run("specific IP only lists its matching interface", func(t *testing.T) {
		current := mustNormalizeShareOrigin(t, "https://admin.example:8443")
		endpoint := mustResolveListenEndpoint(t, "192.168.20.8", 17878)
		items := buildShareOriginCandidates(endpoint, current, addresses)
		if len(items) != 2 {
			t.Fatalf("specific binding produced unrelated candidates: %+v", items)
		}
		candidate := requireShareOriginCandidate(t, items, "https://192.168.20.8:8443")
		if candidate.Source != "interface" || candidate.Interface != "eth0" || candidate.Reachable != "unknown" || candidate.ListenMatch != nil || candidate.ListenMatchStatus != shareListenMatchUnknown {
			t.Fatalf("unexpected specific-IP candidate metadata: %+v", candidate)
		}
		if containsShareString(candidate.Sources, "configured") {
			t.Fatalf("listener reused configured public-origin semantics: %+v", candidate)
		}
		if len(candidate.Interfaces) != 2 || !containsShareString(candidate.Interfaces, "eth0") || !containsShareString(candidate.Interfaces, "eth1") {
			t.Fatalf("duplicate address interfaces were not merged: %+v", candidate)
		}
		assertListenDiagnostic(t, candidate, "tcp4", "ipv4", "specific", "192.168.20.8:17878")
	})

	t.Run("specific IPv6 only lists its matching interface", func(t *testing.T) {
		current := mustNormalizeShareOrigin(t, "https://admin.example:8443")
		endpoint := mustResolveListenEndpoint(t, "2001:db8::8", 17878)
		items := buildShareOriginCandidates(endpoint, current, addresses)
		candidate := requireShareOriginCandidate(t, items, "https://[2001:db8::8]:8443")
		if candidate.Source != "interface" || candidate.ListenMatch != nil || candidate.ListenMatchStatus != shareListenMatchUnknown {
			t.Fatalf("unexpected specific IPv6 candidate: %+v", candidate)
		}
		assertCandidateOriginsExclude(t, items, "192.168.20.8", "10.0.0.9", "127.0.0.1")
		assertListenDiagnostic(t, candidate, "tcp6", "ipv6", "specific", "[2001:db8::8]:17878")
	})

	t.Run("IPv4 wildcard enumerates only IPv4", func(t *testing.T) {
		current := mustNormalizeShareOrigin(t, "http://192.168.20.8:17878")
		endpoint := mustResolveListenEndpoint(t, "0.0.0.0", 17878)
		items := buildShareOriginCandidates(endpoint, current, addresses)
		for _, item := range items {
			if strings.Contains(item.Origin, "[") {
				t.Fatalf("IPv4 wildcard emitted IPv6 candidate: %+v", item)
			}
		}
		for _, origin := range []string{"http://10.0.0.9:17878", "http://192.168.20.8:17878"} {
			requireShareOriginCandidate(t, items, origin)
		}
		assertCandidateOriginsExclude(t, items, "127.0.0.1")
		if countShareOrigin(items, "http://192.168.20.8:17878") != 1 {
			t.Fatalf("duplicate IPv4 origin was not removed: %+v", items)
		}
		matched := requireShareOriginCandidate(t, items, "http://192.168.20.8:17878")
		if matched.ListenMatch == nil || !*matched.ListenMatch || !containsShareString(matched.Sources, "current") || !containsShareString(matched.Sources, "interface") {
			t.Fatalf("current/interface duplicate did not merge as a match: %+v", matched)
		}
		assertListenDiagnostic(t, matched, "tcp4", "ipv4", "wildcard", "0.0.0.0:17878")
	})

	t.Run("IPv6 wildcard enumerates and brackets only IPv6", func(t *testing.T) {
		current := mustNormalizeShareOrigin(t, "https://[2001:db8::8]:8443")
		endpoint := mustResolveListenEndpoint(t, "::", 17878)
		items := buildShareOriginCandidates(endpoint, current, addresses)
		for _, origin := range []string{"https://[2001:db8::8]:8443"} {
			requireShareOriginCandidate(t, items, origin)
		}
		assertCandidateOriginsExclude(t, items, "[::1]")
		for _, item := range items {
			if strings.Contains(item.Origin, "192.168.") || strings.Contains(item.Origin, "127.0.0.1") || strings.Contains(item.Origin, "10.0.0.9") {
				t.Fatalf("IPv6 wildcard emitted IPv4 candidate: %+v", item)
			}
		}
		assertListenDiagnostic(t, items[0], "tcp6", "ipv6", "wildcard", "[::]:17878")
	})

	t.Run("wildcard with DNS current origin is unknown", func(t *testing.T) {
		current := mustNormalizeShareOrigin(t, "https://files.example")
		endpoint := mustResolveListenEndpoint(t, "0.0.0.0", 17878)
		item := requireShareOriginCandidate(t, buildShareOriginCandidates(endpoint, current, nil), "https://files.example")
		if item.ListenMatchStatus != shareListenMatchUnknown || item.ListenMatch != nil {
			t.Fatalf("wildcard DNS match was asserted instead of unknown: %+v", item)
		}
	})
}

func TestShareListenMatchRequiresSameEffectivePort(t *testing.T) {
	addresses := []shareInterfaceAddress{
		{Interface: "eth0", Address: netip.MustParseAddr("192.168.20.8")},
		{Interface: "eth0", Address: netip.MustParseAddr("2001:db8::8")},
	}
	tests := []struct {
		name       string
		origin     string
		listenHost string
		listenPort int
		want       string
	}{
		{name: "same IP and explicit port", origin: "http://192.168.20.8:17878", listenHost: "192.168.20.8", listenPort: 17878, want: shareListenMatch},
		{name: "same IP but Vite port mapping", origin: "http://192.168.20.8:5173", listenHost: "192.168.20.8", listenPort: 17878, want: shareListenMatchUnknown},
		{name: "default HTTP port", origin: "http://192.168.20.8", listenHost: "192.168.20.8", listenPort: 80, want: shareListenMatch},
		{name: "default HTTPS port", origin: "https://[2001:db8::8]", listenHost: "2001:db8::8", listenPort: 443, want: shareListenMatch},
		{name: "specific host mismatch on same port", origin: "http://192.168.20.9:17878", listenHost: "192.168.20.8", listenPort: 17878, want: shareListenMismatch},
		{name: "specific host and port both differ", origin: "http://192.168.20.9:5173", listenHost: "192.168.20.8", listenPort: 17878, want: shareListenMatchUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			current := mustNormalizeShareOrigin(t, test.origin)
			binding := shareListenBindingFromEndpoint(mustResolveListenEndpoint(t, test.listenHost, test.listenPort))
			if got := binding.matchesCurrent(current, addresses); got != test.want {
				t.Fatalf("matchesCurrent(%q, %s:%d)=%q want=%q", test.origin, test.listenHost, test.listenPort, got, test.want)
			}
		})
	}
}

func TestShareAddressScopeUsesNeutralReachabilityNames(t *testing.T) {
	for _, tc := range []struct {
		address string
		want    string
	}{
		{address: "127.0.0.1", want: "loopback"},
		{address: "192.168.1.2", want: "private"},
		{address: "100.64.10.2", want: "carrier-grade-nat"},
		{address: "2001:db8::1", want: "global-unicast"},
	} {
		if got := shareAddressScope(netip.MustParseAddr(tc.address)); got != tc.want {
			t.Fatalf("scope(%s)=%q want=%q", tc.address, got, tc.want)
		}
	}
}

func TestShareOriginsEndpointIsAdminOnlyCompatibleAndIgnoresSpoofedOrigin(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(t.TempDir())
	cfg.Server.Host = "127.0.0.1"
	app := New(cfg, st)
	for _, session := range []struct {
		id, role string
	}{
		{id: "share-user", role: "user"},
		{id: "share-admin", role: "admin"},
	} {
		if err := st.CreateSessionWithIdle(session.id, time.Now().Add(time.Hour), time.Now().Add(time.Hour), session.role, session.role); err != nil {
			t.Fatalf("create %s session: %v", session.role, err)
		}
	}

	userReq := httptest.NewRequest(http.MethodGet, "/api/share-origins", nil)
	userReq.AddCookie(&http.Cookie{Name: "sid", Value: "share-user"})
	userResp, err := app.Test(userReq)
	if err != nil {
		t.Fatalf("non-admin share origins request: %v", err)
	}
	userResp.Body.Close()
	if userResp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin share origins status=%d, want 403", userResp.StatusCode)
	}

	adminReq := httptest.NewRequest(http.MethodGet, "/api/share-origins?currentOrigin="+url.QueryEscape("https://Share.Example/"), nil)
	adminReq.AddCookie(&http.Cookie{Name: "sid", Value: "share-admin"})
	adminResp, err := app.Test(adminReq)
	if err != nil {
		t.Fatalf("admin share origins request: %v", err)
	}
	var compatible []map[string]any
	decodeJSON(t, adminResp, &compatible)
	if adminResp.StatusCode != http.StatusOK || len(compatible) == 0 {
		t.Fatalf("admin share origins status=%d payload=%v", adminResp.StatusCode, compatible)
	}
	foundCurrent := false
	for _, item := range compatible {
		for _, field := range []string{"origin", "label", "source"} {
			if value, ok := item[field].(string); !ok || value == "" {
				t.Fatalf("legacy candidate field %q missing from %v", field, item)
			}
		}
		if item["reachable"] != "unknown" {
			t.Fatalf("candidate claimed reachability: %v", item)
		}
		if item["source"] == "listen" || item["source"] == "configured" {
			t.Fatalf("listener diagnostic polluted legacy origin array: %v", item)
		}
		if item["origin"] == "https://share.example" && item["source"] == "current" {
			foundCurrent = true
			if item["listenMatchStatus"] != "unknown" {
				t.Fatalf("DNS current origin did not expose unknown match status: %v", item)
			}
			if _, exists := item["listenMatch"]; exists {
				t.Fatalf("unknown match must omit legacy optional boolean: %v", item)
			}
			listen, ok := item["listen"].(map[string]any)
			if !ok || listen["source"] != "listen" || listen["address"] != "127.0.0.1:17878" || listen["port"] != float64(17878) || listen["network"] != "tcp4" {
				t.Fatalf("current origin missing actual listener diagnostics: %v", item)
			}
		}
	}
	if !foundCurrent {
		t.Fatalf("normalized current origin missing from compatible array: %v", compatible)
	}

	spoofReq := httptest.NewRequest(http.MethodGet, "http://direct.example:17878/api/share-origins?currentOrigin="+url.QueryEscape("https://attacker.example/path"), nil)
	spoofReq.RemoteAddr = "203.0.113.9:4321"
	spoofReq.Header.Set("Forwarded", "for=198.51.100.2;proto=https;host=forwarded.example")
	spoofReq.Header.Set("X-Forwarded-Proto", "https")
	spoofReq.Header.Set("X-Forwarded-Host", "forwarded.example")
	spoofReq.AddCookie(&http.Cookie{Name: "sid", Value: "share-admin"})
	spoofResp, err := app.Test(spoofReq)
	if err != nil {
		t.Fatalf("spoofed share origins request: %v", err)
	}
	var spoofed []shareOriginDTO
	decodeJSON(t, spoofResp, &spoofed)
	if spoofResp.StatusCode != http.StatusOK {
		t.Fatalf("spoofed request status=%d payload=%+v", spoofResp.StatusCode, spoofed)
	}
	requireShareOriginCandidate(t, spoofed, "http://direct.example:17878")
	for _, item := range spoofed {
		if strings.Contains(item.Origin, "attacker.example") || strings.Contains(item.Origin, "forwarded.example") {
			t.Fatalf("invalid current origin or untrusted proxy header affected candidates: %+v", item)
		}
	}
}

func TestShareOriginsKeepsTrustedHTTPSOriginSeparateFromLoopbackListener(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	cfg := testConfig(t.TempDir())
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = 17878
	cfg.Server.TrustProxyHeaders = true
	cfg.Server.TrustedProxyCIDRs = []string{"0.0.0.0/32"}
	app := New(cfg, st)
	if err := st.CreateSessionWithIdle("proxy-admin", time.Now().Add(time.Hour), time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatalf("create admin session: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://backend.internal:17878/api/share-origins", nil)
	req.RemoteAddr = "203.0.113.9:4321"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "files.example")
	req.AddCookie(&http.Cookie{Name: "sid", Value: "proxy-admin"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("trusted proxy share origins request: %v", err)
	}
	var items []shareOriginDTO
	decodeJSON(t, resp, &items)
	if resp.StatusCode != http.StatusOK || len(items) != 1 {
		t.Fatalf("unexpected trusted proxy response: status=%d items=%+v", resp.StatusCode, items)
	}
	item := items[0]
	if item.Origin != "https://files.example" || item.Source != "current" || item.ListenMatchStatus != shareListenMatchUnknown || item.ListenMatch != nil {
		t.Fatalf("trusted frontend origin was conflated with listener: %+v", item)
	}
	assertCandidateOriginsExclude(t, items, "https://127.0.0.1")
	assertListenDiagnostic(t, item, "tcp4", "ipv4", "specific", "127.0.0.1:17878")
}

func mustResolveListenEndpoint(t *testing.T, host string, port int) ListenEndpoint {
	t.Helper()
	endpoint, err := ResolveListenEndpoint(host, port)
	if err != nil {
		t.Fatalf("resolve listen endpoint %q: %v", host, err)
	}
	return endpoint
}

func assertListenDiagnostic(t *testing.T, item shareOriginDTO, network, family, mode, address string) {
	t.Helper()
	if item.Listen == nil || item.Listen.Source != "listen" || item.Listen.Network != network || item.Listen.Family != family || item.Listen.Mode != mode || item.Listen.Address != address || item.Listen.Reachable != "unknown" {
		t.Fatalf("unexpected listen diagnostic: %+v", item)
	}
}

func mustNormalizeShareOrigin(t *testing.T, value string) normalizedShareOrigin {
	t.Helper()
	origin, ok := normalizeShareOrigin(value)
	if !ok {
		t.Fatalf("test origin %q is invalid", value)
	}
	return origin
}

func requireShareOriginCandidate(t *testing.T, items []shareOriginDTO, origin string) shareOriginDTO {
	t.Helper()
	for _, item := range items {
		if item.Origin == origin {
			return item
		}
	}
	t.Fatalf("missing share origin %q in %+v", origin, items)
	return shareOriginDTO{}
}

func assertCandidateOriginsExclude(t *testing.T, items []shareOriginDTO, fragments ...string) {
	t.Helper()
	for _, item := range items {
		for _, fragment := range fragments {
			if strings.Contains(item.Origin, fragment) {
				t.Fatalf("candidate %q unexpectedly contains %q: %+v", item.Origin, fragment, items)
			}
		}
	}
}

func countShareOrigin(items []shareOriginDTO, origin string) int {
	count := 0
	for _, item := range items {
		if item.Origin == origin {
			count++
		}
	}
	return count
}

func containsShareString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
