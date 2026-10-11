package volcano

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/volcano-tts/tts-api/adapter/provider"
	"github.com/volcano-tts/tts-api/dto"
)

// Provider 是火山 v3 TTS 的 provider.Provider 实现。
// 内部持有复用的 HTTPClient(keep-alive)。
type Provider struct {
	client *HTTPClient
}

// New 构造火山 provider。在 init() 里注册,主干经 provider.Get("volcano") 取用。
func New() *Provider {
	return &Provider{client: NewHTTPClient()}
}

// Name 实现 provider.Provider。
func (p *Provider) Name() string { return "volcano" }

// Capabilities 实现 provider.Provider。
// 火山支持多音色、倍率语速、字符计费,并需要 resource_id 额外维度。
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		Formats:     []string{"mp3", "wav", "pcm", "ogg_opus", "aac", "flac"},
		Speed:       true,
		Usage:       true,
		Voices:      true,
		ExtraScopes: true,
	}
}

// Synthesize 实现 provider.Provider:从归一化 Request 还原火山 Options 后执行合成。
// 行为与原 Synthesis(controller 直接调用)完全一致,仅入口形态变化。
func (p *Provider) Synthesize(ctx context.Context, req provider.Request, mtr provider.MetricsRecorder) (*dto.SynthesisResult, error) {
	if mtr == nil {
		mtr = provider.NoopMetrics()
	}
	opts := optionsFromRequest(req)
	return synthesize(ctx, p.client, opts, req.Format, mtr)
}

// optionsFromRequest 把 provider.Request 还原成火山 Options。
// 厂商私有参数从 Credentials.Scope(resource_id)与 Request.Extra 取回,与 BuildRequest 对称。
func optionsFromRequest(req provider.Request) Options {
	opts := Options{
		APIKey:     req.Credentials.APIKey,
		ResourceID: req.Credentials.Scope["resource_id"],
		Text:       req.Text,
		Speaker:    req.VoiceKey,
		Model:      req.Model,
		Format:     req.Format,
		SampleRate: req.SampleRate,
		SpeechRate: convertSpeedToSpeechRate(req.Speed),
		UID:        "uid",
	}
	if v := req.Extra["uid"]; v != "" {
		opts.UID = v
	}
	if v := req.Extra["bit_rate"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			opts.BitRate = n
		}
	}
	if v := req.Extra["loudness_rate"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			opts.LoudnessRate = n
		}
	}
	opts.EnableSubtitle = req.Extra["enable_subtitle"] == "1"
	opts.EnableTimestamp = req.Extra["enable_timestamp"] == "1"
	if v := req.Extra["additions"]; v != "" {
		var adds Additions
		if err := json.Unmarshal([]byte(v), &adds); err == nil {
			opts.Additions = &adds
		}
	}
	// emoji 默认不过滤(disable_emoji_filter=true):火山默认会"过滤"emoji,
	// 且过滤实现可能导致 emoji 之后的文本不被合成(用户实测)。
	// 这里默认开启;若调用方显式传了 disable_emoji_filter=false,则尊重其覆盖。
	if opts.Additions == nil {
		d := true
		opts.Additions = &Additions{DisableEmojiFilter: &d}
	} else if opts.Additions.DisableEmojiFilter == nil {
		d := true
		opts.Additions.DisableEmojiFilter = &d
	}
	return opts
}

// BuildRequest 把火山 Options 映射成归一化 provider.Request。
// 阶段0使用:setting 继续构造 volcano.Options,再经此函数转成 provider.Request 交给主干,
// 从而让 controller 不再依赖具体实现。
// 阶段2(配置命名空间化)后,此函数被"直接构造 provider.Request"取代。
func BuildRequest(opts Options) provider.Request {
	req := provider.Request{
		Text:       opts.Text,
		VoiceKey:   opts.Speaker,
		Model:      opts.Model,
		SampleRate: opts.SampleRate,
		Credentials: provider.Credentials{
			APIKey: opts.APIKey,
			Scope:  map[string]string{"resource_id": opts.ResourceID},
		},
		Extra: map[string]string{},
	}
	if opts.UID != "" && opts.UID != "uid" {
		req.Extra["uid"] = opts.UID
	}
	if opts.BitRate != 0 {
		req.Extra["bit_rate"] = strconv.Itoa(opts.BitRate)
	}
	if opts.LoudnessRate != 0 {
		req.Extra["loudness_rate"] = strconv.Itoa(opts.LoudnessRate)
	}
	if opts.EnableSubtitle {
		req.Extra["enable_subtitle"] = "1"
	}
	if opts.EnableTimestamp {
		req.Extra["enable_timestamp"] = "1"
	}
	if opts.Additions != nil && !opts.Additions.IsZero() {
		if b, err := json.Marshal(opts.Additions); err == nil {
			req.Extra["additions"] = string(b)
		}
	}
	return req
}

// init 注册火山 provider。主干经 provider.Get("volcano") 取用,新增上游不改主干。
func init() {
	provider.Register(New())
}
