package volcano

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/volcano-tts/tts-api/dto"
)

// 火山引擎 TTS v3 HTTP 单向流式 API 客户端。
// 官方文档:https://www.volcengine.com/docs/6561/1598757
// 官方 Go 示例:请求体仅含 req_params;本实现按 commit 4aed966 经验额外带上
// user.uid 和 namespace="UnidirectionalTTS"(早期用其它 namespace 出现过兼容性
// 问题,显式指定最稳)。复刻音色场景额外带 req_params.model。

type HTTPClient struct {
	client *http.Client
}

func NewHTTPClient() *HTTPClient {
	return &HTTPClient{
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		},
	}
}

func (h *HTTPClient) PostStream(url string, headers map[string]string, body []byte, timeout time.Duration) (*http.Response, error) {
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
	return h.client.Do(req)
}

// --- 请求体结构(对应火山 v3 API 请求 JSON) ---

type ttsRequest struct {
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
	Model       string         `json:"model"`
	AudioParams ttsAudioParams `json:"audio_params"`
}

type ttsAudioParams struct {
	Format     string `json:"format"`
	SampleRate int    `json:"sample_rate"`
	SpeechRate int    `json:"speech_rate"`
}

// convertSpeedToSpeechRate 把 OpenAI 风格的 speed（倍率）转成火山 v3 的 speech_rate（百分比）。
// 文档规定 speech_rate 范围 [-50, 100]，对应 0.5x ~ 2.0x 倍速。
// 输入超出范围会被截断到边界值。
func convertSpeedToSpeechRate(speed float64) int {
	rate := int((speed - 1.0) * 100)
	if rate < -50 {
		rate = -50
	}
	if rate > 100 {
		rate = 100
	}
	return rate
}

// resolveAPIFormat 根据用户期望的输出格式决定实际请求火山 API 的格式。
// 文档明确指出：流式场景下传入 wav 会多次返回 wav header，建议使用 pcm。
// 因此当用户要 wav 输出时，用 pcm 请求 API，最后由本端拼装完整 wav header。
func resolveAPIFormat(desiredFormat string) (apiFormat string, needWavHeader bool) {
	switch desiredFormat {
	case "wav":
		return "pcm", true
	case "mp3", "ogg_opus", "pcm":
		return desiredFormat, false
	default:
		return "mp3", false
	}
}

