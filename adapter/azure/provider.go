// Package azure 实现 Azure Speech(TTS)上游适配器。
//
// 契约:docs/STAGE6_MULTI_PROVIDER_PLAN.md §7.2。
//
// 与 OpenAI/火山的形态差异(这是本期最"非标准"的一个上游):
//   - 鉴权头是 `Ocp-Apim-Subscription-Key`(不是 Bearer);
//   - 请求体是 **SSML**(XML),不是 JSON;
//   - 端点是 `https://{region}.tts.speech.microsoft.com/cognitiveservices/v1`;
//   - 输出格式由 `X-Microsoft-OutputFormat` 头决定,名字体系与我们的归一化格式完全不同;
//   - 采样率固定 24kHz;
//   - 没有 "model" 概念(SSML 里只有 <voice name>)。
package azure

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/volcano-tts/tts-api/adapter/provider"
	"github.com/volcano-tts/tts-api/common"
	"github.com/volcano-tts/tts-api/dto"
)

// fixedSampleRate:Azure 的 24kHz 系列输出固定 24k,不接受任意采样率。
const fixedSampleRate = 24000

// Provider 是 provider.Provider 的 Azure 实现。
type Provider struct {
	client *http.Client
}

// New 构造 Azure provider,供 init() 注册。
func New() *Provider {
	return &Provider{client: &http.Client{}}
}

// Name 实现 provider.Provider。
func (p *Provider) Name() string { return "azure" }

// NormalizeScope 实现 provider.ScopeNormalizer。
//
// 管理端表单只有一个 "Scope" 输入框,前端**统一**把用户填的值放进
// scope["resource_id"](历史字段名),而 Azure 需要的是 scope["region"]。
// 这里做一次搬运,让管理端不必知道每个 provider 的键名。
//
// 规则:
//   - scope["region"] 已有值 → 以它为准,不动 resource_id
//   - 否则 scope["resource_id"] 有值 → 搬到 region
//   - 都没有 → 不动(运行时再报"requires scope.region")
//
// 其它键(base_url 等)原样保留。
func (p *Provider) NormalizeScope(scope map[string]string) map[string]string {
	if len(scope) == 0 {
		return scope
	}
	out := make(map[string]string, len(scope)+1)
	for k, v := range scope {
		out[k] = v
	}
	if strings.TrimSpace(out["region"]) == "" {
		if v := strings.TrimSpace(out["resource_id"]); v != "" {
			out["region"] = v
		}
	}
	return out
}

// Capabilities 实现 provider.Provider。
//
// Formats 只列对外能真实交付的:mp3 / wav / pcm。
// ogg_opus / aac / flac 一律降级 mp3(见 mapOutputFormat)。
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		Formats:     []string{"mp3", "wav", "pcm"},
		Speed:       true,  // 通过 SSML <prosody rate>
		Usage:       false, // 上游不返回字符用量
		Voices:      true,
		ExtraScopes: true, // 需要 scope.region
	}
}

