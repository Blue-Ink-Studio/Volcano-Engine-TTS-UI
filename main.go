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
	"github.com/volcano-tts/tts-api/metrics"
	"github.com/volcano-tts/tts-api/middleware"
	"github.com/volcano-tts/tts-api/router"
	"github.com/volcano-tts/tts-api/setting"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.SetPrefix("[TTS-Server] ")

	setting.InitAllConfigs()
	metrics.Init()
	middleware.InitRateLimiter()
	setting.CheckStaticFiles()
	controller.InitController()
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
		log.Printf("Metrics: http://localhost:%s/metrics", setting.Server.Port)
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
