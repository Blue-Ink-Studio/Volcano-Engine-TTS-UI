package middleware

import (
	"log"
	"net/http"
	"strings"

	"github.com/volcano-tts/tts-api/installer"
)

// InstallGuard 拦截所有非白名单路由,在安装模式下按 Accept 头做内容协商:
//
//   - text/html 类(浏览器) → 302 重定向到 /setup
//   - 其它(API 客户端、curl 等) → 503 + JSON
//
// 设计:放行白名单路径前缀,其余一律拦截。
// 中间件顺序:必须装在 RateLimit / ConcurrencyLimit / Logger 之前,
// 避免安装模式下被限流计数污染(参考 M1 风险点 #2)。
func InstallGuard(currentMode func() installer.Mode, allowPrefixes ...string) func(http.Handler) http.Handler {
	defaults := []string{
		"/setup",          // 安装引导页
		"/api/setup",      // 安装相关 API
		"/health",         // 部署探针要能识别未安装状态
		"/metrics",        // Prometheus 拉取
		"/static/",        // 引导页静态资源(留口子)
	}
	allow := append(defaults, allowPrefixes...)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if currentMode() != installer.ModeSetup {
				next.ServeHTTP(w, r)
				return
			}
			// 安装模式:仅放行白名单
			path := r.URL.Path
			for _, p := range allow {
				if strings.HasPrefix(path, p) {
					next.ServeHTTP(w, r)
					return
				}
			}
			log.Printf("[installguard] 安装模式下拒绝非白名单请求 - 路径=%s 客户端=%s accept=%q",
				path, GetClientIP(r), r.Header.Get("Accept"))
			// 内容协商:浏览器自动跳 /setup,API 客户端拿 JSON。
			// / 不在白名单里,所以这里同时覆盖"敲域名根路径"和"敲其他路径"两种场景。
			if acceptsHTML(r.Header.Get("Accept")) {
				w.Header().Set("Location", "/setup")
				w.WriteHeader(http.StatusFound) // 302
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"not installed","code":"install_required","redirect":"/setup"}`))
		})
	}
}

// acceptsHTML 判断客户端是否接受 HTML 响应。
// 严格匹配:Accept 必须显式包含 text/html 或 text/*,避免通配 */*(curl/API 默认)
// 走 302 路径影响 API 行为。
func acceptsHTML(accept string) bool {
	if accept == "" {
		return false
	}
	for _, part := range strings.Split(accept, ",") {
		mt := strings.TrimSpace(part)
		if mt == "" {
			continue
		}
		// 去掉 q= 等参数
		if idx := strings.Index(mt, ";"); idx >= 0 {
			mt = strings.TrimSpace(mt[:idx])
		}
		mt = strings.ToLower(mt)
		if mt == "text/html" || mt == "text/*" {
			return true
		}
	}
	return false
}
