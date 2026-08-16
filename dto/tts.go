package dto

import (
	"encoding/json"
	"time"
)

// OpenAITTSRequest 是 /v1/audio/speech 接收的请求体。
// 仅 input / speed / response_format 实际影响火山侧;
// voice / model 当前保留接收但不做映射,详见 controller。
type OpenAITTSRequest struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice"`
	ResponseFormat string  `json:"response_format,omitempty"`
	Speed          float64 `json:"speed,omitempty"`
}

// V3TTSResponse 是火山 v3 HTTP Chunked 流式响应中每一行的 JSON 结构。
// Sentence 字段上游有时返回字符串(TTSSentenceStart 里的句文本),有时返回对象
// ({"phonemes":[...],"text":"...","words":[...]}),用 json.RawMessage 兼容两种形态,
// 避免任意一种上游变更都导致整行解析失败。
type V3TTSResponse struct {
	ReqID    string          `json:"reqid"`
	Code     int             `json:"code"`
	Message  string          `json:"message"`
	Event    string          `json:"event"`
	Sequence int             `json:"sequence"`
	Data     string          `json:"data"`
	Sentence json.RawMessage `json:"sentence,omitempty"`
	IsFinal  bool            `json:"is_final"`
	Usage    *V3Usage        `json:"usage,omitempty"`
}

// SentenceText 从 Sentence 提取可读文本:
//   - 字符串直接返回
//   - 对象尝试取 .text 字段
//   - 其它情况返回原始 JSON
func (r *V3TTSResponse) SentenceText() string {
	if len(r.Sentence) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(r.Sentence, &s); err == nil {
		return s
	}
	var obj struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(r.Sentence, &obj); err == nil && obj.Text != "" {
		return obj.Text
	}
	return string(r.Sentence)
}

// V3Usage 由 X-Control-Require-Usage-Tokens-Return 触发,包含计费字符数。
type V3Usage struct {
	TextWords int `json:"text_words"`
}

// ByteDanceTTSConfig 是 setting 包的全局 TTS 配置,目前只承载鉴权 / URL / 超时;
// 完整的合成参数见 adapter/volcano.Options。
type ByteDanceTTSConfig struct {
	ApiKey     string
	ResourceId string
	URL        string
	Timeout    time.Duration
}

// SynthesisResult 是火山适配器向 controller 返回的最终结果。
// Format 与 AudioData 的实际编码一致;controller 据此设置响应 Content-Type。
type SynthesisResult struct {
	AudioData  []byte
	Format     string
	SampleRate int
	ReqID      string
	TextWords  int           // 来自 V3Usage,无 usage 时为 0
	Chunks     int           // 实际收到的音频 chunk 数
	AudioBytes int           // 解码后总字节数
	TTFB       time.Duration // 收到首个音频 chunk 的耗时
	Duration   time.Duration // 整体合成耗时
}

// SubtitleEntry 描述一个字级时间戳条目(当 enable_subtitle / enable_timestamp 启用时返回)。
type SubtitleEntry struct {
	Text     string
	StartMs  int
	EndMs    int
	Sequence int
	// 原始事件可能为不同形态,这里只保留通用字段
}
