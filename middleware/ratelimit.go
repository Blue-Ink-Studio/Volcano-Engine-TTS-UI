package middleware

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/volcano-tts/tts-api/common"
)

type RateLimiter struct {
	requests    map[string][]time.Time
	mutex       sync.Mutex
	limit       int
	window      time.Duration
	lastCleanup time.Time
}

var (
	GlobalRateLimiter *RateLimiter
	ConcurrencySem    chan struct{}
)

func InitRateLimiter() {
	GlobalRateLimiter = &RateLimiter{
		requests: make(map[string][]time.Time),
		limit:    common.RateLimitRequests,
		window:   common.RateLimitWindow,
	}
	ConcurrencySem = make(chan struct{}, common.MaxConcurrentRequests)
}

func (rl *RateLimiter) Allow(key string) bool {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)

	if now.Sub(rl.lastCleanup) > common.CleanupInterval {
		rl.cleanup()
		rl.lastCleanup = now
	}

	timestamps := rl.requests[key]
	valid := make([]time.Time, 0, len(timestamps))
	for _, ts := range timestamps {
		if ts.After(cutoff) {
			valid = append(valid, ts)
		}
	}

	if len(valid) >= rl.limit {
		rl.requests[key] = valid
		return false
	}

	valid = append(valid, now)
	rl.requests[key] = valid
	return true
}

func (rl *RateLimiter) cleanup() {
	cutoff := time.Now().Add(-rl.window)
	for k, v := range rl.requests {
		valid := make([]time.Time, 0, len(v))
		for _, ts := range v {
			if ts.After(cutoff) {
				valid = append(valid, ts)
			}
		}
		if len(valid) == 0 {
			delete(rl.requests, k)
		} else {
			rl.requests[k] = valid
		}
	}
}

func GetClientIP(r *http.Request) string {
	xForwardedFor := r.Header.Get("X-Forwarded-For")
	if xForwardedFor != "" {
		ips := strings.Split(xForwardedFor, ",")
		if len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}

	xRealIP := r.Header.Get("X-Real-IP")
	if xRealIP != "" {
		return xRealIP
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
