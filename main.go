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
	"github.com/volcano-tts/tts-api/middleware"
	"github.com/volcano-tts/tts-api/router"
	"github.com/volcano-tts/tts-api/service"
	"github.com/volcano-tts/tts-api/setting"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.SetPrefix("[TTS-Server] ")

	// 所有环境变量读取在 setting 包内集中完成,业务模块只读全局 Config。
	setting.InitAllConfigs()

	// 兼容旧调用顺序：rate limiter / 静态文件 / stats / controller 的初始化保持独立。
	middleware.InitRateLimiter()
	setting.CheckStaticFiles()
	service.InitStats()
	controller.InitController()

	// 启动期一次性打印所有 Config 状态,便于运维核对。
	// （必填项缺失的明确警告由 LogStartupSummary 自身负责,避免重复打印。）
	setting.LogStartupSummary()

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
		log.Printf("Starting ByteDance TTS to OpenAI API Adapter Server")
		log.Printf("Listening on port: %s", setting.Server.Port)
		log.Printf("OpenAI TTS endpoint: http://localhost:%s/v1/audio/speech", setting.Server.Port)
		log.Printf("Health check: http://localhost:%s/health", setting.Server.Port)
		log.Printf("Using ByteDance v3 API")

		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed to start: %v", err)
		}
	}()

	<-quit
	log.Println("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("Server forced to shutdown: %v", err)
	} else {
		log.Println("Server exited gracefully")
	}
}
