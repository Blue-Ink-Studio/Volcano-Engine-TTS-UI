package controller

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/volcano-tts/tts-api/adapter/volcano"
	"github.com/volcano-tts/tts-api/common"
	"github.com/volcano-tts/tts-api/dto"
	"github.com/volcano-tts/tts-api/middleware"
	"github.com/volcano-tts/tts-api/service"
	"github.com/volcano-tts/tts-api/setting"
)

var volcanoClient *volcano.HTTPClient

func InitController() {
	volcanoClient = volcano.NewHTTPClient()
}

func OpenaiTTSHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !middleware.ValidateAPIKey(r) {
		middleware.SendJSONError(w, http.StatusUnauthorized, "Invalid API key provided.", "invalid_request_error", "invalid_api_key")
		return
	}

	if setting.TTSConfigErr != nil {
		middleware.SendJSONError(w, http.StatusServiceUnavailable, "TTS service configuration error. Please check environment variables and restart the service.", "configuration_error", "service_unavailable")
		return
	}

	select {
	case middleware.ConcurrencySem <- struct{}{}:
		defer func() { <-middleware.ConcurrencySem }()
	default:
		log.Printf("警告: 已达到最大并发请求数限制，拒绝请求 - 客户端IP: %s", middleware.GetClientIP(r))
		middleware.SendJSONError(w, http.StatusServiceUnavailable, "Server is busy, maximum concurrent requests reached. Please try again later.", "concurrency_limit_error", "max_concurrent_requests")
		return
	}

	clientIP := middleware.GetClientIP(r)
	if !middleware.GlobalRateLimiter.Allow(clientIP) {
		log.Printf("警告: 已超过IP速率限制，拒绝请求 - 客户端IP: %s", clientIP)
		middleware.SendJSONError(w, http.StatusTooManyRequests, "Rate limit exceeded. Please try again later.", "rate_limit_error", "rate_limit_exceeded")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, common.MaxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			return
		}
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	var req dto.OpenAITTSRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if req.Input == "" {
		http.Error(w, "Input text is required", http.StatusBadRequest)
		return
	}

	if len(req.Input) > common.MaxTextLength {
		http.Error(w, fmt.Sprintf("Input text too long (max %d characters)", common.MaxTextLength), http.StatusBadRequest)
		return
	}

	speed := req.Speed
	if speed <= 0 {
		speed = common.DefaultSpeed
	}
	if speed < common.MinSpeed {
		speed = common.MinSpeed
	}
	if speed > common.MaxSpeed {
		speed = common.MaxSpeed
	}

	ttsStart := time.Now()
	result, err := volcano.Synthesis(&setting.TTSConfig, volcanoClient, req.Input, speed)
	duration := time.Since(ttsStart)

	if err != nil {
		service.GlobalStats.AddRequest(false, duration, err.Error())
		http.Error(w, "TTS synthesis failed", http.StatusInternalServerError)
		return
	}

	service.GlobalStats.AddRequest(true, duration, "")

	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(result.AudioData)))
	w.Header().Set("X-Request-Id", result.ReqID)
	w.WriteHeader(http.StatusOK)
	w.Write(result.AudioData)
}

func HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if setting.TTSConfigErr != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	totalRequests, successfulRequests, failedRequests, totalResponseTime, recentResponseTimes, lastErrors := service.GlobalStats.GetSnapshot()

	var errorRate float64
	if totalRequests > 0 {
		errorRate = float64(failedRequests) / float64(totalRequests) * 100
	}

	var avgResponseTime float64
	if totalRequests > 0 {
		avgResponseTime = totalResponseTime.Seconds() * 1000 / float64(totalRequests)
	}

	envCheckStatus := setting.CheckEnvironmentVariables()
	allEnvVarsSet := envCheckStatus["all_required_vars_set"].(bool)

	status := "ok"
	if !allEnvVarsSet {
		status = "configuration_error"
	}

	response := dto.HealthResponse{
		Status:    status,
		Service:   "ByteDance TTS to OpenAI API Adapter",
		Version:   "2.0.0 (v3 API)",
		Uptime:    fmt.Sprintf("%.0f seconds", time.Since(startTime).Seconds()),
		StartTime: startTime.Format(time.RFC3339),
		Memory:    service.GetMemoryInfo(),
		APIStats: dto.APIStatsResponse{
			TotalRequests:         int(totalRequests),
			SuccessfulRequests:    successfulRequests,
			FailedRequests:        failedRequests,
			ErrorRatePercent:      fmt.Sprintf("%.2f", errorRate),
			AvgResponseTimeMs:     fmt.Sprintf("%.2f", avgResponseTime),
			RecentResponseTimesMs: recentResponseTimes,
		},
		Errors: dto.ErrorResponse{
			RecentErrorsCount: len(lastErrors),
		},
		ConfigStatus: dto.ConfigStatusResponse{
			AllRequiredVarsSet: allEnvVarsSet,
			ConfigError:        setting.TTSConfigErr != nil,
		},
	}

	json.NewEncoder(w).Encode(response)
}

var startTime time.Time

func SetStartTime(t time.Time) {
	startTime = t
}
