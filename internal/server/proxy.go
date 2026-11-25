package server

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// clientInfo is who sent a request, after looking through trusted proxies.
type clientInfo struct {
	ip    string // the client's address (the proxy's own if untrusted)
	proxy string // the proxy's address when the request came through one
	https bool   // the client used https (TLS here, or a trusted proxy says so)
}

const clientInfoKey ctxKey = 100

func clientFrom(r *http.Request) clientInfo {
	if c, ok := r.Context().Value(clientInfoKey).(clientInfo); ok {
		return c
	}
	return clientInfo{ip: r.RemoteAddr, https: r.TLS != nil}
}

// clientIPMiddleware works out the real client behind trusted reverse
// proxies (Caddy, nginx). X-Forwarded-For and X-Forwarded-Proto are only
// believed when the direct peer is a trusted proxy; anyone else could send
// them. Within X-Forwarded-For the right-most untrusted address is the
// client: entries further left were supplied by the client itself and can
// be forged.
func clientIPMiddleware(trusted []netip.Prefix) func(http.Handler) http.Handler {
	isTrusted := func(a netip.Addr) bool {
		for _, p := range trusted {
			if p.Contains(a.Unmap()) {
				return true
			}
		}
		return false
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c := clientInfo{ip: r.RemoteAddr, https: r.TLS != nil}
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err == nil {
				c.ip = host
			}
			peer, err := netip.ParseAddr(host)
			if err == nil && len(trusted) > 0 && isTrusted(peer) {
				c.proxy = host
				hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
				for i := len(hops) - 1; i >= 0; i-- {
					a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
					if err != nil {
						break // garbage: stop at the last address we could trust
					}
					c.ip = a.Unmap().String()
					if !isTrusted(a) {
						break
					}
				}
				if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
					c.https = p == "https"
				}
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientInfoKey, c)))
		})
	}
}

// ParseTrustedProxies turns addresses and CIDR ranges ("127.0.0.1",
// "10.0.0.0/8", "::1") into prefixes.
func ParseTrustedProxies(list []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, err
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}
