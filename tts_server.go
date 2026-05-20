package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

const (
	DEFAULT_PORT            = "8080"
	DEFAULT_TIMEOUT         = 30 * time.Second
	MAX_TEXT_LENGTH         = 5000
	MIN_SPEED               = 0.25
	MAX_SPEED               = 4.0
	DEFAULT_SPEED           = 1.0
	MAX_REQUEST_BODY_SIZE   = 1024 * 1024
	RATE_LIMIT_REQUESTS     = 100
	RATE_LIMIT_WINDOW       = time.Minute
	MAX_RESPONSE_TIMES      = 100
	MAX_ERRORS              = 10
	MAX_CONCURRENT_REQUESTS = 10
)

type V3TTSResponse struct {
	ReqID    string `json:"reqid"`
	Code     int    `json:"code"`
	Message  string `json:"message"`
	Event    string `json:"event"`
	Sequence int    `json:"sequence"`
	Data     string `json:"data"`
	Sentence string `json:"sentence,omitempty"`
	IsFinal  bool   `json:"is_final"`
	Usage    *Usage `json:"usage,omitempty"`
}

type Usage struct {
	TextWords int `json:"text_words"`
}

type OpenAITTSRequest struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice"`
	ResponseFormat string  `json:"response_format,omitempty"`
	Speed          float64 `json:"speed,omitempty"`
}

type ByteDanceTTSConfig struct {
	ApiKey     string
	ResourceId string
	Speaker    string
	URL        string
	Timeout    time.Duration
}

type RateLimiter struct {
	requests    map[string][]time.Time
	mutex       sync.Mutex
	limit       int
	window      time.Duration
	lastCleanup time.Time
}

const cleanupInterval = time.Hour

type Stats struct {
	totalRequests       int64
	successfulRequests  int64
	failedRequests      int64
	totalResponseTime   time.Duration
	recentResponseTimes []float64
	responseTimesIndex  int
	lastErrors          []string
	errorsIndex         int
	mutex               sync.RWMutex
}

var (
	VALID_API_KEYS   []string
	ttsConfig        ByteDanceTTSConfig
	ttsConfigErr     error
	globalHTTPClient *http.Client
	apiStats         *Stats
	rateLimiter      *RateLimiter
	concurrencySem   chan struct{}
)

func init() {
	globalHTTPClient = &http.Client{
		Timeout: DEFAULT_TIMEOUT,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}

	apiStats = &Stats{
		recentResponseTimes: make([]float64, MAX_RESPONSE_TIMES),
		lastErrors:          make([]string, MAX_ERRORS),
	}

	rateLimiter = &RateLimiter{
		requests: make(map[string][]time.Time),
		limit:    RATE_LIMIT_REQUESTS,
		window:   RATE_LIMIT_WINDOW,
	}

	concurrencySem = make(chan struct{}, MAX_CONCURRENT_REQUESTS)
}

func (rl *RateLimiter) Allow(key string) bool {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)

	if now.Sub(rl.lastCleanup) > cleanupInterval {
		rl.cleanup()
		rl.lastCleanup = now
	}

	timestamps := rl.requests[key]
	valid := make([]time.Time, 0, len(timestamps))
	for _, ts := range timestamps {
		if ts.After(cutoff) {
			valid = append(valid, ts)
		}
	}

	if len(valid) >= rl.limit {
		rl.requests[key] = valid
		return false
	}

	valid = append(valid, now)
	rl.requests[key] = valid
	return true
}

func (rl *RateLimiter) cleanup() {
	cutoff := time.Now().Add(-rl.window)
	for k, v := range rl.requests {
		valid := make([]time.Time, 0, len(v))
		for _, ts := range v {
			if ts.After(cutoff) {
				valid = append(valid, ts)
			}
		}
		if len(valid) == 0 {
			delete(rl.requests, k)
		} else {
			rl.requests[k] = valid
		}
	}
}

