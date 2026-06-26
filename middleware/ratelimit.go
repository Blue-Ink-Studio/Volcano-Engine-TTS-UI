package middleware

import (
	"log"
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

	if len(rl.requests) > common.MaxRateLimiterEntries {
		log.Printf("警告: 限流器条目数 %d 超过上限 %d，触发强制清理", len(rl.requests), common.MaxRateLimiterEntries)
		for k := range rl.requests {
			if len(rl.requests) <= common.MaxRateLimiterEntries/2 {
				break
			}
			delete(rl.requests, k)
		}
	}
}

// 私有网络 CIDR 范围：仅在直连来源属于这些范围时才信任代理头
var privateCIDRs []*net.IPNet

func init() {
	for _, cidr := range []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
	} {
		_, ipNet, _ := net.ParseCIDR(cidr)
		privateCIDRs = append(privateCIDRs, ipNet)
	}
}

func isPrivateIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	for _, cidr := range privateCIDRs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// GetClientIP 提取客户端真实 IP。
// 仅当直连来源为私有网络（本地代理、Docker 网桥等）时才信任 X-Forwarded-For / X-Real-IP，
// 防止公网直连场景下攻击者伪造代理头绕过速率限制。
func GetClientIP(r *http.Request) string {
	directIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		directIP = r.RemoteAddr
	}

	if isPrivateIP(directIP) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			ip := strings.TrimSpace(strings.Split(xff, ",")[0])
			if net.ParseIP(ip) != nil {
				return ip
			}
		}
		if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
			if net.ParseIP(xri) != nil {
				return xri
			}
		}
	}

	return directIP
}
