package router

import (
	_ "embed"
	"net/http"

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

	// 安装相关路由(InstallGuard 已在 setup 模式放行;完成后由 controller 二次校验 404)
	r.HandleFunc("/setup", func(w http.ResponseWriter, r *http.Request) {
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