func initTTSConfig() error {
	apiKey := os.Getenv("BYTEDANCE_TTS_API_KEY")
	resourceId := os.Getenv("BYTEDANCE_TTS_RESOURCE_ID")
	speaker := os.Getenv("BYTEDANCE_TTS_SPEAKER")

	missingVars := []string{}

	if apiKey == "" {
		missingVars = append(missingVars, "BYTEDANCE_TTS_API_KEY")
	}
	if resourceId == "" {
		missingVars = append(missingVars, "BYTEDANCE_TTS_RESOURCE_ID")
	}
	if speaker == "" {
		missingVars = append(missingVars, "BYTEDANCE_TTS_SPEAKER")
	}

	if len(missingVars) > 0 {
		return fmt.Errorf("缺少必需的环境变量: %v", missingVars)
	}

	url := "https://openspeech.bytedance.com/api/v3/tts/unidirectional"

	timeout := DEFAULT_TIMEOUT
	if timeoutStr := os.Getenv("BYTEDANCE_TTS_TIMEOUT"); timeoutStr != "" {
		if parsedTimeout, err := time.ParseDuration(timeoutStr); err == nil {
			timeout = parsedTimeout
		} else {
			log.Printf("无效的超时设置 '%s'，使用默认值: %v", timeoutStr, timeout)
		}
	}

	ttsConfig = ByteDanceTTSConfig{
		ApiKey:     apiKey,
		ResourceId: resourceId,
		Speaker:    speaker,
		URL:        url,
		Timeout:    timeout,
	}

	return nil
}

func initAPIKeys() {
	apiKey := os.Getenv("OPENAI_TTS_API_KEY")
	if apiKey != "" {
		VALID_API_KEYS = strings.Split(apiKey, ",")
		for i, k := range VALID_API_KEYS {
			VALID_API_KEYS[i] = strings.TrimSpace(k)
		}
		log.Printf("已配置 %d 个有效的API密钥", len(VALID_API_KEYS))
	} else {
		log.Println("警告: OPENAI_TTS_API_KEY 环境变量未设置，将允许所有请求")
	}
}

func checkEnvironmentVariables() map[string]interface{} {
	requiredVars := map[string]bool{
		"BYTEDANCE_TTS_API_KEY":     os.Getenv("BYTEDANCE_TTS_API_KEY") != "",
		"BYTEDANCE_TTS_RESOURCE_ID": os.Getenv("BYTEDANCE_TTS_RESOURCE_ID") != "",
		"BYTEDANCE_TTS_SPEAKER":     os.Getenv("BYTEDANCE_TTS_SPEAKER") != "",
	}

	missingVars := []string{}
	for varName, isSet := range requiredVars {
		if !isSet {
			missingVars = append(missingVars, varName)
		}
	}

	optionalVars := map[string]bool{
		"BYTEDANCE_TTS_TIMEOUT": os.Getenv("BYTEDANCE_TTS_TIMEOUT") != "",
		"OPENAI_TTS_API_KEY":    os.Getenv("OPENAI_TTS_API_KEY") != "",
		"PORT":                  os.Getenv("PORT") != "",
	}

	return map[string]interface{}{
		"all_required_vars_set": len(missingVars) == 0,
		"missing_required_vars": missingVars,
		"required_vars_set":     requiredVars,
		"optional_vars_set":     optionalVars,
	}
}

func httpPostStream(url string, headers map[string]string, body []byte, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req = req.WithContext(ctx)

	return globalHTTPClient.Do(req)
}

func convertSpeedToSpeechRate(speed float64) int {
	if speed <= 0.5 {
		return -50
	}
	if speed >= 2.0 {
		return 100
	}
	return int((speed - 1.0) * 100)
}

type SynthesisResult struct {
	AudioData []byte
	ReqID     string
}

