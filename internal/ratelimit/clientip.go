package ratelimit

import (
	"fmt"
	"net/http"
	"net/netip"
	"strings"
)

// ParseTrustedProxies parses "10.0.0.0/8, 192.168.1.1" into prefixes.
// These are the proxies allowed to tell us who the real client is.
func ParseTrustedProxies(csv string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(part)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy %q (want an IP or CIDR)", part)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// ClientIP returns the rate-limit key for the caller.
//
// X-Forwarded-For is a header ANY client can send, so it is only believed
// when the TCP connection itself comes from a trusted proxy. Even then we
// read it from the RIGHT: each proxy appends the address it saw, so the
// right-most entries are the ones our own proxies wrote, and anything an
// attacker prepended on the left is never reached. We walk left past
// trusted proxies and stop at the first address that isn't one.
//
// With no trusted proxies configured the header is ignored completely.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	remote, ok := remoteAddr(r)
	if !ok {
		return r.RemoteAddr
	}

	client := remote
	if isTrusted(remote, trusted) {
		if values := r.Header.Values("X-Forwarded-For"); len(values) > 0 {
			client = clientFromXFF(strings.Join(values, ","), remote, trusted)
		}
	}
	return bucketKey(client)
}

func clientFromXFF(xff string, fallback netip.Addr, trusted []netip.Prefix) netip.Addr {
	parts := strings.Split(xff, ",")
	client := fallback

	for i := len(parts) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			return fallback // malformed header: don't guess
		}
		addr = addr.WithZone("").Unmap()
		client = addr
		if !isTrusted(addr, trusted) {
			return addr
		}
	}
	return client // every hop was trusted: the left-most is the client
}

func remoteAddr(r *http.Request) (netip.Addr, bool) {
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return ap.Addr().WithZone("").Unmap(), true
	}
	if a, err := netip.ParseAddr(r.RemoteAddr); err == nil {
		return a.WithZone("").Unmap(), true
	}
	return netip.Addr{}, false
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// bucketKey groups IPv6 clients by /64. One user typically controls a
// whole /64, so keying on the full address would let them dodge the limit
// by rotating addresses.
func bucketKey(addr netip.Addr) string {
	if addr.Is6() {
		return netip.PrefixFrom(addr, 64).Masked().String()
	}
	return addr.String()
}
