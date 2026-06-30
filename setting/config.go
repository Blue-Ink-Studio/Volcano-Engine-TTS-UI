package setting

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/volcano-tts/tts-api/common"
	"github.com/volcano-tts/tts-api/dto"
)

// 全部环境变量读取的单一入口:其它包不允许直接 os.Getenv,只读这里的全局 Config。

// TTSConfig 上游火山 TTS 配置(由 InitTTSConfig 填充)。
var (
	TTSConfig    dto.ByteDanceTTSConfig
	TTSConfigErr error
)

// AuthConfig OpenAI 兼容接口的客户端 API Key 鉴权配置。
type AuthConfig struct {
	APIKeys []string
}

var Auth AuthConfig

// CORSConfig 跨域白名单配置。
type CORSConfig struct {
	Origins  []string
	AllowAll bool
}

var CORS CORSConfig

// ServerConfig HTTP 服务监听配置。
type ServerConfig struct {
	Port string
}

var Server ServerConfig

// InitAllConfigs 集中初始化所有配置,启动期调用一次。
// 返回 TTSConfigErr(火山 TTS 必填项缺失时为非 nil);其它 Config 缺失时不返回 error,
// 各自有合理兜底(Auth 放行 / CORS 拒绝跨域 / Server 默认 8080)。
func InitAllConfigs() {
	InitServerConfig()
	InitAuthConfig()
	InitCORSConfig()
	TTSConfigErr = InitTTSConfig()
}

// InitServerConfig 读取 PORT,缺省 common.DefaultPort。
func InitServerConfig() {
	Server.Port = os.Getenv("PORT")
	if Server.Port == "" {
		Server.Port = common.DefaultPort
	}
}

// InitAuthConfig 读取 OPENAI_TTS_API_KEY,支持逗号分隔多个 key。
// 留空时 Auth.APIKeys 为空,ValidateAPIKey 会放行所有请求。
func InitAuthConfig() {
	raw := os.Getenv("OPENAI_TTS_API_KEY")
	if raw == "" {
		Auth.APIKeys = nil
		return
	}
	parts := strings.Split(raw, ",")
	keys := make([]string, 0, len(parts))
	for _, p := range parts {
		k := strings.TrimSpace(p)
		if k != "" {
			keys = append(keys, k)
		}
	}
	Auth.APIKeys = keys
}

// InitCORSConfig 读取 ALLOWED_ORIGINS,按逗号分隔;支持 * 通配(AllowAll=true)。
// 留空时 CORS.Origins 为空,跨域请求会被拒绝。
func InitCORSConfig() {
	raw := os.Getenv("ALLOWED_ORIGINS")
	CORS.Origins = nil
	CORS.AllowAll = false
	if raw == "" {
		return
	}
	for _, p := range strings.Split(raw, ",") {
		o := strings.TrimSpace(p)
		if o == "" {
			continue
		}
		if o == "*" {
			CORS.AllowAll = true
			continue
		}
		CORS.Origins = append(CORS.Origins, normalizeOrigin(o))
	}
}

// normalizeOrigin 复制自原 middleware/cors.go:小写 + 去尾斜杠。
func normalizeOrigin(origin string) string {
	origin = strings.TrimSpace(origin)
	origin = strings.TrimRight(origin, "/")
	return strings.ToLower(origin)
}