func synthesis(text string, speed float64) (*SynthesisResult, error) {
	reqID := uuid.NewString()

	speechRate := convertSpeedToSpeechRate(speed)

	params := map[string]interface{}{
		"user": map[string]interface{}{
			"uid": "uid",
		},
		"namespace": "BidirectionalTTS",
		"req_params": map[string]interface{}{
			"text":    text,
			"speaker": ttsConfig.Speaker,
			"audio_params": map[string]interface{}{
				"format":      "wav",
				"sample_rate": 24000,
				"speech_rate": speechRate,
			},
		},
	}

	headers := map[string]string{
		"Content-Type":      "application/json",
		"Connection":        "keep-alive",
		"X-Api-Resource-Id": ttsConfig.ResourceId,
		"X-Api-Request-Id":  reqID,
		"X-Api-Key":         ttsConfig.ApiKey,
	}

	bodyStr, err := json.Marshal(params)
	if err != nil {
		log.Printf("JSON marshal fail: %v", err)
		return nil, err
	}

	resp, err := httpPostStream(ttsConfig.URL, headers, bodyStr, ttsConfig.Timeout)
	if err != nil {
		log.Printf("http post fail: %v", err)
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("Failed to read error response body: %v", err)
		} else {
			log.Printf("TTS service error: status=%d, body=%s", resp.StatusCode, string(body))
		}
		return nil, fmt.Errorf("TTS service error: status %d", resp.StatusCode)
	}

	var audioData []byte
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var v3Resp V3TTSResponse
		if err := json.Unmarshal(line, &v3Resp); err != nil {
			log.Printf("unmarshal chunk fail: %v, line: %s", err, string(line))
			continue
		}

		if v3Resp.Code == 20000000 {
			if v3Resp.Usage != nil {
				log.Printf("TTS synthesis completed, usage: %+v", v3Resp.Usage)
			}
			for scanner.Scan() {
			}
			break
		}

		if v3Resp.Code != 0 {
			log.Printf("TTS service error: code=%d, message=%s", v3Resp.Code, v3Resp.Message)
			return nil, fmt.Errorf("TTS service error: %s", v3Resp.Message)
		}

		if v3Resp.Data != "" {
			chunk, err := base64.StdEncoding.DecodeString(v3Resp.Data)
			if err != nil {
				log.Printf("base64 decode fail: %v", err)
				return nil, err
			}
			audioData = append(audioData, chunk...)
		} else if v3Resp.Sentence != "" {
			log.Printf("Received sentence info (sequence %d): %s", v3Resp.Sequence, v3Resp.Sentence)
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("read stream fail: %v", err)
		return nil, err
	}

	if len(audioData) == 0 {
		return nil, fmt.Errorf("no audio data received")
	}

	return &SynthesisResult{AudioData: audioData, ReqID: reqID}, nil
}

func validateAPIKey(r *http.Request) bool {
	if len(VALID_API_KEYS) == 0 {
		return true
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return false
	}

	if !strings.HasPrefix(authHeader, "Bearer ") {
		return false
	}

	token := strings.TrimPrefix(authHeader, "Bearer ")
	for _, validKey := range VALID_API_KEYS {
		if token == validKey {
			return true
		}
	}
	return false
}

