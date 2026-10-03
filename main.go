package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/volcano-tts/tts-api/controller"
	"github.com/volcano-tts/tts-api/installer"
	"github.com/volcano-tts/tts-api/metrics"
	"github.com/volcano-tts/tts-api/middleware"
	"github.com/volcano-tts/tts-api/router"
	"github.com/volcano-tts/tts-api/setting"
	"github.com/volcano-tts/tts-api/telemetry"
)

// ttsDBPath 返回数据库/lock 所在路径;空时落到当前目录的 tts.db。
func ttsDBPath() string {
	if p := os.Getenv("TTS_DB_PATH"); p != "" {
		return p
	}
	return "tts.db"
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.SetPrefix("[TTS-Server] ")

	// 1) 加载引导环境变量(PORT / TTS_ADMIN_KEY / OPENAI_TTS_API_KEY 兜底)
	// 注: Auth.APIKeys 在 InitAllConfigs 里先读 env,后被 LoadRuntimeConfig 覆盖为 DB 值。
	setting.InitAllConfigs()
	metrics.Init()
	middleware.InitRateLimiter()
	// v0.3.0:根路径 /metrics 的内网白名单(未配置则根路径完全不注册)
	middleware.InitMetricsAllowList()

	// 2) 启动期关键步骤:打开/建库 → 检测 lock → 判定模式
	dbPath := ttsDBPath()
	if err := installer.EnsureDBDir(dbPath); err != nil {
		log.Fatalf("FATAL: cannot create db dir: %v", err)
	}
	st, res, err := installer.Detect(dbPath)
	if err != nil {
		log.Fatalf("FATAL: installer detect failed: %v", err)
	}
	if res.Corrupted {
		log.Printf("[main] 注意: 启动时检测到 db 损坏并已自愈回退(备份=%s)", res.BackupTo)
	}

	// 3) 注入 setup/admin 控制器需要的句柄(M1+M2)
	controller.SetSetupState(st, dbPath)
	controller.SetAdminStore(st)
	controller.SetMetricsTextWriter(func(w http.ResponseWriter) error {
		metrics.Meter.Handler().ServeHTTP(w, &http.Request{})
		return nil
	})

	// 4) M3: 从 store 加载运行时 TTS 配置(替代原来的 env-based InitTTSConfig)
	// 必须在 LogStartupSummary 之前,这样日志显示的是真实状态(API key 已从 DB 加载,不再读 env)
	//
	// 模式区分:
	//   - setup 模式 + 失败 = 正常(还没装), 警告即可
	//   - normal 模式 + 失败 = 致命(已装但配置坏), fail-fast 让 K8s/进程管理器拉起
	if st != nil {
		if err := setting.LoadRuntimeConfig(st); err != nil {
			mode := installer.GetMode()
			modeName := "setup"
			if mode == installer.ModeNormal {
				modeName = "normal"
			}
			metrics.ConfigLoadFailures.Inc(telemetry.Labels{"mode": modeName})
			if mode == installer.ModeNormal {
				log.Printf("[main][FATAL] TTS 运行时配置加载失败 (normal mode, 服务无法启动): %v", err)
				log.Fatalf("service cannot start in normal mode without valid config: %v", err)
			}
			log.Printf("[main][WARN] TTS 运行时配置加载失败 (setup mode, 需先 /setup): %v", err)
		} else {
			opts := setting.GetTTSOptions()
			log.Printf("[main] TTS 运行时配置已加载(api_key=***, speaker=%s, resource=%s, format=%s)",
				telemetry.MaskSpeaker(opts.Speaker), telemetry.MaskResourceID(opts.ResourceID), opts.Format)
		}
	}

	// 5) 启动摘要日志(此时 Auth.APIKeys 已是 DB 值,日志反映真实状态)
	setting.LogStartupSummary()

	// 5.1) v0.3.0 前置校验:normal 模式下必须有管理凭证。
	// 为什么 fail-fast 而不是警告:mware.RequireAdmin 在凭证为空时**拒绝**访问
	// (不再像 v0.3.0 之前那样放行)。若此处不拦住,服务会正常起来,
	// 但 /dashboard 与所有 /api/admin/* 全部 401 —— 相当于把自己锁在门外。
	// 宁可启动失败并打印明确原因,也不要起来一个进不去后台的实例。
	if installer.GetMode() == installer.ModeNormal && len(setting.GetAdminKeys()) == 0 {
		log.Printf("[main][FATAL] normal 模式未配置任何管理凭证:admin_key / auth_key / OPENAI_TTS_API_KEY 均为空。")
		log.Printf("[main][FATAL] 管理接口(含 /dashboard)将全部返回 401,服务拒绝启动。")
		log.Fatalf("no admin credential configured; set admin_key (or auth_key) before starting in normal mode")
	}
	if installer.GetMode() == installer.ModeNormal {
		log.Printf("[main] 管理凭证来源: %s", setting.GetAdminKeySource())
	}

	log.Printf("[main] 当前模式: %s (db=%s lock=%s)", res.Mode, dbPath, res.LockPath)

	controller.InitController()
	controller.SetStartTime(time.Now())

	r := router.Setup()

	server := &http.Server{
		Addr:         ":" + setting.Server.Port,
		Handler:      middleware.CORS(r),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if installer.GetMode() == installer.ModeSetup {
			log.Printf("Starting TTS Server in SETUP mode")
			log.Printf("Open browser to http://localhost:%s/setup to install", setting.Server.Port)
		} else {
			log.Printf("Starting ByteDance TTS to OpenAI API Adapter Server")
			log.Printf("Listening on port: %s", setting.Server.Port)
			log.Printf("OpenAI TTS endpoint: http://localhost:%s/v1/audio/speech", setting.Server.Port)
			log.Printf("Admin WebUI: http://localhost:%s/admin", setting.Server.Port)
		}
		log.Printf("Health check(匿名存活探针): http://localhost:%s/healthz", setting.Server.Port)
		if middleware.MetricsAllowListConfigured() {
			log.Printf("Metrics(内网白名单): http://localhost:%s/metrics", setting.Server.Port)
		} else {
			log.Printf("Metrics: 根路径 /metrics 未注册(未配置 METRICS_ALLOW_CIDR);请用鉴权版 /api/admin/metrics")
		}
		log.Printf("详细健康数据(鉴权): http://localhost:%s/api/admin/health", setting.Server.Port)

		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed to start: %v", err)
		}
	}()

	<-quit
	log.Println("Shutting down server...")

	// 关闭 db 连接(仅当 st 非 nil 时)
	if st != nil {
		_ = st.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("Server forced to shutdown: %v", err)
	} else {
		log.Println("Server exited gracefully")
	}
}
