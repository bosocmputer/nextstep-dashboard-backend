package sml

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"path"
	"strings"
)

type LookupNetIPFunc func(context.Context, string, string) ([]net.IP, error)

type EndpointPolicy struct {
	AllowedPrefixes []netip.Prefix
	AllowedHosts    []string
	LookupNetIP     LookupNetIPFunc
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
	if len(policy.AllowedPrefixes) == 0 && len(policy.AllowedHosts) == 0 {
		return ResolvedEndpoint{}, errors.New("SML endpoint allowlist is empty")
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
		allowedByHostname := policy.allowedHost(hostname) && address.IsGlobalUnicast() && !address.IsPrivate() && !address.IsLoopback()
		if isAlwaysBlockedAddress(address) || (!policy.allowed(address) && !allowedByHostname) {
			return ResolvedEndpoint{}, errors.New("SML endpoint address is not permitted")
		}
	}
	return ResolvedEndpoint{URL: endpoint, IP: addresses[0]}, nil
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
