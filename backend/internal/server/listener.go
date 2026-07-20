package server

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

const (
	ListenNetworkTCP  = "tcp"
	ListenNetworkTCP4 = "tcp4"
	ListenNetworkTCP6 = "tcp6"

	ListenFamilyIPv4    = "ipv4"
	ListenFamilyIPv6    = "ipv6"
	ListenFamilyUnknown = "unknown"

	ListenModeWildcard = "wildcard"
	ListenModeSpecific = "specific"
	ListenModeHostname = "hostname"
)

// ListenEndpoint is the normalized socket contract shared by Fiber and the
// address diagnostics API. Hostnames intentionally use network "tcp": name
// resolution chooses one concrete family at listen time, so callers must not
// infer IPv4, IPv6, or dual-stack behavior from a hostname configuration.
type ListenEndpoint struct {
	Host    string
	Port    int
	Network string
	Family  string
	Mode    string
}

func (endpoint ListenEndpoint) Address() string {
	return net.JoinHostPort(endpoint.Host, strconv.Itoa(endpoint.Port))
}

// ResolveListenEndpoint is pure: it normalizes optional IPv6 brackets and
// makes the configured socket family explicit without resolving DNS.
func ResolveListenEndpoint(host string, port int) (ListenEndpoint, error) {
	if port < 1 || port > 65535 {
		return ListenEndpoint{}, fmt.Errorf("server port must be between 1 and 65535")
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return ListenEndpoint{}, fmt.Errorf("server host must not be empty")
	}
	if strings.HasPrefix(host, "[") || strings.HasSuffix(host, "]") {
		if len(host) < 3 || host[0] != '[' || host[len(host)-1] != ']' {
			return ListenEndpoint{}, fmt.Errorf("server host has malformed IPv6 brackets")
		}
		unbracketed := host[1 : len(host)-1]
		address, err := netip.ParseAddr(unbracketed)
		if err != nil || !address.Is6() {
			return ListenEndpoint{}, fmt.Errorf("bracketed server host must be an IPv6 address")
		}
		host = unbracketed
	}

	if address, err := netip.ParseAddr(host); err == nil {
		if address.Is4In6() {
			return ListenEndpoint{}, fmt.Errorf("IPv4-mapped IPv6 server host is ambiguous; use its IPv4 form")
		}
		address = address.Unmap()
		endpoint := ListenEndpoint{Host: address.String(), Port: port, Mode: ListenModeSpecific}
		if address.Is4() {
			endpoint.Network = ListenNetworkTCP4
			endpoint.Family = ListenFamilyIPv4
		} else {
			endpoint.Network = ListenNetworkTCP6
			endpoint.Family = ListenFamilyIPv6
		}
		if address.IsUnspecified() {
			endpoint.Mode = ListenModeWildcard
		}
		return endpoint, nil
	}

	// A colon in a non-IP host would be an embedded port or malformed IPv6.
	if strings.ContainsAny(host, "[]:") {
		return ListenEndpoint{}, fmt.Errorf("server host must not include brackets or a port")
	}
	normalized, ok := normalizeOriginHost(host)
	if !ok || strings.Contains(normalized, ":") {
		return ListenEndpoint{}, fmt.Errorf("server host is invalid")
	}
	return ListenEndpoint{
		Host:    normalized,
		Port:    port,
		Network: ListenNetworkTCP,
		Family:  ListenFamilyUnknown,
		Mode:    ListenModeHostname,
	}, nil
}
