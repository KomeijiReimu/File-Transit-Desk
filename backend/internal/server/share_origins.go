package server

import (
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
)

const (
	shareOriginReachabilityUnknown = "unknown"
	shareListenMatch               = "match"
	shareListenMismatch            = "mismatch"
	shareListenMatchUnknown        = "unknown"
)

var carrierGradeNATPrefix = netip.MustParsePrefix("100.64.0.0/10")

type normalizedShareOrigin struct {
	Origin string
	Scheme string
	Host   string
	Port   string
}

type shareInterfaceAddress struct {
	Interface string
	Address   netip.Addr
}

// shareListenDTO is diagnostic socket metadata nested inside ordinary origin
// candidates. It is deliberately not another array item: legacy consumers may
// safely copy every top-level origin without mistaking the backend socket for
// a browser-accessible public URL.
type shareListenDTO struct {
	Source    string `json:"source"`
	Network   string `json:"network"`
	Family    string `json:"family"`
	Mode      string `json:"mode"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Address   string `json:"address"`
	Reachable string `json:"reachable"`
}

type shareListenKind uint8

const (
	shareListenInvalid shareListenKind = iota
	shareListenWildcard
	shareListenAddress
	shareListenHostname
)

type shareListenBinding struct {
	kind     shareListenKind
	host     string
	address  netip.Addr
	ipv4Only bool
	port     int
}

// normalizeShareOrigin accepts an origin, not a general URL. In particular, an
// encoded path, credentials, query, or fragment must not be allowed to become
// part of a share-link candidate.
func normalizeShareOrigin(value string) (normalizedShareOrigin, bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "?#") {
		return normalizedShareOrigin{}, false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Opaque != "" || parsed.User != nil || parsed.Host == "" {
		return normalizedShareOrigin{}, false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return normalizedShareOrigin{}, false
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return normalizedShareOrigin{}, false
	}
	if path := parsed.EscapedPath(); path != "" && path != "/" {
		return normalizedShareOrigin{}, false
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return normalizedShareOrigin{}, false
	}
	// url.Parse accepts an empty explicit port ("host:"); an origin must not.
	if strings.HasSuffix(parsed.Host, ":") {
		return normalizedShareOrigin{}, false
	}
	host, ok := normalizeShareHost(parsed.Hostname())
	if !ok {
		return normalizedShareOrigin{}, false
	}
	port := parsed.Port()
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return normalizedShareOrigin{}, false
		}
		port = strconv.Itoa(number)
		if scheme == "http" && port == "80" || scheme == "https" && port == "443" {
			port = ""
		}
	}
	origin := scheme + "://" + formatShareAuthority(host, port)
	return normalizedShareOrigin{Origin: origin, Scheme: scheme, Host: host, Port: port}, true
}

func normalizeShareHost(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	if address, err := netip.ParseAddr(value); err == nil {
		if address.Zone() != "" {
			return "", false
		}
		return address.Unmap().String(), true
	}
	normalized, ok := normalizeOriginHost(value)
	if !ok || strings.Contains(normalized, ":") {
		return "", false
	}
	return normalized, true
}

func formatShareAuthority(host, port string) string {
	if port != "" {
		return net.JoinHostPort(host, port)
	}
	if address, err := netip.ParseAddr(host); err == nil && address.Is6() {
		return "[" + address.String() + "]"
	}
	return host
}

func defaultShareOriginContext(port int) normalizedShareOrigin {
	value := ""
	if port > 0 && port != 80 {
		value = strconv.Itoa(port)
	}
	return normalizedShareOrigin{Scheme: "http", Port: value}
}

func shareListenBindingFromEndpoint(endpoint ListenEndpoint) shareListenBinding {
	switch endpoint.Mode {
	case ListenModeWildcard:
		address, err := netip.ParseAddr(endpoint.Host)
		if err != nil {
			return shareListenBinding{}
		}
		return shareListenBinding{kind: shareListenWildcard, host: endpoint.Host, address: address.Unmap(), ipv4Only: endpoint.Network == ListenNetworkTCP4, port: endpoint.Port}
	case ListenModeSpecific:
		address, err := netip.ParseAddr(endpoint.Host)
		if err != nil {
			return shareListenBinding{}
		}
		return shareListenBinding{kind: shareListenAddress, host: endpoint.Host, address: address.Unmap(), ipv4Only: endpoint.Network == ListenNetworkTCP4, port: endpoint.Port}
	case ListenModeHostname:
		return shareListenBinding{kind: shareListenHostname, host: endpoint.Host, port: endpoint.Port}
	default:
		return shareListenBinding{}
	}
}

func (binding shareListenBinding) matchesAddress(address netip.Addr) bool {
	if !address.IsValid() {
		return false
	}
	address = address.Unmap()
	switch binding.kind {
	case shareListenWildcard:
		if binding.ipv4Only {
			return address.Is4()
		}
		return address.Is6()
	case shareListenAddress:
		return binding.address == address
	default:
		return false
	}
}

func (binding shareListenBinding) matchesCurrent(current normalizedShareOrigin, addresses []shareInterfaceAddress) string {
	currentPort, ok := effectiveShareOriginPort(current)
	if !ok || binding.port < 1 || currentPort != binding.port {
		// A different browser-facing port commonly means Vite, a reverse proxy,
		// NAT, or another port mapping. The socket relationship is therefore
		// unknown rather than a host-only match or mismatch.
		return shareListenMatchUnknown
	}
	address, addressErr := netip.ParseAddr(current.Host)
	if addressErr == nil {
		address = address.Unmap()
	}
	switch binding.kind {
	case shareListenAddress:
		if addressErr != nil {
			return shareListenMatchUnknown
		}
		if binding.address == address {
			return shareListenMatch
		}
		return shareListenMismatch
	case shareListenHostname:
		// Even equal DNS spelling does not identify the address family selected
		// by net.Listen or prove that the frontend bypasses a reverse proxy.
		return shareListenMatchUnknown
	case shareListenWildcard:
		if addressErr != nil {
			// A DNS origin may resolve to either family or terminate at a proxy.
			return shareListenMatchUnknown
		}
		if !binding.matchesAddress(address) {
			return shareListenMismatch
		}
		if address.IsLoopback() {
			return shareListenMatch
		}
		for _, candidate := range addresses {
			if candidate.Address.IsValid() && candidate.Address.Unmap() == address {
				return shareListenMatch
			}
		}
		return shareListenMismatch
	}
	return shareListenMatchUnknown
}

func effectiveShareOriginPort(origin normalizedShareOrigin) (int, bool) {
	if origin.Port != "" {
		port, err := strconv.Atoi(origin.Port)
		return port, err == nil && port >= 1 && port <= 65535
	}
	switch origin.Scheme {
	case "http":
		return 80, true
	case "https":
		return 443, true
	default:
		return 0, false
	}
}

func buildShareOriginCandidates(listenEndpoint ListenEndpoint, current normalizedShareOrigin, addresses []shareInterfaceAddress) []shareOriginDTO {
	binding := shareListenBindingFromEndpoint(listenEndpoint)
	addresses = normalizedShareInterfaceAddresses(addresses)
	listen := &shareListenDTO{
		Source:    "listen",
		Network:   listenEndpoint.Network,
		Family:    listenEndpoint.Family,
		Mode:      listenEndpoint.Mode,
		Host:      listenEndpoint.Host,
		Port:      listenEndpoint.Port,
		Address:   listenEndpoint.Address(),
		Reachable: shareOriginReachabilityUnknown,
	}
	items := make([]shareOriginDTO, 0, len(addresses)+1)
	indexes := make(map[string]int, len(addresses)+1)
	add := func(item shareOriginDTO) {
		if item.Origin == "" {
			return
		}
		if item.Reachable == "" {
			item.Reachable = shareOriginReachabilityUnknown
		}
		if item.Listen == nil {
			item.Listen = listen
		}
		applyShareListenMatch(&item, item.ListenMatchStatus)
		if len(item.Sources) == 0 && item.Source != "" {
			item.Sources = []string{item.Source}
		}
		if item.Interface != "" && len(item.Interfaces) == 0 {
			item.Interfaces = []string{item.Interface}
		}
		if index, exists := indexes[item.Origin]; exists {
			existing := &items[index]
			for _, source := range item.Sources {
				existing.Sources = appendUniqueString(existing.Sources, source)
			}
			for _, name := range item.Interfaces {
				existing.Interfaces = appendUniqueString(existing.Interfaces, name)
			}
			if existing.Interface == "" && item.Interface != "" {
				existing.Interface = item.Interface
			}
			applyShareListenMatch(existing, mergeShareListenMatch(existing.ListenMatchStatus, item.ListenMatchStatus))
			return
		}
		indexes[item.Origin] = len(items)
		items = append(items, item)
	}

	if current.Origin != "" {
		add(shareOriginDTO{
			Origin:            current.Origin,
			Label:             "当前访问地址",
			Source:            "current",
			Sources:           []string{"current"},
			Scope:             shareHostScope(current.Host),
			ListenMatchStatus: binding.matchesCurrent(current, addresses),
		})
	}
	// Top-level source "configured" remains reserved for an explicitly
	// configured public access origin (currently composed by the frontend).
	// cfg.Server.Host is a socket setting and is represented only by Listen.

	for _, candidate := range addresses {
		// Loopback is represented by the current origin or nested socket
		// diagnostics; emitting a proxy-shaped https://127.0.0.1 share URL would
		// conflate frontend access with the backend listener.
		if candidate.Address.IsLoopback() {
			continue
		}
		if !binding.matchesAddress(candidate.Address) {
			continue
		}
		address := candidate.Address.Unmap()
		origin := originFromShareHost(current.Scheme, address.String(), current.Port)
		listenMatchStatus := shareListenMatchUnknown
		if normalized, ok := normalizeShareOrigin(origin); ok {
			listenMatchStatus = binding.matchesCurrent(normalized, addresses)
		}
		label := "网络接口 " + address.String()
		if address.IsLoopback() {
			label = "本机地址 " + address.String()
		} else if address.IsPrivate() {
			label = "局域网 " + address.String()
		}
		add(shareOriginDTO{
			Origin:            origin,
			Label:             label,
			Source:            "interface",
			Sources:           []string{"interface"},
			Scope:             shareAddressScope(address),
			Interface:         candidate.Interface,
			ListenMatchStatus: listenMatchStatus,
		})
	}

	sort.SliceStable(items, func(i, j int) bool {
		left, right := shareOriginSourceRank(items[i].Source), shareOriginSourceRank(items[j].Source)
		if left != right {
			return left < right
		}
		return items[i].Origin < items[j].Origin
	})
	return items
}

func applyShareListenMatch(item *shareOriginDTO, status string) {
	if status == "" {
		status = shareListenMatchUnknown
	}
	item.ListenMatchStatus = status
	switch status {
	case shareListenMatch:
		value := true
		item.ListenMatch = &value
	case shareListenMismatch:
		value := false
		item.ListenMatch = &value
	default:
		item.ListenMatch = nil
	}
}

func mergeShareListenMatch(left, right string) string {
	if left == shareListenMatch || right == shareListenMatch {
		return shareListenMatch
	}
	if left == shareListenMismatch && right == shareListenMismatch {
		return shareListenMismatch
	}
	return shareListenMatchUnknown
}

func normalizedShareInterfaceAddresses(values []shareInterfaceAddress) []shareInterfaceAddress {
	out := make([]shareInterfaceAddress, 0, len(values))
	for _, value := range values {
		if !value.Address.IsValid() {
			continue
		}
		value.Address = value.Address.Unmap()
		if value.Address.IsUnspecified() || value.Address.IsMulticast() || value.Address.IsLinkLocalUnicast() || value.Address.IsLinkLocalMulticast() {
			continue
		}
		if !value.Address.IsGlobalUnicast() && !value.Address.IsLoopback() {
			continue
		}
		value.Interface = strings.TrimSpace(value.Interface)
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool {
		if left, right := out[i].Address.String(), out[j].Address.String(); left != right {
			return left < right
		}
		return out[i].Interface < out[j].Interface
	})
	return out
}

func localShareInterfaceAddresses() []shareInterfaceAddress {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]shareInterfaceAddress, 0)
	for _, networkInterface := range interfaces {
		if networkInterface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, err := networkInterface.Addrs()
		if err != nil {
			continue
		}
		for _, raw := range addresses {
			address, ok := netipFromNetworkAddress(raw)
			if !ok {
				continue
			}
			out = append(out, shareInterfaceAddress{Interface: networkInterface.Name, Address: address})
		}
	}
	return normalizedShareInterfaceAddresses(out)
}

func netipFromNetworkAddress(value net.Addr) (netip.Addr, bool) {
	var ip net.IP
	switch address := value.(type) {
	case *net.IPNet:
		ip = address.IP
	case *net.IPAddr:
		ip = address.IP
	default:
		return netip.Addr{}, false
	}
	parsed, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Addr{}, false
	}
	return parsed.Unmap(), true
}

func originFromShareHost(scheme, host, port string) string {
	if scheme != "http" && scheme != "https" || host == "" {
		return ""
	}
	return scheme + "://" + formatShareAuthority(host, port)
}

func shareHostScope(host string) string {
	if strings.EqualFold(host, "localhost") {
		return "loopback"
	}
	if address, err := netip.ParseAddr(host); err == nil {
		return shareAddressScope(address.Unmap())
	}
	return "hostname"
}

func shareAddressScope(address netip.Addr) string {
	address = address.Unmap()
	if address.IsLoopback() {
		return "loopback"
	}
	if carrierGradeNATPrefix.Contains(address) {
		return "carrier-grade-nat"
	}
	if address.IsPrivate() {
		return "private"
	}
	if address.IsLinkLocalUnicast() {
		return "link-local"
	}
	if address.IsGlobalUnicast() {
		return "global-unicast"
	}
	return "other"
}

func appendUniqueString(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func shareOriginSourceRank(source string) int {
	switch source {
	case "current":
		return 0
	case "configured":
		return 1
	default:
		return 2
	}
}

// Compatibility helpers retained for existing in-package consumers and tests.
// The fallback deliberately uses the socket request and Host only; forwarded
// headers require Server.requestOrigin so that trusted-proxy policy is applied.
func shareOriginSchemePort(currentOrigin string, c *fiber.Ctx) (string, string) {
	if current, ok := normalizeShareOrigin(currentOrigin); ok {
		return current.Scheme, current.Port
	}
	scheme := "http"
	if c != nil && c.Context().IsTLS() {
		scheme = "https"
	}
	if c != nil {
		if current, ok := normalizeShareOrigin(scheme + "://" + string(c.Context().Host())); ok {
			return current.Scheme, current.Port
		}
	}
	return scheme, ""
}

func localShareIPs() []net.IP {
	addresses := localShareInterfaceAddresses()
	out := make([]net.IP, 0, len(addresses))
	seen := make(map[string]struct{}, len(addresses))
	for _, candidate := range addresses {
		if candidate.Address.IsLoopback() {
			continue
		}
		value := candidate.Address.String()
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, net.ParseIP(value))
	}
	return out
}

func originFromIP(scheme string, ip net.IP, port string) string {
	if ip == nil {
		return ""
	}
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return ""
	}
	return originFromShareHost(scheme, address.Unmap().String(), port)
}
