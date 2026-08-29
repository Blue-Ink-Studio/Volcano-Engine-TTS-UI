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

// isSameOrigin 比较 Origin 与 r.Host,判断是否同源。
//   - 直接访问(server 自己:80): Origin=http://server:80, Host=server:80 → 同源
//   - 反向代理(https://app.example.com → http://server:80):
//     Origin=https://app.example.com, Host=server:80
//     默认不同源;但如果设置 TRUSTED_PROXY_HOPS 或 X-Forwarded-Host,要让它们一致。
//   - 浏览器对同源 POST 也会设 Origin(避免被自己的 CORS 误伤),这里豁免。
// 返回 true 表示请求来自自己,无需 CORS 介入。
func isSameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	// 解析 Origin 的 host 部分
	originHost, originScheme := splitOrigin(origin)
	if originHost == "" {
		return false
	}
	// 优先用 X-Forwarded-Host / X-Forwarded-Proto(反向代理场景),
	// 退而用 r.Host(直接访问场景)
	reqHost := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		// X-Forwarded-Host 可能是 host1, host2 (取第一个)
		if i := strings.Index(fh, ","); i >= 0 {
			fh = strings.TrimSpace(fh[:i])
		}
		reqHost = fh
	}
	reqScheme := "http"
	if r.TLS != nil {
		reqScheme = "https"
	} else if fp := r.Header.Get("X-Forwarded-Proto"); fp != "" {
		if i := strings.Index(fp, ","); i >= 0 {
			fp = strings.TrimSpace(fp[:i])
		}
		reqScheme = strings.ToLower(fp)
	}
	// host 匹配(忽略大小写)
	return strings.EqualFold(originHost, reqHost) && strings.EqualFold(originScheme, reqScheme)
}

// splitOrigin 把 "https://example.com:8080" 拆成 ("example.com:8080", "https")。
// 没有 scheme 时返回 ("", "")。
func splitOrigin(origin string) (host, scheme string) {
	idx := strings.Index(origin, "://")
	if idx < 0 || idx == 0 {
		return "", ""
	}
	scheme = origin[:idx]
	rest := origin[idx+3:]
	// 去掉 path 部分
	if i := strings.Index(rest, "/"); i >= 0 {
		rest = rest[:i]
	}
	return rest, scheme
}

func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		// 无 Origin 头:非跨域请求,跳过 CORS 处理
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}

		// 同源豁免:浏览器对同源 POST/JSON 也会发 Origin 头(防 fetch 滥用),
		// 但同源请求本就不需要 CORS 介入。这里对比 Origin 与 Host(含 X-Forwarded-*),
		// 一致就放行,避免自家人被自家 CORS 拦。
		if isSameOrigin(r) {
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
