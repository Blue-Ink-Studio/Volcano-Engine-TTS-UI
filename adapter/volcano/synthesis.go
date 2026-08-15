package volcano

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"time"

	"github.com/volcano-tts/tts-api/dto"
)

// MetricsRecorder 是适配器向上报告埋点的接口。
// 适配器本身不依赖 telemetry 包,controller 在 main 启动时把 Meter 适配成实现;
// 这样测试可以注入 mock,生产可以无侵入替换成 OTel。
type MetricsRecorder interface {
	UpstreamStarted(speaker, model, format string)
	UpstreamFinished(speaker, model, format, status string, duration, ttfb time.Duration, chunks, audioBytes, errCode int)
	UpstreamUsage(model string, textWords int)
}

// nopMetrics 是 MetricsRecorder 的 no-op 默认值。
type nopMetrics struct{}

func (nopMetrics) UpstreamStarted(string, string, string) {}
func (nopMetrics) UpstreamFinished(string, string, string, string, time.Duration, time.Duration, int, int, int) {
}
func (nopMetrics) UpstreamUsage(string, int) {}

// Synthesis 调用火山 v3 一次,返回组装好的结果。
func Synthesis(
	ctx context.Context,
	client *HTTPClient,
	opts Options,
	text string,
	clientFormat string,
	speed float64,
	mtr MetricsRecorder,
) (*dto.SynthesisResult, error) {
	if mtr == nil {
		mtr = nopMetrics{}
	}
	opts.Text = text
	opts.SpeechRate = convertSpeedToSpeechRate(speed)

	reqID := newRequestID()

	upstreamFormat := resolveUpstreamFormat(clientFormat)
	opts.Format = upstreamFormat
	if upstreamFormat != "pcm" && upstreamFormat != "mp3" && upstreamFormat != "ogg_opus" {
		opts.Format = "mp3"
	}

	started := time.Now()
	mtr.UpstreamStarted(opts.Speaker, opts.Model, opts.Format)

	body, err := buildRequest(opts)
	if err != nil {
		mtr.UpstreamFinished(opts.Speaker, opts.Model, opts.Format, "request_error", time.Since(started), 0, 0, 0, 0)
		return nil, &UpstreamError{Code: 0, Message: err.Error(), Stage: "request", Wrapped: err}
	}

	headers := map[string]string{
		"Content-Type":                          "application/json",
		"Connection":                            "keep-alive",
		"X-Api-Resource-Id":                     opts.ResourceID,
		"X-Api-Request-Id":                      reqID,
		"X-Api-Key":                             opts.APIKey,
		"X-Control-Require-Usage-Tokens-Return": "*",
	}

	log.Printf("TTS upstream: resource_id=%s speaker=%s model=%q format=%s sample_rate=%d speech_rate=%d additions=%q",
		opts.ResourceID, opts.Speaker, opts.Model, opts.Format, opts.SampleRate, opts.SpeechRate, extractAdditionsForLog(body))

	resp, err := client.PostStream(ctx, "https://openspeech.bytedance.com/api/v3/tts/unidirectional", headers, body)
	if err != nil {
		mtr.UpstreamFinished(opts.Speaker, opts.Model, opts.Format, "transport_error", time.Since(started), 0, 0, 0, 0)
		return nil, &UpstreamError{Code: 0, Message: err.Error(), Stage: "request", Wrapped: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		rawBody := ReadErrorBody(resp.Body)
		mtr.UpstreamFinished(opts.Speaker, opts.Model, opts.Format, fmt.Sprintf("http_%d", resp.StatusCode), time.Since(started), 0, 0, 0, resp.StatusCode)
		return nil, &UpstreamError{
			Code:    resp.StatusCode,
			Message: fmt.Sprintf("upstream http %d: %s", resp.StatusCode, rawBody),
			Stage:   "http",
		}
	}

	parsed, err := ParseStream(resp.Body, started)
	if err != nil {
		ue, _ := err.(*UpstreamError)
		code := 0
		if ue != nil {
			code = ue.Code
		}
		mtr.UpstreamFinished(opts.Speaker, opts.Model, opts.Format, "stream_error", time.Since(started), 0, 0, 0, code)
		return nil, err
	}

	duration := time.Since(started)

	finalData := parsed.AudioData
	finalFormat := clientFormat
	sampleRate := opts.SampleRate
	if clientFormat == "wav" {
		wav, wrapErr := WrapWAVHeader(parsed.AudioData, opts.SampleRate)
		if wrapErr != nil {
			mtr.UpstreamFinished(opts.Speaker, opts.Model, opts.Format, "wrap_error", duration, parsed.FirstChunk, parsed.Chunks, len(parsed.AudioData), 0)
			return nil, &UpstreamError{Code: 0, Message: wrapErr.Error(), Stage: "wrap", Wrapped: wrapErr}
		}
		finalData = wav
	}

	if parsed.HasUsage {
		mtr.UpstreamUsage(opts.Model, parsed.TextWords)
	}
	mtr.UpstreamFinished(opts.Speaker, opts.Model, opts.Format, "ok", duration, parsed.FirstChunk, parsed.Chunks, len(finalData), 0)

	return &dto.SynthesisResult{
		AudioData:  finalData,
		Format:     finalFormat,
		SampleRate: sampleRate,
		ReqID:      reqID,
		TextWords:  parsed.TextWords,
		Chunks:     parsed.Chunks,
		AudioBytes: len(finalData),
		TTFB:       parsed.FirstChunk,
		Duration:   duration,
	}, nil
}

// newRequestID 16 字节随机 ID(hex 编码),无外部依赖。
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// extractAdditionsForLog 从已编码的请求体里取 additions 字段值,便于日志展示。
func extractAdditionsForLog(body []byte) string {
	const key = `"additions":"`
	idx := bytesIndex(body, key)
	if idx < 0 {
		return ""
	}
	rest := body[idx+len(key):]
	end := bytesIndex(rest, []byte{'"'})
	if end < 0 {
		return ""
	}
	return string(rest[:end])
}

func bytesIndex(haystack []byte, needle string) int {
	if len(needle) == 0 {
		return 0
	}
outer:
	for i := 0; i+len(needle) <= len(haystack); i++ {
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}