// Synthesize 实现 provider.Provider。
func (p *Provider) Synthesize(ctx context.Context, req provider.Request, mtr provider.MetricsRecorder) (*dto.SynthesisResult, error) {
	if mtr == nil {
		mtr = provider.NoopMetrics()
	}
	started := time.Now()

	region := strings.TrimSpace(req.Credentials.Scope["region"])
	if region == "" {
		return nil, &provider.UpstreamError{
			Code:    0,
			Message: "azure channel requires scope.region",
			Stage:   "request",
		}
	}
	// 归一化格式为空时按 mp3 处理(与主干 resolveClientFormat 的默认一致)。
	clientFormat := req.Format
	if clientFormat == "" {
		clientFormat = "mp3"
	}
	upstreamFormat, resultFormat := mapOutputFormat(clientFormat)

	mtr.UpstreamStarted(req.VoiceKey, "", upstreamFormat)

	ssml := buildSSML(req.VoiceKey, req.Text, req.Speed)

	base := strings.TrimRight(fmt.Sprintf("https://%s.tts.speech.microsoft.com", region), "/")
	if v := req.Credentials.Scope["base_url"]; v != "" {
		base = strings.TrimRight(v, "/")
	}
	endpoint := base + "/cognitiveservices/v1"

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(ssml))
	if err != nil {
		mtr.UpstreamFinished(req.VoiceKey, "", upstreamFormat, "request_error", time.Since(started), 0, 0, 0, 0)
		return nil, &provider.UpstreamError{Code: 0, Message: err.Error(), Stage: "request", Wrapped: err}
	}
	httpReq.Header.Set("Content-Type", "application/ssml+xml")
	httpReq.Header.Set("X-Microsoft-OutputFormat", upstreamFormat)
	httpReq.Header.Set("Ocp-Apim-Subscription-Key", req.Credentials.APIKey)
	httpReq.Header.Set("User-Agent", "tts-api")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		mtr.UpstreamFinished(req.VoiceKey, "", upstreamFormat, "transport_error", time.Since(started), 0, 0, 0, 0)
		return nil, &provider.UpstreamError{Code: 0, Message: err.Error(), Stage: "request", Wrapped: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		safe := strings.NewReplacer("\n", "\\n", "\r", "\\r").Replace(string(raw))
		mtr.UpstreamFinished(req.VoiceKey, "", upstreamFormat,
			fmt.Sprintf("http_%d", resp.StatusCode), time.Since(started), 0, 0, 0, resp.StatusCode)
		return nil, &provider.UpstreamError{
			Code:    resp.StatusCode,
			Message: fmt.Sprintf("upstream http %d: %s", resp.StatusCode, safe),
			Stage:   "http",
		}
	}

	audio, err := io.ReadAll(resp.Body)
	if err != nil {
		mtr.UpstreamFinished(req.VoiceKey, "", upstreamFormat, "stream_error", time.Since(started), 0, 0, 0, 0)
		return nil, &provider.UpstreamError{Code: 0, Message: err.Error(), Stage: "stream", Wrapped: err}
	}

	// 客户端要 pcm 时,上游给的是带 RIFF 头的 wav → 剥头。
	if resultFormat == "pcm" {
		stripped, sErr := common.StripWAVHeader(audio)
		if sErr != nil {
			mtr.UpstreamFinished(req.VoiceKey, "", upstreamFormat, "wrap_error", time.Since(started), 0, 0, 0, 0)
			return nil, &provider.UpstreamError{Code: 0, Message: "strip wav header: " + sErr.Error(), Stage: "wrap", Wrapped: sErr}
		}
		audio = stripped
	}

	duration := time.Since(started)
	mtr.UpstreamFinished(req.VoiceKey, "", upstreamFormat, "ok", duration, 0, 1, len(audio), 0)

	return &dto.SynthesisResult{
		AudioData:  audio,
		Format:     resultFormat,
		SampleRate: fixedSampleRate,
		ReqID:      newRequestID(),
		AudioBytes: len(audio),
		Duration:   duration,
	}, nil
}

// mapOutputFormat 把对外格式映射成 Azure 的 X-Microsoft-OutputFormat,并给出回写格式。
//
//   - wav → riff-24khz-16bit-mono-pcm(**已含 RIFF 头**,直接返回)
//   - pcm → 同一个上游格式,但 stlip 后返回裸 PCM
//   - mp3 → audio-24khz-48kbitrate-mono-mp3
//   - ogg_opus / aac / flac → 上游不支持,降级 mp3 并**如实回写 mp3**
//     (S10:必须回写真实格式,否则客户端按 ogg 解码 mp3 会全是噪音)
func mapOutputFormat(clientFormat string) (upstream, result string) {
	switch strings.ToLower(clientFormat) {
	case "wav":
		return "riff-24khz-16bit-mono-pcm", "wav"
	case "pcm":
		return "riff-24khz-16bit-mono-pcm", "pcm"
	case "mp3":
		return "audio-24khz-48kbitrate-mono-mp3", "mp3"
	default: // ogg_opus / aac / flac / 未知
		return "audio-24khz-48kbitrate-mono-mp3", "mp3"
	}
}

// buildSSML 构造 Azure 的 SSML 请求体。
//
// speed → <prosody rate>:Azure 用百分比字符串("-50%" ~ "+100%"),
// 我们的 speed 是倍率(0.25 ~ 4.0)⇒ rate = (speed-1)*100。
// speed<=0 视为原速(不写 rate 属性,让上游用默认值)。
func buildSSML(voice, text string, speed float64) string {
	var b strings.Builder
	b.WriteString(`<speak version='1.0' xml:lang='zh-CN'>`)
	b.WriteString(`<voice name='`)
	b.WriteString(escapeXML(voice))
	b.WriteString(`'>`)
	if speed > 0 && speed != 1.0 {
		pct := (speed - 1.0) * 100
		b.WriteString(`<prosody rate='`)
		if pct > 0 {
			b.WriteString("+")
		}
		fmt.Fprintf(&b, "%.0f%%", pct)
		b.WriteString(`'>`)
		b.WriteString(escapeXML(text))
		b.WriteString(`</prosody>`)
	} else {
		b.WriteString(escapeXML(text))
	}
	b.WriteString(`</voice></speak>`)
	return b.String()
}

// escapeXML 转义 SSML 文本节点里的 XML 特殊字符。
// 文本来自客户端输入,不转义会破坏 SSML 结构(甚至注入额外标签)。
func escapeXML(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"'", "&apos;",
		`"`, "&quot;",
	).Replace(s)
}

// newRequestID 16 字节随机 ID(hex),用于 usage.request_id 与 X-Request-Id。
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// init 注册 provider。
func init() { provider.Register(New()) }
