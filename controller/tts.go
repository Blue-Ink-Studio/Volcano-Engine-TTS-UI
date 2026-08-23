package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/volcano-tts/tts-api/adapter/volcano"
	"github.com/volcano-tts/tts-api/common"
	"github.com/volcano-tts/tts-api/dto"
	"github.com/volcano-tts/tts-api/metrics"
	"github.com/volcano-tts/tts-api/middleware"
	"github.com/volcano-tts/tts-api/setting"
	"github.com/volcano-tts/tts-api/telemetry"
)

var (
	volcanoClient *volcano.HTTPClient
	adapterRec    volcano.MetricsRecorder = metrics.AdapterRecorder{}
)

func InitController() {
	volcanoClient = volcano.NewHTTPClient()
}

func truncateForLog(b []byte, max int) string {
	if len(b) > max {
		return string(b[:max]) + fmt.Sprintf("...(truncated, total %d bytes)", len(b))
	}
	return string(b)
}

// resolveClientFormat 把 OpenAI 风格的 response_format 映射为最终输出格式;
// 不识别或未指定时回退到 setting.TTSOptions.Format。
func resolveClientFormat(reqFmt string) string {
	switch strings.ToLower(reqFmt) {
	case "mp3", "wav", "opus", "pcm", "aac", "flac":
		if reqFmt == "opus" {
			return "ogg_opus"
		}
		return strings.ToLower(reqFmt)
	}
	if reqFmt == "" {
		return setting.TTSOptions.Format
	}
	return setting.TTSOptions.Format
}

// OpenaiTTSHandler 是 /v1/audio/speech 的入口。
func OpenaiTTSHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if r.Method != http.MethodPost {
		log.Printf("警告: 错误的方法 - 方法=%s 期望=POST 路径=%s 客户端=%s",
			r.Method, r.URL.Path, middleware.GetClientIP(r))
		metrics.RequestTotal.Inc(telemetry.Labels{"status": "method_not_allowed", "format": "", "speaker": "", "model": ""})
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !middleware.ValidateAPIKey(r) {
		metrics.AuthFailed.Inc(telemetry.Labels{})
		log.Printf("警告: API Key 鉴权失败 - 路径=%s 客户端=%s 远端=%s",
			r.URL.Path, middleware.GetClientIP(r), r.RemoteAddr)
		middleware.SendJSONError(w, http.StatusUnauthorized, "Invalid API key provided.", "invalid_request_error", "invalid_api_key")
		return
	}

	if setting.TTSConfigErr != nil {
		log.Printf("警告: TTS配置未就绪,拒绝请求 - 错误=%v 路径=%s 客户端=%s",
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

	clientFormat := resolveClientFormat(req.ResponseFormat)

	opts := setting.TTSOptions
	opts.Text = req.Input

	ctx, cancel := context.WithTimeout(r.Context(), setting.TTSTimeout)
	defer cancel()

	result, err := volcano.Synthesis(ctx, volcanoClient, opts, req.Input, clientFormat, speed, adapterRec)
	duration := time.Since(start)

	finalLabels := telemetry.Labels{
		"format":  clientFormat,
		"speaker": opts.Speaker,
		"model":   opts.Model,
	}
	if err != nil {
		finalLabels["status"] = classifyStatus(err)
		metrics.RequestTotal.Inc(finalLabels)
		metrics.RequestDuration.Observe(duration.Seconds(), telemetry.Labels{"status": finalLabels["status"], "format": clientFormat})
		log.Printf("警告: TTS 合成失败 - 路径=%s 客户端=%s 文本长度=%d 耗时=%v 错误=%v",
			r.URL.Path, middleware.GetClientIP(r), len(req.Input), duration, err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "TTS synthesis failed.", "server_error", "synthesis_failed")
		return
	}

	finalLabels["status"] = "ok"
	metrics.RequestTotal.Inc(finalLabels)
	metrics.RequestDuration.Observe(duration.Seconds(), telemetry.Labels{"status": "ok", "format": clientFormat})

	w.Header().Set("Content-Type", contentTypeFor(result.Format))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(result.AudioData)))
	w.Header().Set("X-Request-Id", result.ReqID)
	w.WriteHeader(http.StatusOK)
	w.Write(result.AudioData)
}

func classifyStatus(err error) string {
	if ue, ok := err.(*volcano.UpstreamError); ok {
		switch ue.Stage {
		case "request":
			return "request_error"
		case "http":
			return fmt.Sprintf("http_%d", ue.Code)
		case "stream":
			return "upstream_error"
		case "wrap":
			return "wrap_error"
		}
	}
	return "internal_error"
}

func contentTypeFor(format string) string {
	switch strings.ToLower(format) {
	case "wav":
		return "audio/wav"
	case "mp3":
		return "audio/mpeg"
	case "ogg_opus", "opus":
		return "audio/ogg"
	case "pcm":
		return "audio/L16"
	case "aac":
		return "audio/aac"
	case "flac":
		return "audio/flac"
	}
	return "application/octet-stream"
}

// HealthHandler 暴露运行期状态;无鉴权。
func HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if setting.TTSConfigErr != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	env := setting.CheckEnvironmentVariables()
	allRequired := env["all_required_vars_set"].(bool)

	status := "ok"
	if !allRequired {
		status = "configuration_error"
	}

	resp := dto.HealthResponse{
		Status:    status,
		Service:   "ByteDance TTS to OpenAI API Adapter",
		Version:   "2.0.0 (v3 API)",
		Uptime:    fmt.Sprintf("%.0f seconds", time.Since(startTime).Seconds()),
		StartTime: startTime.Format(time.RFC3339),
		Memory:    collectMemorySnapshot(),
		ConfigStatus: dto.ConfigStatusResponse{
			AllRequiredVarsSet: allRequired,
			ConfigError:        setting.TTSConfigErr != nil,
		},
	}
	json.NewEncoder(w).Encode(resp)
}

var startTime time.Time

func SetStartTime(t time.Time) { startTime = t }

func collectMemorySnapshot() map[string]interface{} {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return map[string]interface{}{
		"heap_alloc": ms.HeapAlloc,
		"heap_inuse": ms.HeapInuse,
		"goroutines": runtime.NumGoroutine(),
	}
}
