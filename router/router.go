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

//go:embed admin-login.html
var adminLoginHTML []byte

//go:embed admin-voices.html
var adminVoicesHTML []byte

//go:embed admin-settings.html
var adminSettingsHTML []byte

//go:embed admin.css
var adminCSS []byte

//go:embed admin-shell.js
var adminShellJS []byte

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
			http.Redirect(w, req, "/dashboard", http.StatusFound) // v0.3.0: 管理入口迁至 /dashboard
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(setupHTML)
	}).Methods("GET")
	// v0.3.0:安装状态不再匿名暴露(原本 normal 模式下仍返回 {installed, mode})
	r.Handle("/api/setup/status", middleware.RequireAdmin(http.HandlerFunc(controller.SetupStatusHandler))).Methods("GET")
	r.HandleFunc("/api/setup/prefill", controller.SetupPrefillHandler).Methods("GET")
	r.HandleFunc("/api/setup", controller.SetupSubmitHandler).Methods("POST")

	// ===== v0.3.0 阶段 3:管理面板入口从 /admin 迁至 /dashboard =====
	//
	// 页面本身走 htmlAwareAdmin:浏览器请求(Accept: text/html)返回页面外壳,
	// 前端依据 sessionStorage 里的凭证显示登录视图;非 HTML 请求(脚本/抓取)强制鉴权。
	// 原因:SPA 的登录态存在 sessionStorage、不随请求发送,服务端无从判断是否已登录,
	// 一律 401 会把"打开 /dashboard 看到登录页"变成"打开 /dashboard 直接报错"。
	// 页面外壳不含任何数据,数据全部来自鉴权后的 /api/admin/* 接口。
	// /dashboard 是管理看板(admin.html,与 /dashboard/voices、/dashboard/settings 同属一套 SPA)。
	// 注意:拆分前的 /dashboard 曾是"服务状态预览页"(health.html),v0.3.0 起
	// 管理入口统一到 /dashboard,该预览页迁到 /dashboard/status 作为"详细状态"视图。
	r.Handle("/dashboard", htmlAwareAdmin(serveAdmin(adminHTML))).Methods("GET")
	r.Handle("/dashboard/voices", htmlAwareAdmin(serveAdmin(adminVoicesHTML))).Methods("GET")
	r.Handle("/dashboard/settings", htmlAwareAdmin(serveAdmin(adminSettingsHTML))).Methods("GET")
	r.Handle("/dashboard/status", htmlAwareAdmin(serveAdmin(dashboardHTML))).Methods("GET")
	// 登录页无数据,保持公开(否则无法登录)
	r.HandleFunc("/dashboard/login", serveAdmin(adminLoginHTML)).Methods("GET")

	// 静态资源:迁到 /dashboard/ 下,同时**保留 /admin/ 旧路径为别名**,
	// 这样过渡期内新旧页面都能正确加载样式与脚本,不会出现"页面能开但丢样式"。
	serveCSS := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write(adminCSS)
	}
	serveShellJS := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		_, _ = w.Write(adminShellJS)
	}
	r.HandleFunc("/dashboard/admin.css", serveCSS).Methods("GET")
	r.HandleFunc("/dashboard/admin-shell.js", serveShellJS).Methods("GET")
	r.HandleFunc("/admin/admin.css", serveCSS).Methods("GET")
	r.HandleFunc("/admin/admin-shell.js", serveShellJS).Methods("GET")

	// 旧入口永久重定向(页面用 301 是正确的;API 路径**不用**重定向,见下方别名说明)
	r.HandleFunc("/admin", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/dashboard", http.StatusMovedPermanently)
	}).Methods("GET")
	r.HandleFunc("/admin/login", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/dashboard/login", http.StatusMovedPermanently)
	}).Methods("GET")
	r.HandleFunc("/admin/voices", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/dashboard/voices", http.StatusMovedPermanently)
	}).Methods("GET")
	r.HandleFunc("/admin/settings", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/dashboard/settings", http.StatusMovedPermanently)
	}).Methods("GET")

	// /api/admin/overview (鉴权)
	r.Handle("/api/admin/overview", middleware.RequireAdmin(http.HandlerFunc(controller.AdminOverviewHandler))).Methods("GET")
	// /api/admin/metrics (鉴权);返 Prometheus 文本
	r.Handle("/api/admin/metrics", middleware.RequireAdmin(http.HandlerFunc(controller.AdminMetricsHandler))).Methods("GET")
	// v0.3.0:详细健康数据的鉴权版(面板与运维用)。
	// 与根路径 /health 的区别:这里始终注册且强制鉴权;根路径 /health 默认 404。
	r.Handle("/api/admin/health", middleware.RequireAdmin(http.HandlerFunc(controller.HealthHandler))).Methods("GET")

	// /api/voices 音色 CRUD (鉴权)
	//
	// ⚠️ v0.3.0 阶段 3 路径迁移:/api/admin/voices 是**正式路径**,
	// /api/voices 保留为**过渡期别名**(同一个 handler,不做重定向)。
	//
	// 为什么用别名而不是 301/308:
	//   - 301 会把 POST/PUT/PATCH/DELETE 降级成 GET(RFC 7231),且被浏览器永久缓存,
	//     客户端根本察觉不到路径变化,"过渡期"名存实亡
	//   - 308 能保留方法,但对非浏览器脚本仍是行为变化,且多一次往返
	//   - 别名让新旧路径都能直接用,零往返、零破坏,老脚本完全无感
	// 旧别名计划在后续版本移除。
	r.Handle("/api/admin/voices", middleware.RequireAdmin(http.HandlerFunc(controller.AdminVoicesListHandler))).Methods("GET")
	r.Handle("/api/admin/voices", middleware.RequireAdmin(http.HandlerFunc(controller.AdminVoiceCreateHandler))).Methods("POST")
	r.Handle("/api/admin/voices/{name}", middleware.RequireAdmin(http.HandlerFunc(controller.AdminVoiceDeleteHandler))).Methods("DELETE")
	r.Handle("/api/admin/voices/{name}/toggle", middleware.RequireAdmin(http.HandlerFunc(controller.AdminVoiceToggleHandler))).Methods("PATCH")

	r.Handle("/api/voices", middleware.RequireAdmin(http.HandlerFunc(controller.AdminVoicesListHandler))).Methods("GET")
	r.Handle("/api/voices", middleware.RequireAdmin(http.HandlerFunc(controller.AdminVoiceCreateHandler))).Methods("POST")
	r.Handle("/api/voices/{name}", middleware.RequireAdmin(http.HandlerFunc(controller.AdminVoiceDeleteHandler))).Methods("DELETE")
	r.Handle("/api/voices/{name}/toggle", middleware.RequireAdmin(http.HandlerFunc(controller.AdminVoiceToggleHandler))).Methods("PATCH")

	// /api/settings 全局设置 (鉴权) — M3
	// 同样:/api/admin/settings 为正式路径,/api/settings 为过渡期别名。
	// 注意更长的 /api/admin/settings/admin-key 必须**先注册**(mux 按注册顺序取首个匹配)。
	r.Handle("/api/admin/settings/admin-key", middleware.RequireAdmin(http.HandlerFunc(controller.SettingsAdminKeyHandler))).Methods("PUT")
	r.Handle("/api/admin/settings", middleware.RequireAdmin(http.HandlerFunc(controller.SettingsGetHandler))).Methods("GET")
	r.Handle("/api/admin/settings", middleware.RequireAdmin(http.HandlerFunc(controller.SettingsUpdateHandler))).Methods("PUT")
	r.Handle("/api/admin/settings/api-key", middleware.RequireAdmin(http.HandlerFunc(controller.SettingsAPIKeyHandler))).Methods("PUT")
	r.Handle("/api/admin/settings/auth-key", middleware.RequireAdmin(http.HandlerFunc(controller.SettingsAuthKeyHandler))).Methods("PUT")

	r.Handle("/api/settings", middleware.RequireAdmin(http.HandlerFunc(controller.SettingsGetHandler))).Methods("GET")
	r.Handle("/api/settings", middleware.RequireAdmin(http.HandlerFunc(controller.SettingsUpdateHandler))).Methods("PUT")
	r.Handle("/api/settings/api-key", middleware.RequireAdmin(http.HandlerFunc(controller.SettingsAPIKeyHandler))).Methods("PUT")
	r.Handle("/api/settings/auth-key", middleware.RequireAdmin(http.HandlerFunc(controller.SettingsAuthKeyHandler))).Methods("PUT")
	r.Handle("/api/settings/admin-key", middleware.RequireAdmin(http.HandlerFunc(controller.SettingsAdminKeyHandler))).Methods("PUT")
	r.Handle("/api/settings/cors", middleware.RequireAdmin(http.HandlerFunc(controller.SettingsCORSHandler))).Methods("PUT")

	// 业务路由
	r.HandleFunc("/v1/audio/speech", controller.OpenaiTTSHandler).Methods("POST", "OPTIONS")

	// v0.3.0 端点收口:
	//   /healthz        匿名存活探针,只回 "ok"(K8s probe / Docker HEALTHCHECK 用)
	//   /health         详细健康数据,**鉴权**(原本匿名,会泄漏版本/内存/配置错误文本)
	//   /dashboard      管理看板(**鉴权**,见上方阶段 3 的注册处)
	//   /api/admin/health 鉴权版详细健康数据(面板拉取用,风格与 /api/admin/* 一致)
	r.HandleFunc("/healthz", controller.HealthzHandler).Methods("GET")
	r.Handle("/health", middleware.RequireAdmin(http.HandlerFunc(controller.HealthHandler))).Methods("GET")
	r.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		// 安装模式下,根路径跳 /setup
		if installer.GetMode() == installer.ModeSetup {
			http.Redirect(w, req, "/setup", http.StatusFound)
			return
		}
		// 正常模式:跳 /dashboard(v0.3.0 管理入口)
		http.Redirect(w, req, "/dashboard", http.StatusFound)
	}).Methods("GET")

	// 根路径 /metrics 与 /health 的机器访问:
	//   - 配置了 METRICS_ALLOW_CIDR → 注册 /metrics,包裹内网白名单(非白名单返回 404)
	//   - 未配置 → 根路径 /metrics **完全不注册**(访问 404),避免默认对外暴露
	// /health 已在上面按鉴权注册,不再提供匿名版本。
	if middleware.MetricsAllowListConfigured() {
		r.Handle("/metrics", middleware.MetricsIPAllowList(metrics.Meter.Handler())).Methods("GET")
	}

	return r
}

