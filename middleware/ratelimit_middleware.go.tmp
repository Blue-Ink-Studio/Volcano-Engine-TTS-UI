package middleware

import (
	"log"
	"net/http"
)

func RateLimit(next http.Handler) http.Handler {
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

func ConcurrencyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case ConcurrencySem <- struct{}{}:
			defer func() { <-ConcurrencySem }()
			next.ServeHTTP(w, r)
		default:
			log.Printf("警告: 已达到最大并发请求数限制，拒绝请求 - 客户端IP: %s", GetClientIP(r))
			SendJSONError(w, http.StatusServiceUnavailable, "Server is busy, maximum concurrent requests reached. Please try again later.", "concurrency_limit_error", "max_concurrent_requests")
			return
		}
	})
}
