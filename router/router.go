package router

import (
	"net/http"

	"github.com/gorilla/mux"
	"github.com/volcano-tts/tts-api/controller"
	"github.com/volcano-tts/tts-api/middleware"
)

func Setup() *mux.Router {
	r := mux.NewRouter()

	r.Use(middleware.SecurityHeaders)
	r.Use(middleware.RateLimit)
	r.Use(middleware.ConcurrencyLimit)
	r.Use(middleware.Logger)

	r.HandleFunc("/v1/audio/speech", controller.OpenaiTTSHandler).Methods("POST", "OPTIONS")
	r.HandleFunc("/health", controller.HealthHandler).Methods("GET")
	r.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "health.html")
	}).Methods("GET")
	r.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard", http.StatusFound)
	}).Methods("GET")

	return r
}
