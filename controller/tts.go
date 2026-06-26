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

// truncateForLog 用于在日志中安全地展示请求内容（截断避免日志爆炸、控制不可打印字符）
func truncateForLog(b []byte, max int) string {
	if len(b) > max {
		return string(b[:max]) + fmt.Sprintf("...(truncated, total %d bytes)", len(b))
	}
	return string(b)
}

func OpenaiTTSHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		log.Printf("警告: 错误的方法 - 方法=%s 期望=POST 路径=%s 客户端=%s",
			r.Method, r.URL.Path, middleware.GetClientIP(r))
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !middleware.ValidateAPIKey(r) {
		log.Printf("警告: API Key 鉴权失败 - 路径=%s 客户端=%s 远端=%s",
			r.URL.Path, middleware.GetClientIP(r), r.RemoteAddr)
		middleware.SendJSONError(w, http.StatusUnauthorized, "Invalid API key provided.", "invalid_request_error", "invalid_api_key")
		return
	}

	if setting.TTSConfigErr != nil {
		log.Printf("警告: TTS配置未就绪，拒绝请求 - 错误=%v 路径=%s 客户端=%s",
			setting.TTSConfigErr, r.URL.Path, middleware.GetClientIP(r))
		middleware.SendJSONError(w, http.StatusServiceUnavailable, "TTS service configuration error. Please check environment variables and restart the service.", "configuration_error", "service_unavailable")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, common.MaxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			log.Printf("警告: 请求体过大 - 路径=%s 客户端=%s 限制=%d字节",
				r.URL.Path, middleware.GetClientIP(r), common.MaxRequestBodySize)
			http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		log.Printf("警告: 读取请求体失败 - 路径=%s 客户端=%s 错误=%v",
			r.URL.Path, middleware.GetClientIP(r), err)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	var req dto.OpenAITTSRequest
	if err := json.Unmarshal(body, &req); err != nil {
		log.Printf("警告: JSON 解析失败 - 路径=%s 客户端=%s 错误=%v body前200字节=%q",
			r.URL.Path, middleware.GetClientIP(r), err, truncateForLog(body, 200))
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if req.Model != "" {
		if len(req.Model) > common.MaxModelNameLength {
			log.Printf("警告: Model 名过长 - 路径=%s 客户端=%s 长度=%d 限制=%d",
				r.URL.Path, middleware.GetClientIP(r), len(req.Model), common.MaxModelNameLength)
			http.Error(w, fmt.Sprintf("Model name too long (max %d characters)", common.MaxModelNameLength), http.StatusBadRequest)
			return
		}
		if strings.ContainsAny(req.Model, "\x00\n\r\t") {
			log.Printf("警告: Model 名含非法字符 - 路径=%s 客户端=%s model前50字节=%q",
				r.URL.Path, middleware.GetClientIP(r), truncateForLog([]byte(req.Model), 50))
			http.Error(w, "Model name contains invalid characters", http.StatusBadRequest)
			return
		}
	}

	if req.Input == "" {
		log.Printf("警告: input 字段为空 - 路径=%s 客户端=%s", r.URL.Path, middleware.GetClientIP(r))
		http.Error(w, "Input text is required", http.StatusBadRequest)
		return
	}

	if len(req.Input) > common.MaxTextLength {
		log.Printf("警告: input 文本过长 - 路径=%s 客户端=%s 长度=%d 限制=%d",
			r.URL.Path, middleware.GetClientIP(r), len(req.Input), common.MaxTextLength)
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
	result, err := volcano.Synthesis(&setting.TTSConfig, volcanoClient, req.Input, speed, req.Voice)
	duration := time.Since(ttsStart)

	if err != nil {
		service.GlobalStats.AddRequest(false, duration, err.Error())
		log.Printf("警告: TTS 合成失败 - 路径=%s 客户端=%s 文本长度=%d 耗时=%v 错误=%v",
			r.URL.Path, middleware.GetClientIP(r), len(req.Input), duration, err)
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
