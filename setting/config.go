package setting

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/volcano-tts/tts-api/common"
	"github.com/volcano-tts/tts-api/dto"
)

var (
	TTSConfig    dto.ByteDanceTTSConfig
	TTSConfigErr error
)

func InitTTSConfig() error {
	apiKey := os.Getenv("BYTEDANCE_TTS_API_KEY")
	resourceId := os.Getenv("BYTEDANCE_TTS_RESOURCE_ID")
	speaker := os.Getenv("BYTEDANCE_TTS_SPEAKER")
	model := os.Getenv("BYTEDANCE_TTS_MODEL")
	if model == "" {
		model = "seed-tts-2.0-standard" // 文档默认值 复刻音色可设为 seed-tts-2.0-expressive
	}

	missingVars := []string{}
	if apiKey == "" {
		missingVars = append(missingVars, "BYTEDANCE_TTS_API_KEY")
	}
	if resourceId == "" {
		missingVars = append(missingVars, "BYTEDANCE_TTS_RESOURCE_ID")
	}
	if speaker == "" {
		missingVars = append(missingVars, "BYTEDANCE_TTS_SPEAKER")
	}

	if len(missingVars) > 0 {
		return fmt.Errorf("缺少必需的环境变量: %v", missingVars)
	}

	url := "https://openspeech.bytedance.com/api/v3/tts/unidirectional"

	timeout := common.DefaultTimeout
	if timeoutStr := os.Getenv("BYTEDANCE_TTS_TIMEOUT"); timeoutStr != "" {
		if parsedTimeout, err := time.ParseDuration(timeoutStr); err == nil {
			timeout = parsedTimeout
		} else {
			log.Printf("无效的超时设置 '%s'，使用默认值: %v", timeoutStr, timeout)
		}
	}

	// 音频格式，默认 mp3（文档默认值，流式场景下 wav 会多次返回 header，不推荐）
	format := os.Getenv("BYTEDANCE_TTS_FORMAT")
	if format == "" {
		format = "mp3"
	}

	// 采样率，默认 24000
	sampleRate := 24000
	if srStr := os.Getenv("BYTEDANCE_TTS_SAMPLE_RATE"); srStr != "" {
		if sr, err := fmt.Sscanf(srStr, "%d", &sampleRate); err != nil || sr != 1 {
			log.Printf("无效的采样率设置 '%s'，使用默认值: 24000", srStr)
			sampleRate = 24000
		}
		validRates := map[int]bool{8000: true, 16000: true, 22050: true, 24000: true, 32000: true, 44100: true, 48000: true}
		if !validRates[sampleRate] {
			log.Printf("不支持的采样率 %d，使用默认值: 24000", sampleRate)
			sampleRate = 24000
		}
	}

	TTSConfig = dto.ByteDanceTTSConfig{
		ApiKey:     apiKey,
		ResourceId: resourceId,
		Speaker:    speaker,
		Model:      model,
		URL:        url,
		Timeout:    timeout,
		Format:     format,
		SampleRate: sampleRate,
	}
	return nil
}

func CheckEnvironmentVariables() map[string]interface{} {
	requiredVars := map[string]bool{
		"BYTEDANCE_TTS_API_KEY":     os.Getenv("BYTEDANCE_TTS_API_KEY") != "",
		"BYTEDANCE_TTS_RESOURCE_ID": os.Getenv("BYTEDANCE_TTS_RESOURCE_ID") != "",
		"BYTEDANCE_TTS_SPEAKER":     os.Getenv("BYTEDANCE_TTS_SPEAKER") != "",
	}

	missingVars := []string{}
	for varName, isSet := range requiredVars {
		if !isSet {
			missingVars = append(missingVars, varName)
		}
	}

	optionalVars := map[string]bool{
		"BYTEDANCE_TTS_TIMEOUT":     os.Getenv("BYTEDANCE_TTS_TIMEOUT") != "",
		"BYTEDANCE_TTS_MODEL":       os.Getenv("BYTEDANCE_TTS_MODEL") != "",
		"BYTEDANCE_TTS_FORMAT":      os.Getenv("BYTEDANCE_TTS_FORMAT") != "",
		"BYTEDANCE_TTS_SAMPLE_RATE": os.Getenv("BYTEDANCE_TTS_SAMPLE_RATE") != "",
		"OPENAI_TTS_API_KEY":        os.Getenv("OPENAI_TTS_API_KEY") != "",
		"ALLOWED_ORIGINS":           os.Getenv("ALLOWED_ORIGINS") != "",
		"PORT":                      os.Getenv("PORT") != "",
	}

	return map[string]interface{}{
		"all_required_vars_set": len(missingVars) == 0,
		"missing_required_vars": missingVars,
		"required_vars_set":     requiredVars,
		"optional_vars_set":     optionalVars,
	}
}

func CheckStaticFiles() {
	if _, err := os.Stat("health.html"); os.IsNotExist(err) {
		log.Println("警告: health.html 不存在，/dashboard 路由将返回 404")
	}
}
