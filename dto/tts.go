package dto

import "time"

type OpenAITTSRequest struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice"`
	ResponseFormat string  `json:"response_format,omitempty"`
	Speed          float64 `json:"speed,omitempty"`
}

type V3TTSResponse struct {
	ReqID    string   `json:"reqid"`
	Code     int      `json:"code"`
	Message  string   `json:"message"`
	Event    string   `json:"event"`
	Sequence int      `json:"sequence"`
	Data     string   `json:"data"`
	Sentence string   `json:"sentence,omitempty"`
	IsFinal  bool     `json:"is_final"`
	Usage    *V3Usage `json:"usage,omitempty"`
}

type V3Usage struct {
	TextWords int `json:"text_words"`
}

type ByteDanceTTSConfig struct {
	ApiKey     string
	ResourceId string
	Speaker    string
	Model      string // v3 声音复刻/语音大模型 子模型版本，复刻音色必填
	URL        string
	Timeout    time.Duration
	Format     string // 音频编码格式: mp3/ogg_opus/pcm/wav（wav内部用pcm请求再封装header）
	SampleRate int    // 采样率: 8000/16000/22050/24000/32000/44100/48000
}

type SynthesisResult struct {
	AudioData []byte
	ReqID     string
	Format    string // 实际输出格式，用于设置 Content-Type
}
