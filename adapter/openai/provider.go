// Package openai 实现 OpenAI 官方 TTS 上游适配器(/v1/audio/speech)。
//
// 契约:docs/STAGE6_MULTI_PROVIDER_PLAN.md §7.1。
//
// 与火山的差异:
//   - 只要 api_key(Authorization: Bearer),不需要 resource_id;
//   - 模型档位由请求的 model 决定(tts-1 / tts-1-hd),不是"复刻音色"概念;
//   - 输出固定 24kHz;
//   - 返回整段音频(非 chunked 流),因此实现比火山简单得多。
package openai

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/volcano-tts/tts-api/adapter/provider"
	"github.com/volcano-tts/tts-api/dto"
)

// defaultBaseURL 是官方端点;第三方 OpenAI 兼容上游可用 scope.base_url 覆盖。
const defaultBaseURL = "https://api.openai.com"

// defaultModel 是 voices.model 为空时的默认上游模型(G6)。
const defaultModel = "tts-1"

// fixedSampleRate:OpenAI TTS 固定 24kHz,不接受采样率参数。
const fixedSampleRate = 24000

// Provider 是 provider.Provider 的 OpenAI 实现。
type Provider struct {
	client *http.Client
}

// New 构造 OpenAI provider,供 init() 注册。
func New() *Provider {
	return &Provider{
		client: &http.Client{
			// 不设 Timeout:由调用方 ctx 控制(与火山一致,便于逐请求超时)。
			Timeout: 0,
		},
	}
}

// Name 实现 provider.Provider。
func (p *Provider) Name() string { return "openai" }

// Capabilities 实现 provider.Provider。
//
// Formats 只列**上游真实支持**的:mp3 / wav / pcm / opus(OGG 容器)。
// aac / flac 不在其中 —— 主干不会替我们降级(B9 要求 adapter 自己降级)。
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		Formats:     []string{"mp3", "wav", "pcm", "ogg_opus"},
		Speed:       true,
		Usage:       false, // /v1/audio/speech 不返回用量
		Voices:      true,
		ExtraScopes: false, // 只要 api_key(base_url 是可选覆盖,不算必需维度)
	}
}

// openaiSpeechRequest 是 OpenAI /v1/audio/speech 的请求体。
type openaiSpeechRequest struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice"`
	ResponseFormat string  `json:"response_format,omitempty"`
	Speed          float64 `json:"speed,omitempty"`
}

// Synthesize 实现 provider.Provider。
func (p *Provider) Synthesize(ctx context.Context, req provider.Request, mtr provider.MetricsRecorder) (*dto.SynthesisResult, error) {
	if mtr == nil {
		mtr = provider.NoopMetrics()
	}
	started := time.Now()

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = defaultModel // G6:voices.model 为空时的默认档位
	}

	upstreamFormat, resultFormat := mapFormat(req.Format)
	mtr.UpstreamStarted(req.VoiceKey, model, upstreamFormat)

	body, err := json.Marshal(openaiSpeechRequest{
		Model:          model,
		Input:          req.Text,
		Voice:          req.VoiceKey,
		ResponseFormat: upstreamFormat,
		Speed:          req.Speed,
	})
	if err != nil {
		mtr.UpstreamFinished(req.VoiceKey, model, upstreamFormat, "request_error", time.Since(started), 0, 0, 0, 0)
		return nil, &provider.UpstreamError{Code: 0, Message: "marshal request: " + err.Error(), Stage: "request", Wrapped: err}
	}

	base := strings.TrimRight(defaultBaseURL, "/")
	if v := req.Credentials.Scope["base_url"]; v != "" {
		base = strings.TrimRight(v, "/")
	}
	endpoint := base + "/v1/audio/speech"

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		mtr.UpstreamFinished(req.VoiceKey, model, upstreamFormat, "request_error", time.Since(started), 0, 0, 0, 0)
		return nil, &provider.UpstreamError{Code: 0, Message: err.Error(), Stage: "request", Wrapped: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.Credentials.APIKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		mtr.UpstreamFinished(req.VoiceKey, model, upstreamFormat, "transport_error", time.Since(started), 0, 0, 0, 0)
		return nil, &provider.UpstreamError{Code: 0, Message: err.Error(), Stage: "request", Wrapped: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		// 上游响应体可能含换行,转义后嵌入错误消息,避免伪造日志行。
		safe := strings.NewReplacer("\n", "\\n", "\r", "\\r").Replace(string(raw))
		mtr.UpstreamFinished(req.VoiceKey, model, upstreamFormat,
			fmt.Sprintf("http_%d", resp.StatusCode), time.Since(started), 0, 0, 0, resp.StatusCode)
		return nil, &provider.UpstreamError{
			Code:    resp.StatusCode,
			Message: fmt.Sprintf("upstream http %d: %s", resp.StatusCode, safe),
			Stage:   "http",
		}
	}

	audio, err := io.ReadAll(resp.Body)
	if err != nil {
		mtr.UpstreamFinished(req.VoiceKey, model, upstreamFormat, "stream_error", time.Since(started), 0, 0, 0, 0)
		return nil, &provider.UpstreamError{Code: 0, Message: err.Error(), Stage: "stream", Wrapped: err}
	}

	duration := time.Since(started)
	mtr.UpstreamFinished(req.VoiceKey, model, upstreamFormat, "ok", duration, 0, 1, len(audio), 0)
	mtr.UpstreamUsage(model, len([]rune(req.Text)))

	return &dto.SynthesisResult{
		AudioData:  audio,
		Format:     resultFormat,
		SampleRate: fixedSampleRate,
		ReqID:      newRequestID(),
		AudioBytes: len(audio),
		Duration:   duration,
	}, nil
}

// mapFormat 把对外格式映射成上游 response_format,并给出应当回写的真实格式。
//
//   - opus 系:对外 `ogg_opus` → 上游 `opus` → **回写 `ogg_opus`**
//     (OpenAI 的 opus 就是 OGG/Opus 容器,对外语义正确)
//   - aac / flac:上游不支持 → 降级 mp3,回写 mp3
//   - 其余原样
func mapFormat(clientFormat string) (upstream, result string) {
	switch strings.ToLower(clientFormat) {
	case "mp3":
		return "mp3", "mp3"
	case "wav":
		return "wav", "wav"
	case "pcm":
		return "pcm", "pcm"
	case "ogg_opus", "opus":
		return "opus", "ogg_opus"
	case "aac", "flac":
		return "mp3", "mp3" // 降级
	default:
		return "mp3", "mp3"
	}
}

// newRequestID 16 字节随机 ID(hex),与火山 adapter 的本地请求号同构,
// 用于 usage.request_id 与 X-Request-Id(OpenAI 不返回 reqid)。
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// init 注册 provider。主干经 provider.Get("openai") 取用,新增上游不改主干。
func init() { provider.Register(New()) }
