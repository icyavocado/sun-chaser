package handlers

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ipRateLimiter implements a sliding-window per-IP rate limiter.
type ipRateLimiter struct {
	mu      sync.Mutex
	entries map[string][]time.Time
	limit   int
	window  time.Duration
}

func newIPRateLimiter(limit int, window time.Duration) *ipRateLimiter {
	rl := &ipRateLimiter{
		entries: make(map[string][]time.Time),
		limit:   limit,
		window:  window,
	}
	go rl.cleanupLoop()
	return rl
}

// Allow reports whether the request from ip is within the rate limit.
// It advances the window on each allowed request.
func (rl *ipRateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)

	times := rl.entries[ip]
	// Slide: drop timestamps older than the window.
	i := 0
	for i < len(times) && times[i].Before(cutoff) {
		i++
	}
	times = times[i:]

	if len(times) >= rl.limit {
		rl.entries[ip] = times
		return false
	}

	rl.entries[ip] = append(times, now)
	return true
}

func (rl *ipRateLimiter) cleanupLoop() {
	ticker := time.NewTicker(rl.window)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		cutoff := time.Now().Add(-rl.window)
		for ip, times := range rl.entries {
			i := 0
			for i < len(times) && times[i].Before(cutoff) {
				i++
			}
			if i == len(times) {
				delete(rl.entries, ip)
			} else {
				rl.entries[ip] = times[i:]
			}
		}
		rl.mu.Unlock()
	}
}

// clientIP extracts the real client IP from the request.
//
// When trustProxy is true (TRUST_PROXY=1 env var, set when running behind
// Traefik/Nginx), X-Real-IP is preferred because Traefik sets it to the
// original client IP. X-Forwarded-For is used as a fallback — only the
// leftmost (client) entry is taken. Both values are validated as proper IP
// addresses to prevent header-injection attacks.
//
// When trustProxy is false, RemoteAddr is used directly to prevent untrusted
// callers from spoofing their apparent IP.
func (h *Handler) clientIP(r *http.Request) string {
	if h.trustProxy {
		if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
			if ip := net.ParseIP(xri); ip != nil {
				return ip.String()
			}
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first := strings.TrimSpace(strings.SplitN(xff, ",", 2)[0])
			if ip := net.ParseIP(first); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
