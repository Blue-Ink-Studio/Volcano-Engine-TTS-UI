package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/volcano-tts/tts-api/common"
	"github.com/volcano-tts/tts-api/controller"
	"github.com/volcano-tts/tts-api/middleware"
	"github.com/volcano-tts/tts-api/router"
	"github.com/volcano-tts/tts-api/service"
	"github.com/volcano-tts/tts-api/setting"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.SetPrefix("[TTS-Server] ")

	middleware.InitAPIKeys()
	middleware.InitCORSConfig()
	middleware.InitRateLimiter()
	setting.CheckStaticFiles()
	service.InitStats()
	controller.InitController()

	setting.TTSConfigErr = setting.InitTTSConfig()
	if setting.TTSConfigErr != nil {
		log.Printf("警告：配置初始化失败: %v", setting.TTSConfigErr)
		log.Printf("服务将继续运行，但TTS功能不可用，请检查环境变量配置
	} else {
		log.Printf("閰嶇疆鍒濆鍖栨垚鍔?)
	}

	controller.SetStartTime(time.Now())

	r := router.Setup()

	port := os.Getenv("PORT")
	if port == "" {
		port = common.DefaultPort
	}

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      r,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("Starting ByteDance TTS to OpenAI API Adapter Server")
		log.Printf("Listening on port: %s", port)
		log.Printf("OpenAI TTS endpoint: http://localhost:%s/v1/audio/speech", port)
		log.Printf("Health check: http://localhost:%s/health", port)
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
