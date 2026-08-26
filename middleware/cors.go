package middleware

import (
	"log"
	"net/http"
	"strings"

	"github.com/volcano-tts/tts-api/common"
	"github.com/volcano-tts/tts-api/setting"
)

var (
	corsMaxAgeHeader = "86400"
)

func isValidOrigin(origin string) bool {
	if origin == "" || origin == "null" || origin == "nil" {
		return false
	}
	lowerOrigin := strings.ToLower(origin)
	if !strings.HasPrefix(lowerOrigin, "http://") && !strings.HasPrefix(lowerOrigin, "https://") {
		return false
	}
	return true
}

func matchOrigin(origin string) (string, bool) {
	if !isValidOrigin(origin) {
		return "", false
	}
	if setting.CORS.AllowAll {
		return "*", true
	}
	normalized := strings.ToLower(strings.TrimRight(strings.TrimSpace(origin), "/"))
	for _, allowed := range setting.CORS.Origins {
		if allowed == normalized {
			return origin, true
		}
	}
	return "", false
}

func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		// 无 Origin 头:非跨域请求,跳过 CORS 处理
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}

		// 有 Origin 头时,响应必须携带 Vary: Origin 防止 CDN 缓存污染
		vary := w.Header().Get("Vary")
		if vary == "" {
			w.Header().Set("Vary", "Origin")
		} else if !strings.Contains(vary, "Origin") {
			w.Header().Set("Vary", vary+", Origin")
		}

		isPreflight := r.Method == http.MethodOptions

		allowOrigin, matched := matchOrigin(origin)
		if !matched {
			// Origin 不在白名单:拒绝请求(预检和非预检均拒绝),
			// 防止不匹配的请求穿透到后端浪费 TTS 资源
			if common.DebugLog {
				log.Printf("CORS拦截: 来源=%q 路径=%s 方法=%s 客户端=%s",
					origin, r.URL.Path, r.Method, GetClientIP(r))
			}
			w.WriteHeader(http.StatusForbidden)
			return
		}

		// Origin 匹配:设置 CORS 响应头
		w.Header().Set("Access-Control-Allow-Origin", allowOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Expose-Headers", "X-Request-Id")
		w.Header().Set("Access-Control-Max-Age", corsMaxAgeHeader)
		if allowOrigin != "*" {
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}

		// 预检请求:直接返回 204,不进入内层中间件链,
		// 避免消耗速率限制配额和并发槽位
		if isPreflight {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