func getClientIP(r *http.Request) string {
	xForwardedFor := r.Header.Get("X-Forwarded-For")
	if xForwardedFor != "" {
		ips := strings.Split(xForwardedFor, ",")
		if len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}

	xRealIP := r.Header.Get("X-Real-IP")
	if xRealIP != "" {
		return xRealIP
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func openaiTTSHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !validateAPIKey(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"message": "Invalid API key provided.",
				"type":    "invalid_request_error",
				"code":    "invalid_api_key",
			},
		})
		return
	}

	if ttsConfigErr != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"message": fmt.Sprintf("TTS service configuration error: %v. Please check environment variables and restart the service.", ttsConfigErr),
				"type":    "configuration_error",
				"code":    "service_unavailable",
			},
		})
		return
	}

	clientIP := getClientIP(r)
	if !rateLimiter.Allow(clientIP) {
		log.Printf("警告: 已超过IP速率限制，拒绝请求 - 客户端IP: %s", clientIP)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"message": "Rate limit exceeded. Please try again later.",
				"type":    "rate_limit_error",
				"code":    "rate_limit_exceeded",
			},
		})
		return
	}

	select {
	case concurrencySem <- struct{}{}:
		defer func() { <-concurrencySem }()
	default:
		log.Printf("警告: 已达到最大并发请求数限制，拒绝请求 - 客户端IP: %s", getClientIP(r))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"message": "Server is busy, maximum concurrent requests reached. Please try again later.",
				"type":    "concurrency_limit_error",
				"code":    "max_concurrent_requests",
			},
		})
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MAX_REQUEST_BODY_SIZE))
	if err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "Failed to read request body", http.StatusBadRequest)
		}
		return
	}

	var req OpenAITTSRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if req.Input == "" {
		http.Error(w, "Input text is required", http.StatusBadRequest)
		return
	}

	if len(req.Input) > MAX_TEXT_LENGTH {
		http.Error(w, fmt.Sprintf("Input text too long (max %d characters)", MAX_TEXT_LENGTH), http.StatusBadRequest)
		return
	}

	speed := req.Speed
	if speed <= 0 {
		speed = DEFAULT_SPEED
	}
	if speed < MIN_SPEED {
		speed = MIN_SPEED
	}
	if speed > MAX_SPEED {
		speed = MAX_SPEED
	}

	ttsStart := time.Now()
	result, err := synthesis(req.Input, speed)
	duration := time.Since(ttsStart)

	if err != nil {
		addRequestStats(false, duration, err.Error())
		http.Error(w, "TTS synthesis failed", http.StatusInternalServerError)
		return
	}

	addRequestStats(true, duration, "")

	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(result.AudioData)))
	w.Header().Set("X-Request-Id", result.ReqID)
	w.WriteHeader(http.StatusOK)
	w.Write(result.AudioData)
}

func addRequestStats(success bool, responseTime time.Duration, errMsg string) {
	apiStats.mutex.Lock()
	defer apiStats.mutex.Unlock()

	apiStats.totalRequests++
	apiStats.totalResponseTime += responseTime

	apiStats.recentResponseTimes[apiStats.responseTimesIndex] = responseTime.Seconds() * 1000
	apiStats.responseTimesIndex = (apiStats.responseTimesIndex + 1) % MAX_RESPONSE_TIMES

	if success {
		apiStats.successfulRequests++
	} else {
		apiStats.failedRequests++
		if errMsg != "" {
			errInfo := fmt.Sprintf("%s: %s", time.Now().Format(time.RFC3339), errMsg)
			apiStats.lastErrors[apiStats.errorsIndex] = errInfo
			apiStats.errorsIndex = (apiStats.errorsIndex + 1) % MAX_ERRORS
		}
	}
}

