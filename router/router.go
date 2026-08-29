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

//go:embed admin.html
var adminHTML []byte

// Setup 返回主路由。
// 中间件顺序(由外向内):
//   SecurityHeaders → InstallGuard → RateLimit → ConcurrencyLimit → Logger → handler
// 关键: InstallGuard 必须在 RateLimit 之前,避免安装模式被限流计数污染。
//
// /admin 和 /api/admin/* 都加 RequireAdmin;InstallGuard 不预先放行(让安装模式下
// 自动 302 跳 /setup,体验一致)。
func Setup() *mux.Router {
	r := mux.NewRouter()

	r.Use(middleware.SecurityHeaders)
	r.Use(middleware.InstallGuard(installer.GetMode))
	r.Use(middleware.RateLimitWithMetrics)
	r.Use(middleware.ConcurrencyLimitWithMetrics)
	r.Use(middleware.Logger)

	// mux 路由未匹配时 NotFoundHandler 单独处理(不走 r.Use() 中间件链);
	// 安装模式 + 浏览器访问任意未注册路径 → 302 跳 /setup。
	r.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if installer.GetMode() == installer.ModeSetup && acceptsHTML(req.Header.Get("Accept")) {
			http.Redirect(w, req, "/setup", http.StatusFound)
			return
		}
		http.NotFound(w, req)
	})

	// 安装相关路由(InstallGuard 已在 setup 模式放行;完成后由 controller 二次校验 404)
	// /setup 页面本身:装完后必须不可用,否则用户敲 /setup 还会看到安装表单,容易误以为要重装。
	r.HandleFunc("/setup", func(w http.ResponseWriter, req *http.Request) {
		if installer.GetMode() == installer.ModeNormal {
			http.Redirect(w, req, "/admin", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(setupHTML)
	}).Methods("GET")
	r.HandleFunc("/api/setup/status", controller.SetupStatusHandler).Methods("GET")
	r.HandleFunc("/api/setup/prefill", controller.SetupPrefillHandler).Methods("GET")
	r.HandleFunc("/api/setup", controller.SetupSubmitHandler).Methods("POST")

	// /admin 管理后台(M2);HTML 本身公开,鉴权由前端 JS 拦截
	// (sessionStorage 没 key 就显示登录页;有 key 调 /api/admin/overview 触发 401 跳登录)
	// API 端点(/api/admin/* /api/voices*)才需要 RequireAdmin。
	r.HandleFunc("/admin", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(adminHTML)
	}).Methods("GET")

	// /api/admin/overview (鉴权)
	r.Handle("/api/admin/overview", middleware.RequireAdmin(http.HandlerFunc(controller.AdminOverviewHandler))).Methods("GET")
	// /api/admin/metrics (鉴权);返 Prometheus 文本
	r.Handle("/api/admin/metrics", middleware.RequireAdmin(http.HandlerFunc(controller.AdminMetricsHandler))).Methods("GET")

	// /api/voices 音色 CRUD (鉴权)
	r.Handle("/api/voices", middleware.RequireAdmin(http.HandlerFunc(controller.AdminVoicesListHandler))).Methods("GET")
	r.Handle("/api/voices", middleware.RequireAdmin(http.HandlerFunc(controller.AdminVoiceCreateHandler))).Methods("POST")
	r.Handle("/api/voices/{name}", middleware.RequireAdmin(http.HandlerFunc(controller.AdminVoiceDeleteHandler))).Methods("DELETE")
	r.Handle("/api/voices/{name}/toggle", middleware.RequireAdmin(http.HandlerFunc(controller.AdminVoiceToggleHandler))).Methods("PATCH")

	// 业务路由
	r.HandleFunc("/v1/audio/speech", controller.OpenaiTTSHandler).Methods("POST", "OPTIONS")
	r.HandleFunc("/health", controller.HealthHandler).Methods("GET")
	r.HandleFunc("/dashboard", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(dashboardHTML)
	}).Methods("GET")
	r.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		// 安装模式下,根路径跳 /setup
		if installer.GetMode() == installer.ModeSetup {
			http.Redirect(w, req, "/setup", http.StatusFound)
			return
		}
		// 正常模式:跳 /admin(M2 之后优先于 /dashboard)
		http.Redirect(w, req, "/admin", http.StatusFound)
	}).Methods("GET")

	// /metrics 不做鉴权(对齐 /health 策略),但仍然走 RateLimit / ConcurrencyLimit。
	// Prometheus 抓取不带 Origin,因此经过 CORS 中间件时会直接 pass-through。
	r.Handle("/metrics", metrics.Meter.Handler()).Methods("GET")

	return r
}

// acceptsHTML 在 router 包内复刻,middleware 包的版本未导出。
// 用途:NotFoundHandler 判断浏览器 Accept。
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
