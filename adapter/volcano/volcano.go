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

func Synthesis(config *dto.ByteDanceTTSConfig, httpClient *HTTPClient, text string, speed float64, voice string) (*dto.SynthesisResult, error) {
	reqID := uuid.NewString()
	speechRate := convertSpeedToSpeechRate(speed)

	speaker := config.Speaker
	if voice != "" {
		speaker = voice
	}

	model := config.Model
	if model == "" {
		model = "seed-tts-2.0-standard" // 文档默认值 复刻音色可设为 seed-tts-2.0-expressive
	}

	// 请求体结构严格按火山 v3 单向流式 API 文档构造
	// https://www.volcengine.com/docs/6561/2528925
	// v3 鉴权只依赖 X-Api-Key 一个 header,不再需要业务集群参数
	params := map[string]interface{}{
		"user": map[string]interface{}{
			"uid": reqID, // 文档要求随机字符串,这里复用请求级 UUID
		},
		"namespace": "UnidirectionalTTS",
		"req_params": map[string]interface{}{
			"text":    text,
			"speaker": speaker,
			"model":   model, // 复刻音色必填
			"audio_params": map[string]interface{}{
				"format":      "wav",
				"sample_rate": 24000,
				"speech_rate": speechRate,
			},
		},
	}

	headers := map[string]string{
		"Content-Type":      "application/json",
		"Connection":        "keep-alive",
		"X-Api-Resource-Id": config.ResourceId, // 模型路由（seed-tts-2.0 / seed-icl-2.0）
		"X-Api-Request-Id":  reqID,
		"X-Api-Key":         config.ApiKey, // v3 鉴权 key
	}

	bodyStr, err := json.Marshal(params)
	if err != nil {
		log.Printf("JSON marshal fail: %v", err)
		return nil, err
	}

	resp, err := httpClient.PostStream(config.URL, headers, bodyStr, config.Timeout)
	if err != nil {
		log.Printf("http post fail: %v", err)
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("Failed to read error response body: %v", err)
		} else {
			log.Printf("TTS service error: status=%d, body=%s", resp.StatusCode, string(body))
		}
		return nil, fmt.Errorf("TTS service error: status %d", resp.StatusCode)
	}

	var audioData []byte
	scanner := bufio.NewScanner(resp.Body)
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

		if v3Resp.Code == 20000000 {
			if v3Resp.Usage != nil {
				log.Printf("TTS synthesis completed, usage: %+v", v3Resp.Usage)
			}
			for scanner.Scan() {
			}
			break
		}

		if v3Resp.Code != 0 {
			log.Printf("TTS service error: code=%d, message=%s", v3Resp.Code, v3Resp.Message)
			return nil, fmt.Errorf("TTS service error: %s", v3Resp.Message)
		}

		if v3Resp.Data != "" {
			chunk, err := base64.StdEncoding.DecodeString(v3Resp.Data)
			if err != nil {
				log.Printf("base64 decode fail: %v", err)
				return nil, err
			}
			audioData = append(audioData, chunk...)
		} else if v3Resp.Sentence != "" {
			log.Printf("Received sentence info (sequence %d): %s", v3Resp.Sequence, v3Resp.Sentence)
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("read stream fail: %v", err)
		return nil, err
	}

	if len(audioData) == 0 {
		return nil, fmt.Errorf("no audio data received")
	}

	return &dto.SynthesisResult{AudioData: audioData, ReqID: reqID}, nil
}