func getMemoryInfo() map[string]interface{} {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return map[string]interface{}{
		"total_alloc": m.TotalAlloc,
		"heap_alloc":  m.HeapAlloc,
		"heap_inuse":  m.HeapInuse,
		"goroutines":  runtime.NumGoroutine(),
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if ttsConfigErr != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	apiStats.mutex.RLock()
	totalRequests := apiStats.totalRequests
	successfulRequests := apiStats.successfulRequests
	failedRequests := apiStats.failedRequests
	totalResponseTime := apiStats.totalResponseTime
	recentResponseTimes := make([]float64, 0, MAX_RESPONSE_TIMES)
	for _, t := range apiStats.recentResponseTimes {
		if t > 0 {
			recentResponseTimes = append(recentResponseTimes, t)
		}
	}
	lastErrors := make([]string, 0, MAX_ERRORS)
	for _, e := range apiStats.lastErrors {
		if e != "" {
			lastErrors = append(lastErrors, e)
		}
	}
	apiStats.mutex.RUnlock()

	var errorRate float64
	if totalRequests > 0 {
		errorRate = float64(failedRequests) / float64(totalRequests) * 100
	}

	var avgResponseTime float64
	if totalRequests > 0 {
		avgResponseTime = totalResponseTime.Seconds() * 1000 / float64(totalRequests)
	}

	envCheckStatus := checkEnvironmentVariables()
	allEnvVarsSet := envCheckStatus["all_required_vars_set"].(bool)

	status := "ok"
	if !allEnvVarsSet {
		status = "configuration_error"
	}

	response := map[string]interface{}{
		"status":     status,
		"service":    "ByteDance TTS to OpenAI API Adapter",
		"version":    "2.0.0 (v3 API)",
		"uptime":     fmt.Sprintf("%.0f seconds", time.Since(startTime).Seconds()),
		"start_time": startTime.Format(time.RFC3339),
		"memory":     getMemoryInfo(),
		"api_stats": map[string]interface{}{
			"total_requests":           totalRequests,
			"successful_requests":      successfulRequests,
			"failed_requests":          failedRequests,
			"error_rate_percent":       fmt.Sprintf("%.2f", errorRate),
			"avg_response_time_ms":     fmt.Sprintf("%.2f", avgResponseTime),
			"recent_response_times_ms": recentResponseTimes,
		},
		"errors": map[string]interface{}{
			"recent_errors_count": len(lastErrors),
		},
		"config_status": map[string]interface{}{
			"all_required_vars_set": allEnvVarsSet,
			"config_error":          ttsConfigErr != nil,
		},
	}

	json.NewEncoder(w).Encode(response)
}

var startTime time.Time

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (rec *statusRecorder) WriteHeader(code int) {
	rec.statusCode = code
	rec.ResponseWriter.WriteHeader(code)
}

var allowedOrigins []string

func initCORSConfig() {
	origins := os.Getenv("ALLOWED_ORIGINS")
	if origins != "" {
		allowedOrigins = strings.Split(origins, ",")
		for i, origin := range allowedOrigins {
			allowedOrigins[i] = strings.TrimSpace(origin)
		}
		log.Printf("已配置 %d 个允许的跨域来源", len(allowedOrigins))
	} else {
		log.Println("警告: ALLOWED_ORIGINS 环境变量未设置")
		log.Println("将使用反射模式允许所有请求来源，生产环境建议配置白名单")
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowOrigin := ""

		if len(allowedOrigins) == 0 {
			allowOrigin = origin
		} else {
			for _, allowed := range allowedOrigins {
				if allowed == origin {
					allowOrigin = origin
					break
				}
			}
		}

		if allowOrigin != "" {
			w.Header().Set("Access-Control-Allow-Origin", allowOrigin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func main() {
	startTime = time.Now()

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.SetPrefix("[TTS-Server] ")

	initAPIKeys()
	initCORSConfig()

	ttsConfigErr = initTTSConfig()
	if ttsConfigErr != nil {
		log.Printf("警告: 配置初始化失败: %v", ttsConfigErr)
		log.Printf("服务将继续运行，但TTS功能不可用，请检查环境变量配置")
	} else {
		log.Printf("配置初始化成功")
	}

	router := mux.NewRouter()

	router.Use(corsMiddleware)

	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				start := time.Now()
				next.ServeHTTP(w, r)
				log.Printf("%s %s %s %v", r.Method, r.RequestURI, r.RemoteAddr, time.Since(start))
				return
			}

			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
			next.ServeHTTP(rec, r)
			duration := time.Since(start)

			log.Printf("%s %s %s %d %v", r.Method, r.RequestURI, r.RemoteAddr, rec.statusCode, duration)
		})
	})

	router.HandleFunc("/v1/audio/speech", openaiTTSHandler).Methods("POST", "OPTIONS")
	router.HandleFunc("/health", healthHandler).Methods("GET")
	router.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "health.html")
	}).Methods("GET")
	router.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard", http.StatusFound)
	}).Methods("GET")

	port := os.Getenv("PORT")
	if port == "" {
		port = DEFAULT_PORT
	}

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
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
