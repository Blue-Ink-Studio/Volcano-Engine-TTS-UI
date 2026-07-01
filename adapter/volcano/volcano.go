package volcano

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
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
// 官方 Go 示例:请求体仅含 req_params;严格按单文件参考实现的请求结构。

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
func Synthesis(config *dto.ByteDanceTTSConfig, httpClient *HTTPClient, text string, speed float64) (*dto.SynthesisResult, error) {
	reqID := uuid.NewString()
	speechRate := convertSpeedToSpeechRate(speed)

	// 构造请求体:严格按 v3 API 文档 JSON 结构
	req := ttsRequest{
		User:      ttsUser{UID: "uid"},
		Namespace: "BidirectionalTTS",
		ReqParams: ttsReqParams{
			Text:    text,
			Speaker: config.Speaker,
			AudioParams: ttsAudioParams{
				Format:     "wav",
				SampleRate: 24000,
				SpeechRate: speechRate,
			},
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal TTS request: %w", err)
	}
	// 诊断日志:记录实际发到上游的请求体(去 model/speaker/resource 关键字段)
	log.Printf("TTS upstream request: X-Api-Resource-Id=%s speaker=%s namespace=BidirectionalTTS body=%s",
		config.ResourceId, config.Speaker, string(body))

	// 鉴权 header 按 v3 新版控制台方式(Connection 由 Go http 默认 keep-alive)
	headers := map[string]string{
		"Content-Type":      "application/json",
		"Connection":        "keep-alive",
		"X-Api-Resource-Id": config.ResourceId,
		"X-Api-Request-Id":  reqID,
		"X-Api-Key":         config.ApiKey,
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

	return &dto.SynthesisResult{AudioData: audioData, ReqID: reqID, Format: "wav"}, nil
}
