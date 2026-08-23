package volcano

import (
	"encoding/json"
	"fmt"
)

// requestBody 是真正发到上游 v3 端点的 JSON 顶层结构。
type requestBody struct {
	User      ttsUser      `json:"user"`
	Namespace string       `json:"namespace"`
	ReqParams ttsReqParams `json:"req_params"`
}

type ttsUser struct {
	UID string `json:"uid"`
}

type ttsReqParams struct {
	Text        string         `json:"text"`
	Speaker     string         `json:"speaker"`
	Model       string         `json:"model,omitempty"`
	AudioParams ttsAudioParams `json:"audio_params"`
	Additions   string         `json:"additions,omitempty"` // 注意:字符串
}

type ttsAudioParams struct {
	Format          string `json:"format"`
	SampleRate      int    `json:"sample_rate"`
	BitRate         int    `json:"bit_rate,omitempty"`
	SpeechRate      int    `json:"speech_rate"`
	LoudnessRate    int    `json:"loudness_rate,omitempty"`
	EnableSubtitle  bool   `json:"enable_subtitle,omitempty"`
	EnableTimestamp bool   `json:"enable_timestamp,omitempty"`
}

// buildRequest 把 Options 序列化为上游请求体 JSON。
func buildRequest(opts Options) ([]byte, error) {
	if opts.Text == "" {
		return nil, fmt.Errorf("volcano: text is required")
	}
	if opts.Speaker == "" {
		return nil, fmt.Errorf("volcano: speaker is required")
	}
	if opts.ResourceID == "" {
		return nil, fmt.Errorf("volcano: resource id is required")
	}
	if opts.APIKey == "" {
		return nil, fmt.Errorf("volcano: api key is required")
	}

	body := requestBody{
		User:      ttsUser{UID: opts.UID},
		Namespace: "UnidirectionalTTS",
		ReqParams: ttsReqParams{
			Text:    opts.Text,
			Speaker: opts.Speaker,
			Model:   opts.Model,
			AudioParams: ttsAudioParams{
				Format:          opts.Format,
				SampleRate:      opts.SampleRate,
				BitRate:         opts.BitRate,
				SpeechRate:      opts.SpeechRate,
				LoudnessRate:    opts.LoudnessRate,
				EnableSubtitle:  opts.EnableSubtitle,
				EnableTimestamp: opts.EnableTimestamp,
			},
		},
	}

	if opts.Additions != nil && !opts.Additions.IsZero() {
		// 文档明确 additions 字段为 JSON 字符串。
		raw, err := json.Marshal(opts.Additions)
		if err != nil {
			return nil, fmt.Errorf("marshal additions: %w", err)
		}
		body.ReqParams.Additions = string(raw)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	return raw, nil
}

// convertSpeedToSpeechRate 把 OpenAI 风格的 speed(倍率)转换为 speech_rate(百分比)。
// speech_rate 范围 [-50, 100],对应 0.5x ~ 2.0x。
func convertSpeedToSpeechRate(speed float64) int {
	if speed <= 0 {
		speed = 1.0
	}
	rate := int((speed - 1.0) * 100)
	if rate < -50 {
		rate = -50
	}
	if rate > 100 {
		rate = 100
	}
	return rate
}

// resolveUpstreamFormat 决定上游实际请求的 format。
//   - 客户端要求 wav -> 上游走 pcm,我们本地拼 header
//   - 其他 -> 直接用 clientFormat
//
// sampleRate 在 wav 走 pcm 的情况下也按原样传给上游(影响 PCM 的实际采样率)。
func resolveUpstreamFormat(clientFormat string) string {
	if clientFormat == "wav" {
		return "pcm"
	}
	return clientFormat
}
