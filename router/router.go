package router

import (
	_ "embed"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	"github.com/volcano-tts/tts-api/controller"
	"github.com/volcano-tts/tts-api/installer"
	"github.com/volcano-tts/tts-api/metrics"
	"github.com/volcano-tts/tts-api/middleware"
)

//go:embed health.html
var dashboardHTML []byte

//go:embed setup.html
var setupHTML []byte

// Setup 返回主路由。
// 中间件顺序(由外向内):
//   SecurityHeaders → InstallGuard → RateLimit → ConcurrencyLimit → Logger → handler
// 关键: InstallGuard 必须在 RateLimit 之前,避免安装模式被限流计数污染。
func Setup() *mux.Router {
	r := mux.NewRouter()

	r.Use(middleware.SecurityHeaders)
	r.Use(middleware.InstallGuard(installer.GetMode))
	r.Use(middleware.RateLimitWithMetrics)
	r.Use(middleware.ConcurrencyLimitWithMetrics)
	r.Use(middleware.Logger)

	// mux 的 NotFoundHandler 不会走 r.Use() 中间件链,
	// 所以 InstallGuard 的内容协商在 404 路径上不生效。
	// 手动设一个:安装模式 + 浏览器访问任意未注册路径 → 302 跳 /setup。
	r.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if installer.GetMode() == installer.ModeSetup && acceptsHTML(r.Header.Get("Accept")) {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})

	// 安装相关路由(InstallGuard 已在 setup 模式放行;完成后由 controller 二次校验 404)
	// /setup 页面本身:装完后必须不可用,否则用户敲 /setup 还会看到安装表单,容易误以为要重装。
	// 装后跳 /admin(M2 之后才有;目前会 404,这是预期,比继续显示表单好)。
	r.HandleFunc("/setup", func(w http.ResponseWriter, r *http.Request) {
		if installer.GetMode() == installer.ModeNormal {
			http.Redirect(w, r, "/admin", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(setupHTML)
	}).Methods("GET")
	r.HandleFunc("/api/setup/status", controller.SetupStatusHandler).Methods("GET")
	r.HandleFunc("/api/setup/prefill", controller.SetupPrefillHandler).Methods("GET")
	r.HandleFunc("/api/setup", controller.SetupSubmitHandler).Methods("POST")

	// 业务路由
	r.HandleFunc("/v1/audio/speech", controller.OpenaiTTSHandler).Methods("POST", "OPTIONS")
	r.HandleFunc("/health", controller.HealthHandler).Methods("GET")
	r.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(dashboardHTML)
	}).Methods("GET")
	r.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// 安装模式下,根路径跳 /setup(给运维一个明显入口)
		if installer.GetMode() == installer.ModeSetup {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		http.Redirect(w, r, "/dashboard", http.StatusFound)
	}).Methods("GET")

	// /metrics 不做鉴权(对齐 /health 策略),但仍然走 RateLimit / ConcurrencyLimit。
	// Prometheus 抓取不带 Origin,因此经过 CORS 中间件时会直接 pass-through。
	r.Handle("/metrics", metrics.Meter.Handler()).Methods("GET")

	return r
}

// acceptsHTML 在 router 包内复刻一份,middleware 包的版本未导出。
// 用途:NotFoundHandler 判断浏览器 Accept。
// 与 middleware.acceptsHTML 行为一致(简单实现,严格匹配 text/html 或 text/*)。
func acceptsHTML(accept string) bool {
	if accept == "" {
		return false
	}
	for _, part := range strings.Split(accept, ",") {
		mt := strings.TrimSpace(part)
		if mt == "" {
			continue
		}
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