// htmlAwareAdmin 给**浏览器直接访问的管理页面**套鉴权,但对 HTML 请求做内容协商:
//
//   - Accept 含 text/html(地址栏打开、点链接)→ 照常返回页面外壳。
//     此时不做 401:因为前端登录态存在 sessionStorage,**不会随请求发送**,
//     服务端无从判断是否已登录;强行 401 会让"打开 /dashboard 看到登录页"
//     这一现有体验变成"打开 /dashboard 直接报错"。
//     页面外壳本身不含任何数据,数据全部来自鉴权后的 /api/admin/* 接口。
//   - 其它(脚本、curl、抓取工具)→ 强制 RequireAdmin,无凭证 401。
//
// 这样既堵住了"匿名抓取健康数据",又不破坏 SPA 的登录流程。
func htmlAwareAdmin(html http.Handler) http.Handler {
	guarded := middleware.RequireAdmin(html)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if acceptsHTML(r.Header.Get("Accept")) {
			html.ServeHTTP(w, r)
			return
		}
		guarded.ServeHTTP(w, r)
	})
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

// serveAdmin 返回一个把 embed 的 HTML bytes 以 text/html 写出的 handler。
// 抽出来只为让 /admin/voices 等多条路由的注册保持单行,避免重复 4 段。
func serveAdmin(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(body)
	}
}
