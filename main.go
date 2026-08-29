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
	if st != nil {
		if err := setting.LoadRuntimeConfig(st); err != nil {
			log.Printf("[main] TTS 运行时配置加载失败:%v", err)
		} else {
			log.Printf("[main] TTS 运行时配置已加载(api_key=***, speaker=%s, resource=%s, format=%s)",
				setting.TTSOptions.Speaker, setting.TTSOptions.ResourceID, setting.TTSOptions.Format)
		}
	}

	// 5) 启动摘要日志(此时 Auth.APIKeys 已是 DB 值,日志反映真实状态)
	setting.LogStartupSummary()
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
		log.Printf("Health check: http://localhost:%s/health", setting.Server.Port)
		log.Printf("Metrics: http://localhost:%s/metrics", setting.Server.Port)

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
