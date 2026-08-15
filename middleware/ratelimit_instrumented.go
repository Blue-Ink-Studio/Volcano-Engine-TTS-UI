package middleware

// 本文件提供带 metrics 埋点的限流 / 并发中间件版本;
// 由于原 ratelimit_middleware.go 在本仓库的云盘同步下被永久占用,
// 这里用独立实现覆盖路由使用入口,旧实现保留为未引用代码。
//
// 行为与原 ratelimit_middleware.go 完全一致,只是多了 metrics 调用。

import (
	"log"
	"net/http"

	"github.com/volcano-tts/tts-api/metrics"
)

// RateLimitWithMetrics 是 middleware.RateLimit 的可埋点版本。
func RateLimitWithMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientIP := GetClientIP(r)
		if !GlobalRateLimiter.Allow(clientIP) {
			log.Printf("警告: 已超过IP速率限制，拒绝请求 - 客户端IP: %s", clientIP)
			SendJSONError(w, http.StatusTooManyRequests, "Rate limit exceeded. Please try again later.", "rate_limit_error", "rate_limit_exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ConcurrencyLimitWithMetrics 是 middleware.ConcurrencyLimit 的可埋点版本。
func ConcurrencyLimitWithMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
