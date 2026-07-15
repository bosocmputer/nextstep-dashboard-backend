package sml

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// CanonicalHostKey identifies the JavaWS origin without retaining or exposing
// the customer's hostname. Paths intentionally do not affect host concurrency.
func CanonicalHostKey(rawURL string) ([32]byte, error) {
	endpoint, err := url.Parse(rawURL)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return [32]byte{}, errors.New("SML endpoint URL is invalid")
	}
	port := endpoint.Port()
	if port == "" {
		if endpoint.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	canonical := strings.ToLower(endpoint.Scheme) + "://" + net.JoinHostPort(strings.ToLower(strings.TrimSuffix(endpoint.Hostname(), ".")), port)
	return sha256.Sum256([]byte(canonical)), nil
}

type LookupNetIPFunc func(context.Context, string, string) ([]net.IP, error)

type EndpointPolicy struct {
	AllowedPrefixes      []netip.Prefix
	AllowedHosts         []string
	AllowPublicEndpoints bool
	AllowedPorts         []uint16
	LookupNetIP          LookupNetIPFunc
}

type ResolvedEndpoint struct {
	URL *url.URL
	IP  netip.Addr
}

func (policy EndpointPolicy) Resolve(ctx context.Context, rawURL string) (ResolvedEndpoint, error) {
	endpoint, err := url.Parse(rawURL)
	if err != nil || endpoint.Host == "" {
		return ResolvedEndpoint{}, errors.New("SML endpoint URL is invalid")
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return ResolvedEndpoint{}, errors.New("SML endpoint must use HTTP or HTTPS")
	}
	if endpoint.User != nil || endpoint.Fragment != "" {
		return ResolvedEndpoint{}, errors.New("SML endpoint must not include user information or a fragment")
	}
	if endpoint.RawQuery != "" || endpoint.RawPath != "" {
		return ResolvedEndpoint{}, errors.New("SML endpoint must not include a query or encoded path")
	}
	if len(policy.AllowedPrefixes) == 0 && len(policy.AllowedHosts) == 0 && !policy.AllowPublicEndpoints {
		return ResolvedEndpoint{}, errors.New("SML endpoint allowlist is empty")
	}
	port := endpoint.Port()
	if port == "" {
		if endpoint.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	parsedPort, err := strconv.ParseUint(port, 10, 16)
	if err != nil || !policy.allowedPort(uint16(parsedPort)) {
		return ResolvedEndpoint{}, errors.New("SML endpoint port is not permitted")
	}
	endpoint.Path = normalizeJavaWSPath(endpoint.Path)

	hostname := endpoint.Hostname()
	addresses := make([]netip.Addr, 0, 2)
	if address, err := netip.ParseAddr(hostname); err == nil {
		addresses = append(addresses, address.Unmap())
	} else {
		lookup := policy.LookupNetIP
		if lookup == nil {
			lookup = func(ctx context.Context, network, host string) ([]net.IP, error) {
				return net.DefaultResolver.LookupIP(ctx, network, host)
			}
		}
		resolved, err := lookup(ctx, "ip", hostname)
		if err != nil || len(resolved) == 0 {
			return ResolvedEndpoint{}, errors.New("SML endpoint hostname could not be resolved")
		}
		for _, value := range resolved {
			address, ok := netip.AddrFromSlice(value)
			if !ok {
				return ResolvedEndpoint{}, errors.New("SML endpoint resolved to an invalid address")
			}
			addresses = append(addresses, address.Unmap())
		}
	}
	for _, address := range addresses {
		isSafePublicAddress := address.IsGlobalUnicast() && !address.IsPrivate() && !address.IsLoopback() && !isAlwaysBlockedAddress(address)
		allowedByHostname := policy.allowedHost(hostname) && isSafePublicAddress
		allowedByPublicEndpoint := policy.AllowPublicEndpoints && isSafePublicAddress
		if isAlwaysBlockedAddress(address) || (!policy.allowed(address) && !allowedByHostname && !allowedByPublicEndpoint) {
			return ResolvedEndpoint{}, errors.New("SML endpoint address is not permitted")
		}
	}
	return ResolvedEndpoint{URL: endpoint, IP: addresses[0]}, nil
}

func (policy EndpointPolicy) allowedPort(port uint16) bool {
	if len(policy.AllowedPorts) == 0 {
		return true
	}
	for _, allowed := range policy.AllowedPorts {
		if port == allowed {
			return true
		}
	}
	return false
}

func (policy EndpointPolicy) allowedHost(hostname string) bool {
	hostname = strings.ToLower(strings.TrimSuffix(hostname, "."))
	for _, allowed := range policy.AllowedHosts {
		if hostname == strings.ToLower(strings.TrimSuffix(allowed, ".")) {
			return true
		}
	}
	return false
}

func normalizeJavaWSPath(rawPath string) string {
	cleaned := path.Clean("/" + strings.TrimPrefix(rawPath, "/"))
	switch strings.ToLower(cleaned) {
	case "/", "/.":
		return "/SMLJavaWebService/DotNetFrameWork"
	case "/smljavawebservice":
		return "/SMLJavaWebService/DotNetFrameWork"
	default:
		return cleaned
	}
}

func (policy EndpointPolicy) allowed(address netip.Addr) bool {
	for _, prefix := range policy.AllowedPrefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func isAlwaysBlockedAddress(address netip.Addr) bool {
	if !address.IsValid() || address.IsUnspecified() || address.IsMulticast() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() {
		return true
	}
	metadata := netip.MustParseAddr("169.254.169.254")
	return address == metadata || strings.EqualFold(address.String(), "fd00:ec2::254")
}
