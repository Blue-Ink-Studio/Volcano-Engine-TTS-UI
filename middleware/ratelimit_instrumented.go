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
//
// STAGE6(P2-6)之后有两档:
//   - /v1/* 业务合成:严格限流(100 次/分钟/IP)
//   - /api/admin/* /api/me/* /api/auth/* 管理与自助:宽松限流(600 次/分钟/IP)
//   - 其余(页面、静态资源、/health、/metrics):不限流
func RateLimitWithMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// 业务接口:严格限流
		if strings.HasPrefix(path, "/v1/") {
			clientIP := GetClientIP(r)
			if !GlobalRateLimiter.Allow(clientIP) {
				log.Printf("警告: 已超过IP速率限制，拒绝请求 - 客户端IP: %s", clientIP)
				SendJSONError(w, http.StatusTooManyRequests, "Rate limit exceeded. Please try again later.", "rate_limit_error", "rate_limit_exceeded")
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// 管理/自助/认证接口:宽松限流(STAGE6 P2-6)
		if isAdminOrSelfPath(path) {
			clientIP := GetClientIP(r)
			if AdminRateLimiter != nil && !AdminRateLimiter.Allow(clientIP) {
				log.Printf("警告: 已超过管理接口速率限制，拒绝请求 - 客户端IP: %s 路径=%s", clientIP, path)
				SendJSONError(w, http.StatusTooManyRequests, "Rate limit exceeded. Please try again later.", "rate_limit_error", "rate_limit_exceeded")
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// 监控路径与页面:不限流
		next.ServeHTTP(w, r)
	})
}

// isAdminOrSelfPath 判断路径是否属于"管理/自助/认证"类接口。
//
// 这三类是审计 P2-6 说的"重查询容易被打垮"的来源:
//   - /api/admin/*  管理端(用户列表、用量统计、渠道列表……)
//   - /api/me/*     用户自助(用量、流水、令牌)
//   - /api/auth/*   登录/注册(未认证可打,最需要保护)
//
// 不包含 /api/setup/*:那是安装期接口,一次性使用,不受影响。
func isAdminOrSelfPath(path string) bool {
	return strings.HasPrefix(path, "/api/admin/") ||
		strings.HasPrefix(path, "/api/me") ||
		strings.HasPrefix(path, "/api/auth/")
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
