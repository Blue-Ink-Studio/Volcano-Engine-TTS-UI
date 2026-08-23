package volcano

// Options 是火山 v3 TTS 适配器的完整调用参数集合。
// 由 setting 包从环境变量构造,controller 直接透传,不做 OpenAI 侧映射。
//
// 字段顺序与文档 3.x 节一致,便于对照。
type Options struct {
	// --- 鉴权 / 路由 ---
	APIKey     string // X-Api-Key
	ResourceID string // X-Api-Resource-Id,决定模型版本与计费,如 seed-icl-2.0

	// --- req_params 核心字段 ---
	Text    string
	Speaker string
	Model   string // 可空,仅复刻 2.0 生效;env 默认 seed-tts-2.0-standard
	UID     string // user.uid,默认 "uid"

	// --- audio_params ---
	Format          string // 上游实际请求的 format:mp3 / pcm / ogg_opus
	SampleRate      int    // 8000/16000/22050/24000/32000/44100/48000
	BitRate         int    // 可选,仅 MP3 生效
	SpeechRate      int    // [-50, 100]
	LoudnessRate    int    // [-50, 100]
	EnableSubtitle  bool   // 复刻 2.0 生效,返回 TTSSubtitle
	EnableTimestamp bool   // 复刻 1.0 生效,内嵌字级时间戳

	// --- additions(扩展参数,JSON 字符串承载)---
	// 文档明确 additions 在请求体里必须是 string,内容是 JSON。
	// 这里直接存结构体,序列化时由 MarshalJSON 输出为 string。
	Additions *Additions
}

// Additions 对应文档 3.4 节的扩展参数。
// 注意:在请求体里 additions 是 JSON 字符串,所以 MarshalJSON 序列化为 string。
type Additions struct {
	ModelType                  *int     `json:"model_type,omitempty"`               // 复刻 2.0 推荐显式指定,4=ICL V2、5=ICL V3
	ContextTexts               []string `json:"context_texts,omitempty"`            // 语音指令
	UseTagParser               *bool    `json:"use_tag_parser,omitempty"`           // 复刻 2.0 expressive 启用语音标签 Cot
	ExplicitLanguage           string   `json:"explicit_language,omitempty"`        // 明确语种
	ContextLanguage            string   `json:"context_language,omitempty"`         // 参考语种
	SilenceDuration            *int     `json:"silence_duration,omitempty"`         // 0~30000ms
	EnableLanguageDetector     *bool    `json:"enable_language_detector,omitempty"` // 自动识别语种
	DisableMarkdownFilter      *bool    `json:"disable_markdown_filter,omitempty"`  // 是否解析 markdown
	DisableEmojiFilter         *bool    `json:"disable_emoji_filter,omitempty"`     // 是否过滤 emoji
	MaxLengthFilterParenthesis *int     `json:"max_length_to_filter_parenthesis,omitempty"`
	UnsupportedCharRatio       *float64 `json:"unsupported_char_ratio_thresh,omitempty"`
	AIGCWatermark              *bool    `json:"aigc_watermark,omitempty"`
	AIGCMetadata               any      `json:"aigc_metadata,omitempty"`
	CacheConfig                any      `json:"cache_config,omitempty"`
	PostProcess                any      `json:"post_process,omitempty"`
}

// IsZero 报告 Additions 是否为空(没有任何字段设置),用于在序列化前跳过 additions。
func (a *Additions) IsZero() bool {
	if a == nil {
		return true
	}
	return a.ModelType == nil && a.ContextTexts == nil && a.UseTagParser == nil &&
		a.ExplicitLanguage == "" && a.ContextLanguage == "" && a.SilenceDuration == nil &&
		a.EnableLanguageDetector == nil && a.DisableMarkdownFilter == nil && a.DisableEmojiFilter == nil &&
		a.MaxLengthFilterParenthesis == nil && a.UnsupportedCharRatio == nil &&
		a.AIGCWatermark == nil && a.AIGCMetadata == nil && a.CacheConfig == nil && a.PostProcess == nil
}
