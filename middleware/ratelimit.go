package middleware

import (
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/volcano-tts/tts-api/common"
	"github.com/volcano-tts/tts-api/metrics"
	"github.com/volcano-tts/tts-api/setting"
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
	// trustedProxyHops controls how X-Forwarded-For (XFF) is parsed when the
	// direct connection comes from a private IP (i.e., we're behind a reverse
	// proxy). Two modes are supported, switched by this single value:
	//
	//   HEURISTIC MODE (trustedProxyHops == 0, the default):
	//     Walk XFF from the end, return the first PUBLIC IP. Skips private
	//     and loopback hops automatically. Works for ~90% of deployments
	//     without the operator needing to know the exact number of proxy
	//     hops. Trade-off in multi-hop: rate limiting is per-CDN-edge rather
	//     than per-real-client, which is "good enough" for abuse protection
	//     but not for fine-grained per-user quotas.
	//
	//   PRECISE MODE (trustedProxyHops > 0):
	//     Count back N hops from the end of XFF and return that value. Gives
	//     precise per-real-client rate limiting even in multi-hop setups
	//     (e.g., Cloudflare + nginx). Operator MUST set this to the number
	//     of trusted reverse proxies between this service and the client.
	//
	// Both modes walk from the END of the XFF chain. The first value is
	// client-controllable; trusting it would let attackers bypass IP rate
	// limiting by sending a forged X-Forwarded-For header.
	trustedProxyHops = 0
)

func InitRateLimiter() {
	switch v := os.Getenv("TRUSTED_PROXY_HOPS"); {
	case v == "":
		log.Printf("TRUSTED_PROXY_HOPS 未设置,使用默认启发式模式(XFF 链尾第一个公网 IP)")
	default:
		n, err := strconv.Atoi(v)
		switch {
		case err != nil || n < 0 || n > 10:
			log.Printf("警告: TRUSTED_PROXY_HOPS=%q 无效(需 0-10 的整数),回退到默认启发式模式", v)
		case n == 0:
			// "0" 或 "00" 等被 Atoi 解析为 0 的形式都归到启发式模式,
			// 避免日志出现"精确模式, 信任 0 跳"这种自相矛盾的输出。
			log.Printf("已配置 TRUSTED_PROXY_HOPS=%d(启发式模式,等同默认)", n)
		default:
			trustedProxyHops = n
			log.Printf("已配置 TRUSTED_PROXY_HOPS=%d(精确模式,信任 %d 跳反代)", n, n)
		}
	}
	GlobalRateLimiter = &RateLimiter{
		requests: make(map[string][]time.Time),
		limit:    common.RateLimitRequests,
		window:   common.RateLimitWindow,
	}
	ConcurrencySem = make(chan struct{}, common.MaxConcurrentRequests)

	// 同步到 setting 包,供 LogStartupSummary 展示
	setting.TrustedProxyHops = trustedProxyHops
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
		metrics.RateLimitRejected.Inc(nil)
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

func GetClientIP(r *http.Request) string {
	directIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		directIP = r.RemoteAddr
	}

	if isPrivateIP(directIP) {
		// Parse X-Forwarded-For when there's a reverse proxy in front (direct
		// connection is from a private IP). Both modes walk from the END of
		// the chain so that the client-controllable first value cannot be
		// used to spoof a different client IP for rate limit bypass.
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if trustedProxyHops > 0 {
				// PRECISE MODE: count back N hops from end. Real client IP
				// sits at index (len(parts) - N). Walk backwards to skip
				// any malformed values; if chain is shorter than expected,
				// fall through to the first valid IP in the chain.
				target := len(parts) - trustedProxyHops
				if target < 0 {
					target = 0
				}
				for i := target; i >= 0; i-- {
					ip := strings.TrimSpace(parts[i])
					if net.ParseIP(ip) != nil {
						return ip
					}
				}
			} else {
				// HEURISTIC MODE (default): walk from end, return first
				// PUBLIC IP. Skips private/loopback hops that come from
				// internal proxies between the public-facing proxy and us.
				for i := len(parts) - 1; i >= 0; i-- {
					ip := strings.TrimSpace(parts[i])
					if parsed := net.ParseIP(ip); parsed != nil && !isPrivateIP(ip) {
						return ip
					}
				}
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
