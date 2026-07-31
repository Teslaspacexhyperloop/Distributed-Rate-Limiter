// Package security provides IP-based access control for the gateway.
package security

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
)

type contextKey int

const whitelistedKey contextKey = 0

// IPFilter holds parsed CIDR ranges for whitelist and blacklist decisions.
// Whitelisted IPs bypass rate limiting entirely.
// Blacklisted IPs are rejected with 403 before any other processing.
type IPFilter struct {
	whitelist []*net.IPNet
	blacklist []*net.IPNet
}

// NewIPFilter parses comma-separated CIDR strings.
// Plain IPs without a mask (e.g. "1.2.3.4") are treated as /32 (IPv4) or /128 (IPv6).
// Either argument may be empty to disable that list.
func NewIPFilter(whitelistCIDRs, blacklistCIDRs string) (*IPFilter, error) {
	wl, err := parseCIDRs(whitelistCIDRs)
	if err != nil {
		return nil, fmt.Errorf("whitelist: %w", err)
	}
	bl, err := parseCIDRs(blacklistCIDRs)
	if err != nil {
		return nil, fmt.Errorf("blacklist: %w", err)
	}
	return &IPFilter{whitelist: wl, blacklist: bl}, nil
}

// Middleware returns HTTP middleware that:
//   - Rejects blacklisted IPs with 403 before anything else runs
//   - Marks whitelisted IPs in the request context so the rate-limit
//     middleware can skip them
func (f *IPFilter) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := realIP(r)

			if f.isBlacklisted(ip) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"access denied"}`))
				return
			}

			ctx := r.Context()
			if f.isWhitelisted(ip) {
				ctx = context.WithValue(ctx, whitelistedKey, true)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// IsWhitelisted returns true if this request's IP was marked whitelisted by
// the IPFilter middleware. Rate-limit middleware reads this to skip the check.
func IsWhitelisted(ctx context.Context) bool {
	v, _ := ctx.Value(whitelistedKey).(bool)
	return v
}

func (f *IPFilter) isWhitelisted(ip net.IP) bool {
	for _, cidr := range f.whitelist {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

func (f *IPFilter) isBlacklisted(ip net.IP) bool {
	for _, cidr := range f.blacklist {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// realIP extracts and parses the client IP from X-Forwarded-For (set by NGINX)
// or RemoteAddr for direct connections.
func realIP(r *http.Request) net.IP {
	addr := r.RemoteAddr
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		addr = strings.SplitN(xff, ",", 2)[0]
	}
	addr = strings.TrimSpace(addr)
	// Strip port if present.
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	}
	return net.ParseIP(addr)
}

func parseCIDRs(s string) ([]*net.IPNet, error) {
	if s == "" {
		return nil, nil
	}
	var nets []*net.IPNet
	for _, raw := range strings.Split(s, ",") {
		cidr := strings.TrimSpace(raw)
		if cidr == "" {
			continue
		}
		// Accept bare IPs by appending the appropriate all-host mask.
		if !strings.Contains(cidr, "/") {
			if strings.Contains(cidr, ":") {
				cidr += "/128"
			} else {
				cidr += "/32"
			}
		}
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", cidr, err)
		}
		nets = append(nets, ipNet)
	}
	return nets, nil
}
