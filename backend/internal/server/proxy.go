package server

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"filetrans-backend/internal/config"

	"github.com/gofiber/fiber/v2"
)

const maxForwardedForNodes = 32

type proxyResolver struct {
	trusted []netip.Prefix
}

func newProxyResolver(serverConfig config.ServerConfig) (*proxyResolver, error) {
	resolver := &proxyResolver{}
	if !serverConfig.TrustProxyHeaders {
		if len(serverConfig.TrustedProxyCIDRs) != 0 {
			return nil, fmt.Errorf("trusted proxy CIDRs require proxy trust")
		}
		return resolver, nil
	}
	if len(serverConfig.TrustedProxyCIDRs) == 0 || len(serverConfig.TrustedProxyCIDRs) > 64 {
		return nil, fmt.Errorf("trusted proxy mode requires 1 to 64 CIDRs")
	}
	resolver.trusted = make([]netip.Prefix, 0, len(serverConfig.TrustedProxyCIDRs))
	for _, value := range serverConfig.TrustedProxyCIDRs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy CIDR")
		}
		if prefix.Addr().Is4In6() && prefix.Bits() >= 96 {
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		resolver.trusted = append(resolver.trusted, prefix.Masked())
	}
	return resolver, nil
}

func (r *proxyResolver) isTrusted(address netip.Addr) bool {
	address = address.Unmap()
	for _, prefix := range r.trusted {
		candidate := address
		if prefix.Addr().Is4() {
			candidate = candidate.Unmap()
		}
		if prefix.Contains(candidate) {
			return true
		}
	}
	return false
}

func socketRemoteIP(c *fiber.Ctx) netip.Addr {
	if address, err := parseProxyIP(c.Context().RemoteAddr().String()); err == nil {
		return address
	}
	if address, ok := netip.AddrFromSlice(c.Context().RemoteIP()); ok {
		return address.Unmap()
	}
	return netip.IPv4Unspecified()
}

func parseProxyIP(value string) (netip.Addr, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return netip.Addr{}, fmt.Errorf("empty address")
	}
	if address, err := netip.ParseAddr(value); err == nil {
		return address.Unmap(), nil
	}
	if addressPort, err := netip.ParseAddrPort(value); err == nil {
		return addressPort.Addr().Unmap(), nil
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		if address, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
			return address.Unmap(), nil
		}
	}
	return netip.Addr{}, fmt.Errorf("invalid address")
}

func (r *proxyResolver) resolveClientIP(c *fiber.Ctx) string {
	return r.resolve(socketRemoteIP(c), c.Get("X-Forwarded-For"), c.Get("X-Real-IP"))
}

func (r *proxyResolver) resolve(remote netip.Addr, forwarded, realIPHeader string) string {
	remote = remote.Unmap()
	if !r.isTrusted(remote) {
		return remote.String()
	}
	if forwarded = strings.TrimSpace(forwarded); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		if len(parts) <= maxForwardedForNodes {
			chain := make([]netip.Addr, 0, len(parts)+1)
			valid := true
			for _, part := range parts {
				address, err := parseProxyIP(part)
				if err != nil {
					valid = false
					break
				}
				chain = append(chain, address)
			}
			if valid {
				chain = append(chain, remote)
				index := len(chain) - 1
				for index > 0 && r.isTrusted(chain[index]) {
					index--
				}
				return chain[index].Unmap().String()
			}
		}
	}
	if realIP, err := parseProxyIP(realIPHeader); err == nil {
		return realIP.String()
	}
	return remote.String()
}

func validForwardedProto(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value != "http" && value != "https" {
		return "", false
	}
	return value, true
}

func normalizeOriginHost(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, " ,/\\@\t\r\n") {
		return "", false
	}
	host, port := value, ""
	if strings.HasPrefix(value, "[") {
		end := strings.IndexByte(value, ']')
		if end < 0 {
			return "", false
		}
		host = value[1:end]
		if len(value) > end+1 {
			if value[end+1] != ':' {
				return "", false
			}
			port = value[end+2:]
		}
	} else if strings.Count(value, ":") == 1 {
		var err error
		host, port, err = net.SplitHostPort(value)
		if err != nil {
			return "", false
		}
	} else if strings.Count(value, ":") > 1 {
		return "", false
	}
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", false
		}
	}
	if address, err := netip.ParseAddr(host); err == nil {
		address = address.Unmap()
		if port != "" {
			return net.JoinHostPort(address.String(), port), true
		}
		if address.Is6() {
			return "[" + address.String() + "]", true
		}
		return address.String(), true
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || len(host) > 253 {
		return "", false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for _, character := range label {
			if character != '-' && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
				return "", false
			}
		}
	}
	if port != "" {
		return net.JoinHostPort(host, port), true
	}
	return host, true
}
