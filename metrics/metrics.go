// Package metrics 集中声明本服务所有埋点指标,并提供 telemetry.Meter 的全局访问入口。
//
// 设计:
//   - 启动期 Init() 一次性注册所有指标;Panic 表示有重名 bug,应立即暴露。
//   - 上游适配器通过 AdapterRecorder 接入,无需直接 import telemetry。
//   - 控制器 / 中间件通过本包的全局变量直接 Inc/Observe/Set。
package metrics

import (
	"time"

	"github.com/volcano-tts/tts-api/telemetry"
)

var (
	// Meter 全局 telemetry Meter。
	Meter telemetry.Meter = telemetry.NoopMeter{}

	// HTTP 请求侧
	RequestTotal    *telemetry.Counter
	RequestDuration *telemetry.Histogram

	// 上游 TTS 调用侧
	UpstreamTotal    *telemetry.Counter
	UpstreamDuration *telemetry.Histogram
	UpstreamTTFB     *telemetry.Histogram
	UpstreamChunks   *telemetry.Counter
	UpstreamBytes    *telemetry.Counter
	UpstreamErrors   *telemetry.Counter
	UpstreamUsage    *telemetry.Counter

	// 限流 / 并发 / 鉴权
	ConcurrencyActive   *telemetry.Gauge
	ConcurrencyRejected *telemetry.Counter
	RateLimitRejected   *telemetry.Counter
	AuthFailed          *telemetry.Counter
)

// Init 初始化所有指标。在 main 启动期调用一次。
func Init() {
	m := telemetry.NewMeter()
	Meter = m

	RequestTotal = m.NewCounter(
		"tts_request_total",
		"Total /v1/audio/speech requests, labeled by status and chosen format/speaker/model.",
		"status", "format", "speaker", "model",
	)
	RequestDuration = m.NewHistogram(
		"tts_request_duration_seconds",
		"End-to-end /v1/audio/speech latency in seconds.",
		telemetry.DefaultLatencyBuckets,
		"status", "format",
	)

	UpstreamTotal = m.NewCounter(
		"tts_upstream_total",
		"Total upstream TTS calls, labeled by status.",
		"status", "format", "model", "speaker",
	)
	UpstreamDuration = m.NewHistogram(
		"tts_upstream_duration_seconds",
		"Upstream TTS call duration in seconds.",
		telemetry.DefaultLatencyBuckets,
		"status", "format",
	)
	UpstreamTTFB = m.NewHistogram(
		"tts_upstream_first_byte_seconds",
		"Time from request send to first audio chunk, in seconds.",
		telemetry.DefaultLatencyBuckets,
		"format",
	)
	UpstreamChunks = m.NewCounter(
		"tts_upstream_chunks_total",
		"Total audio chunks received from upstream.",
		"format",
	)
	UpstreamBytes = m.NewCounter(
		"tts_upstream_audio_bytes_total",
		"Total audio bytes (post-wrap) returned to clients.",
		"format",
	)
	UpstreamErrors = m.NewCounter(
		"tts_upstream_errors_total",
		"Upstream TTS errors, labeled by error code family.",
		"code",
	)
	UpstreamUsage = m.NewCounter(
		"tts_usage_text_words_total",
		"Text words charged by upstream, per model.",
		"model",
	)

	ConcurrencyActive = m.NewGauge(
		"tts_concurrency_active",
		"Current in-flight request count.",
	)
	ConcurrencyRejected = m.NewCounter(
		"tts_concurrency_rejected_total",
		"Requests rejected due to concurrency limit.",
	)
	RateLimitRejected = m.NewCounter(
		"tts_ratelimit_rejected_total",
		"Requests rejected due to per-IP rate limit.",
	)
	AuthFailed = m.NewCounter(
		"tts_auth_failed_total",
		"Requests rejected due to invalid/missing API key.",
	)
}

// AdapterRecorder 把 telemetry 指标适配为 volcano.MetricsRecorder。
type AdapterRecorder struct{}

// UpstreamStarted 满足 volcano.MetricsRecorder 接口。
func (AdapterRecorder) UpstreamStarted(speaker, model, format string) {
	UpstreamTotal.Inc(telemetry.Labels{"status": "started", "format": format, "model": model, "speaker": speaker})
}

// UpstreamFinished 满足 volcano.MetricsRecorder 接口。
func (AdapterRecorder) UpstreamFinished(speaker, model, format, status string, duration, ttfb time.Duration, chunks, audioBytes, errCode int) {
	labels := telemetry.Labels{"status": status, "format": format, "model": model, "speaker": speaker}
	UpstreamTotal.Inc(labels)
	UpstreamDuration.Observe(duration.Seconds(), telemetry.Labels{"status": status, "format": format})
	if ttfb > 0 {
		UpstreamTTFB.Observe(ttfb.Seconds(), telemetry.Labels{"format": format})
	}
	if chunks > 0 {
		UpstreamChunks.Add(float64(chunks), telemetry.Labels{"format": format})
	}
	if audioBytes > 0 {
		UpstreamBytes.Add(float64(audioBytes), telemetry.Labels{"format": format})
	}
	// 上游调用只要 status != "ok" 即视为错误。原版 if errCode != 0 会漏掉
	// errCode=0 的 request_error / transport_error / wrap_error / stream_error
	// (code=0 的流错误) 等场景,导致 transport 类错误在 /metrics 上完全不可见。
	if status != "ok" {
		UpstreamErrors.Inc(telemetry.Labels{"code": codeLabel(errCode)})
	}
}

// UpstreamUsage 满足 volcano.MetricsRecorder 接口。
func (AdapterRecorder) UpstreamUsage(model string, textWords int) {
	if textWords <= 0 {
		return
	}
	UpstreamUsage.Add(float64(textWords), telemetry.Labels{"model": model})
}

// codeLabel 把整数错误码格式化为 label value,聚合到 4 类便于仪表盘展示。
func codeLabel(code int) string {
	switch {
	case code == 0:
		return "transport"
	case code >= 400 && code < 500:
		return "client"
	case code >= 500 && code < 600:
		return "server"
	default:
		return "upstream"
	}
}
