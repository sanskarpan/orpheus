// Package ssrfguard validates outbound URLs (webhook targets) so a
// tenant cannot point the server at internal infrastructure — cloud
// metadata endpoints (169.254.169.254), loopback, RFC1918/ULA private
// ranges, or link-local addresses. It provides both a fast
// registration-time check and an authoritative dial-time guard that
// closes the DNS-rebinding window by re-checking the IP the dialer
// actually connects to (and every redirect hop).
package ssrfguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// ErrDisallowed is returned when a URL is not a permitted public target.
type ErrDisallowed struct{ Reason string }

func (e *ErrDisallowed) Error() string { return "ssrf: " + e.Reason }

// IsDisallowedIP reports whether ip falls in a range that must never be
// reachable from a user-controlled outbound request. This is the single
// source of truth used by both the registration check and the dialer.
func IsDisallowedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	return ip.IsLoopback() || // 127/8, ::1
		ip.IsPrivate() || // 10/8, 172.16/12, 192.168/16, fc00::/7
		ip.IsLinkLocalUnicast() || // 169.254/16 (incl. metadata), fe80::/10
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() // 0.0.0.0, ::
}

// Resolver resolves a host to IPs. *net.Resolver satisfies it; tests
// inject a stub to stay hermetic.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// ValidateURLStatic performs the DNS-free portion of validation: it
// requires an https scheme and rejects IP literals in disallowed ranges
// and the "localhost" name. It deliberately does NOT resolve hostnames —
// registration-time DNS is both flaky and pointless (an attacker can
// rebind after registration), so the authoritative resolved-IP check
// lives in the delivery dialer (SafeHTTPClient). Use this at
// registration; use SafeHTTPClient at delivery.
func ValidateURLStatic(rawURL string) error {
	u, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return &ErrDisallowed{Reason: "url invalid"}
	}
	if u.Scheme != "https" {
		return &ErrDisallowed{Reason: "url must be https"}
	}
	host := u.Hostname()
	if host == "" {
		return &ErrDisallowed{Reason: "url has no host"}
	}
	if host == "localhost" {
		return &ErrDisallowed{Reason: "host is not public"}
	}
	if ip := net.ParseIP(host); ip != nil && IsDisallowedIP(ip) {
		return &ErrDisallowed{Reason: "host is a private or link-local address"}
	}
	return nil
}

// ValidateURL parses rawURL, requires an https scheme, and rejects the
// request if the host is an IP literal in a disallowed range or resolves
// (via resolver) to any disallowed IP. A nil resolver uses the default.
func ValidateURL(ctx context.Context, rawURL string, resolver Resolver) error {
	u, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return &ErrDisallowed{Reason: "url invalid"}
	}
	if u.Scheme != "https" {
		return &ErrDisallowed{Reason: "url must be https"}
	}
	host := u.Hostname()
	if host == "" {
		return &ErrDisallowed{Reason: "url has no host"}
	}
	// IP literal: check directly, no DNS.
	if ip := net.ParseIP(host); ip != nil {
		if IsDisallowedIP(ip) {
			return &ErrDisallowed{Reason: "host resolves to a private or link-local address"}
		}
		return nil
	}
	// Reject obvious internal names before paying for DNS.
	if host == "localhost" {
		return &ErrDisallowed{Reason: "host is not public"}
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	ips, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return &ErrDisallowed{Reason: "host does not resolve"}
	}
	if len(ips) == 0 {
		return &ErrDisallowed{Reason: "host does not resolve"}
	}
	for _, ipa := range ips {
		if IsDisallowedIP(ipa.IP) {
			return &ErrDisallowed{Reason: "host resolves to a private or link-local address"}
		}
	}
	return nil
}

// SafeHTTPClient returns an *http.Client whose dialer refuses to connect
// to disallowed IPs (authoritative, rebinding-safe because it checks the
// address actually being dialed) and whose redirect policy re-validates
// every hop's URL.
func SafeHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, ipa := range ips {
				if IsDisallowedIP(ipa.IP) {
					return nil, &ErrDisallowed{Reason: fmt.Sprintf("blocked dial to %s", ipa.IP)}
				}
			}
			// Dial the first allowed IP explicitly so the connection
			// cannot race a rebind to a different address.
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		},
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("stopped after 5 redirects")
			}
			return ValidateURL(req.Context(), req.URL.String(), nil)
		},
	}
}