// InitTTSConfig 读取火山 TTS 必填和可选配置,填充 TTSConfig。
// 必填项缺失时返回 error,服务可继续运行但 TTS 功能不可用。
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
			log.Printf("无效的超时设置 '%s',使用默认值: %v", timeoutStr, timeout)
		}
	}

	// 音频格式,默认 mp3(文档默认值,流式场景中 wav 会多次返回 header,不推荐)
	format := os.Getenv("BYTEDANCE_TTS_FORMAT")
	if format == "" {
		format = "mp3"
	}

	// 采样率,默认 24000
	sampleRate := 24000
	if srStr := os.Getenv("BYTEDANCE_TTS_SAMPLE_RATE"); srStr != "" {
		if sr, err := fmt.Sscanf(srStr, "%d", &sampleRate); err != nil || sr != 1 {
			log.Printf("无效的采样率设置 '%s',使用默认值: 24000", srStr)
			sampleRate = 24000
		}
		validRates := map[int]bool{8000: true, 16000: true, 22050: true, 24000: true, 32000: true, 44100: true, 48000: true}
		if !validRates[sampleRate] {
			log.Printf("不支持的采样率 %d,使用默认值: 24000", sampleRate)
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

// LogStartupSummary 在启动期打印所有 Config 的最终状态。
// 调用时机:InitAllConfigs 之后,ListenAndServe 之前。
// 必填项逐项输出,失败分支明确告知"v1/audio/speech 路由将 500"。
func LogStartupSummary() {
	log.Printf("=== 环境配置汇总 ===")
	log.Printf("服务端口: %s", Server.Port)

	if len(Auth.APIKeys) == 0 {
		log.Printf("OPENAI_TTS_API_KEY: 未设置(所有请求无需鉴权)")
	} else {
		log.Printf("OPENAI_TTS_API_KEY: 已设置 %d 个有效密钥", len(Auth.APIKeys))
	}

	if CORS.AllowAll {
		log.Printf("ALLOWED_ORIGINS: *(允许所有跨域,不可与凭据共用)")
	} else if len(CORS.Origins) == 0 {
		log.Printf("ALLOWED_ORIGINS: 未设置(跨域请求将被拒绝)")
	} else {
		log.Printf("ALLOWED_ORIGINS: 已配置 %d 个允许的跨域来源白名单", len(CORS.Origins))
	}

	// 火山 TTS 必填项逐项状态:缺则 ❌,有则 ✓(API Key 脱敏,仅显示头尾各 4 字符)
	log.Printf("火山 TTS 必填项状态:")
	type ttsCheck struct {
		name  string
		value string
		ok    bool
	}
	checks := []ttsCheck{
		{"BYTEDANCE_TTS_API_KEY", maskAPIKey(TTSConfig.ApiKey), TTSConfig.ApiKey != ""},
		{"BYTEDANCE_TTS_RESOURCE_ID", TTSConfig.ResourceId, TTSConfig.ResourceId != ""},
		{"BYTEDANCE_TTS_SPEAKER", TTSConfig.Speaker, TTSConfig.Speaker != ""},
	}
	missingCount := 0
	for _, c := range checks {
		mark := "✓"
		if !c.ok {
			mark = "❌"
			missingCount++
		}
		val := c.value
		if val == "" {
			val = "(未设置)"
		}
		log.Printf("  %s %s: %s", mark, c.name, val)
	}

	if TTSConfigErr != nil {
		log.Printf("火山 TTS 整体: 初始化失败,%d 个必填项缺失,/v1/audio/speech 路由将全部返回 500", missingCount)
	} else {
		log.Printf("火山 TTS 可选项: model=%s, format=%s, sample_rate=%d, timeout=%v",
			TTSConfig.Model, TTSConfig.Format, TTSConfig.SampleRate, TTSConfig.Timeout)
		log.Printf("火山 TTS 整体: 初始化成功")
	}
}

// maskAPIKey 对 API Key 脱敏,显示头 4 / 尾 4 字符,中间 * 号代替。
// 短于等于 8 字符整体掩为 ****,空串原样返回。
func maskAPIKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "****" + key[len(key)-4:]
}

// CheckEnvironmentVariables 返回环境变量状态,供 /health 端点使用。
// 不再直接 os.Getenv,改为读已初始化的全局 Config(单一数据源)。
func CheckEnvironmentVariables() map[string]interface{} {
	requiredVars := map[string]bool{
		"BYTEDANCE_TTS_API_KEY":     TTSConfig.ApiKey != "",
		"BYTEDANCE_TTS_RESOURCE_ID": TTSConfig.ResourceId != "",
		"BYTEDANCE_TTS_SPEAKER":     TTSConfig.Speaker != "",
	}

	missingVars := []string{}
	for varName, isSet := range requiredVars {
		if !isSet {
			missingVars = append(missingVars, varName)
		}
	}

	optionalVars := map[string]bool{

		"BYTEDANCE_TTS_MODEL":       TTSConfig.Model != "" && TTSConfig.Model != "seed-tts-2.0-standard",
		"BYTEDANCE_TTS_FORMAT":      TTSConfig.Format != "" && TTSConfig.Format != "mp3",
		"BYTEDANCE_TTS_SAMPLE_RATE": TTSConfig.SampleRate != 24000,
		"OPENAI_TTS_API_KEY":        len(Auth.APIKeys) > 0,
		"ALLOWED_ORIGINS":           CORS.AllowAll || len(CORS.Origins) > 0,
		"PORT":                      Server.Port != common.DefaultPort,
	}

	return map[string]interface{}{
		"all_required_vars_set": len(missingVars) == 0,
		"missing_required_vars": missingVars,
		"required_vars_set":     requiredVars,
		"optional_vars_set":     optionalVars,
	}
}

// CheckStaticFiles 静态文件存在性检查,/dashboard 路由需要 health.html。
func CheckStaticFiles() {
	if _, err := os.Stat("health.html"); os.IsNotExist(err) {
		log.Println("警告: health.html 不存在,/dashboard 路由将返回 404")
	}
}
