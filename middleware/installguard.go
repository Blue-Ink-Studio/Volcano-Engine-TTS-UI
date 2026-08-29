package middleware

import (
	"log"
	"net/http"
	"strings"

	"github.com/volcano-tts/tts-api/installer"
)

// InstallGuard 拦截所有非 /setup 路由,在安装模式下返回 503。
// 设计:放行白名单路径前缀,其余一律 503 + 引导跳转。
//
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
			log.Printf("[installguard] 安装模式下拒绝非白名单请求 - 路径=%s 客户端=%s", path, GetClientIP(r))
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"not installed","code":"install_required","redirect":"/setup"}`))
		})
	}
}
