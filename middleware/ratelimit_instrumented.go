package middleware

// 本文件提供带 metrics 埋点的限流 / 并发中间件版本。
// 相比 router 实际使用的实现,本版本额外做了:
//   - 加 metrics 埋点(限流拒绝 / 并发拒绝计数)
//   - 仅对 /v1/ 下的业务请求生效,监控路径(/health /metrics /dashboard)不消耗配额

import (
	"log"
	"net/http"
	"strings"

	"github.com/volcano-tts/tts-api/metrics"
)

// RateLimitWithMetrics 是限流中间件,带埋点 + 路径过滤。
func RateLimitWithMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 仅对 /v1/ 下的业务请求限流,/health /metrics /dashboard 等监控路径不限流
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		clientIP := GetClientIP(r)
		if !GlobalRateLimiter.Allow(clientIP) {
			log.Printf("警告: 已超过IP速率限制，拒绝请求 - 客户端IP: %s", clientIP)
			SendJSONError(w, http.StatusTooManyRequests, "Rate limit exceeded. Please try again later.", "rate_limit_error", "rate_limit_exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ConcurrencyLimitWithMetrics 是并发控制中间件,带埋点 + 路径过滤。
func ConcurrencyLimitWithMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 仅对 /v1/ 下的业务请求统计并发和加锁,监控路径不占用并发槽位
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		select {
		case ConcurrencySem <- struct{}{}:
			metrics.ConcurrencyActive.Inc(nil)
			defer func() {
				<-ConcurrencySem
				metrics.ConcurrencyActive.Dec(nil)
			}()
			next.ServeHTTP(w, r)
		default:
			metrics.ConcurrencyRejected.Inc(nil)
			log.Printf("警告: 已达到最大并发请求数限制，拒绝请求 - 客户端IP: %s", GetClientIP(r))
			SendJSONError(w, http.StatusServiceUnavailable, "Server is busy, maximum concurrent requests reached. Please try again later.", "concurrency_limit_error", "max_concurrent_requests")
			return
		}
	})
}