// buildWavHeader 构造标准 44 字节 WAV 文件头（16-bit PCM, mono）。
func buildWavHeader(dataLen int, sampleRate int) []byte {
	header := make([]byte, 44)
	byteRate := sampleRate * 2 // 16bit * 1channel / 8 * sampleRate
	blockAlign := 2            // 16bit / 8 * 1channel

	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(36+dataLen))
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16) // SubChunk1Size
	binary.LittleEndian.PutUint16(header[20:22], 1)  // PCM format
	binary.LittleEndian.PutUint16(header[22:24], 1)  // NumChannels
	binary.LittleEndian.PutUint32(header[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(header[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(header[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(header[34:36], 16) // BitsPerSample
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], uint32(dataLen))

	return header
}

// FormatContentType 返回音频格式对应的 HTTP Content-Type。
func FormatContentType(format string) string {
	switch format {
	case "mp3":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	case "ogg_opus":
		return "audio/ogg"
	case "pcm":
		return "audio/pcm"
	default:
		return "application/octet-stream"
	}
}

// MapOpenAIFormat 将 OpenAI TTS response_format 映射为火山 API 支持的格式。
// OpenAI 支持: mp3, opus, aac, flac, wav, pcm
// 火山支持: mp3, ogg_opus, pcm, wav(流式不推荐)
func MapOpenAIFormat(openaiFormat string) string {
	switch openaiFormat {
	case "mp3":
		return "mp3"
	case "opus":
		return "ogg_opus"
	case "wav":
		return "wav"
	case "pcm":
		return "pcm"
	case "aac", "flac":
		return "mp3" // 火山不支持 aac/flac，降级到 mp3
	default:
		return "mp3"
	}
}

func Synthesis(config *dto.ByteDanceTTSConfig, httpClient *HTTPClient, text string, speed float64, voice string, requestFormat string) (*dto.SynthesisResult, error) {
	reqID := uuid.NewString()
	speechRate := convertSpeedToSpeechRate(speed)

	speaker := config.Speaker
	if voice != "" {
		speaker = voice
	}

	model := config.Model
	if model == "" {
		model = "seed-tts-2.0-standard" // 文档默认值,复刻音色可设为 seed-tts-2.0-expressive
	}

	// 决定实际输出格式:优先用请求中指定的格式,否则用配置中的格式,最后默认 mp3
	outputFormat := config.Format
	if requestFormat != "" {
		outputFormat = requestFormat
	}
	if outputFormat == "" {
		outputFormat = "mp3"
	}

	// 根据输出格式确定 API 请求格式(wav → pcm + 本端封装 header)
	apiFormat, needWavHeader := resolveAPIFormat(outputFormat)

	sampleRate := config.SampleRate
	if sampleRate == 0 {
		sampleRate = 24000
	}

	// 构造请求体:严格按 v3 API 文档 JSON 结构
	req := ttsRequest{
		User:      ttsUser{UID: reqID},
		Namespace: "UnidirectionalTTS",
		ReqParams: ttsReqParams{
			Text:    text,
			Speaker: speaker,
			Model:   model,
			AudioParams: ttsAudioParams{
				Format:     apiFormat,
				SampleRate: sampleRate,
				SpeechRate: speechRate,
			},
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal TTS request: %w", err)
	}

	// 鉴权 header 按 v3 新版控制台方式(Connection 由 Go http 默认 keep-alive)
	headers := map[string]string{
		"Content-Type": "application/json",
		// 请求用量返回,合成结束时响应中携带 usage 字段
		"X-Control-Require-Usage-Tokens-Return": "*",
		"X-Api-Resource-Id":                     config.ResourceId, // 模型路由(seed-tts-2.0 / seed-icl-2.0)
		"X-Api-Request-Id":                      reqID,
		"X-Api-Key":                             config.ApiKey, // v3 鉴权 key
	}

	resp, err := httpClient.PostStream(config.URL, headers, body, config.Timeout)
	if err != nil {
		return nil, fmt.Errorf("send TTS request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			log.Printf("TTS service error: status=%d, read body fail: %v", resp.StatusCode, readErr)
		} else {
			log.Printf("TTS service error: status=%d, body=%s", resp.StatusCode, string(respBody))
		}
		return nil, fmt.Errorf("TTS service error: status %d", resp.StatusCode)
	}

	var audioData []byte
	scanner := bufio.NewScanner(resp.Body)
	// 初始 1MB / 最大 8MB,与示例同量级,留足 TTS 长文本 room
	scanner.Buffer(make([]byte, 1024*1024), 8*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var v3Resp dto.V3TTSResponse
		if err := json.Unmarshal(line, &v3Resp); err != nil {
			log.Printf("unmarshal chunk fail: %v, line: %s", err, string(line))
			continue
		}

		// code=20000000 表示合成结束
		if v3Resp.Code == 20000000 {
			if v3Resp.Usage != nil {
				log.Printf("TTS synthesis completed, usage: %+v", v3Resp.Usage)
			}
			// 跳过后续可能的空行
			for scanner.Scan() {
			}
			break
		}

		// 非零 code 为错误
		if v3Resp.Code != 0 {
			log.Printf("TTS service error: code=%d, message=%s, event=%s", v3Resp.Code, v3Resp.Message, v3Resp.Event)
			return nil, fmt.Errorf("TTS service error: %s", v3Resp.Message)
		}

		// 根据 event 字段分类处理
		switch v3Resp.Event {
		case "TTSSentenceStart":
			log.Printf("Sentence start: sequence=%d, sentence=%s", v3Resp.Sequence, v3Resp.Sentence)
		case "TTSSentenceEnd":
			log.Printf("Sentence end: sequence=%d", v3Resp.Sequence)
		default:
			// 音频数据 chunk:data 字段为 base64 编码的音频片段
			if v3Resp.Data != "" {
				chunk, err := base64.StdEncoding.DecodeString(v3Resp.Data)
				if err != nil {
					return nil, fmt.Errorf("decode audio chunk: %w", err)
				}
				audioData = append(audioData, chunk...)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read TTS stream: %w", err)
	}

	if len(audioData) == 0 {
		return nil, fmt.Errorf("no audio data received from TTS service")
	}

	// 若输出格式为 wav,需要在 pcm 数据前拼装完整的 wav header
	if needWavHeader {
		wavHeader := buildWavHeader(len(audioData), sampleRate)
		wavData := make([]byte, 0, len(wavHeader)+len(audioData))
		wavData = append(wavData, wavHeader...)
		wavData = append(wavData, audioData...)
		audioData = wavData
	}

	return &dto.SynthesisResult{AudioData: audioData, ReqID: reqID, Format: outputFormat}, nil
}
