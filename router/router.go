package router

import (
	"net/http"

	"github.com/gorilla/mux"
	"github.com/volcano-tts/tts-api/controller"
	"github.com/volcano-tts/tts-api/metrics"
	"github.com/volcano-tts/tts-api/middleware"
)

func Setup() *mux.Router {
	r := mux.NewRouter()

	r.Use(middleware.SecurityHeaders)
	r.Use(middleware.RateLimitWithMetrics)
	r.Use(middleware.ConcurrencyLimitWithMetrics)
	r.Use(middleware.Logger)

	r.HandleFunc("/v1/audio/speech", controller.OpenaiTTSHandler).Methods("POST", "OPTIONS")
	r.HandleFunc("/health", controller.HealthHandler).Methods("GET")
	r.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "health.html")
	}).Methods("GET")
	r.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard", http.StatusFound)
	}).Methods("GET")

	// /metrics 不做鉴权(对齐 /health 策略),但仍然走 RateLimit / ConcurrencyLimit。
	// Prometheus 抓取不带 Origin,因此经过 CORS 中间件时会直接 pass-through。
	r.Handle("/metrics", metrics.Meter.Handler()).Methods("GET")

	return r
}
