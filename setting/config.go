package setting

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/volcano-tts/tts-api/adapter/volcano"
	"github.com/volcano-tts/tts-api/common"
	"github.com/volcano-tts/tts-api/dto"
)

// 全部环境变量读取的单一入口:其它包不允许直接 os.Getenv,只读这里的全局 Config。

// TTSOptions 是火山 v3 TTS 调用的完整参数集合,启动期由 InitTTSConfig 填充。
// 业务侧(controller)直接读取并传入 volcano.Synthesis。
var (
	TTSOptions   volcano.Options
	TTSConfigErr error
	// TTSTimeout 单次合成请求的超时;controller 用来派生 context。
	TTSTimeout time.Duration = common.DefaultTimeout
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
func InitAllConfigs() {
	InitServerConfig()
	InitAuthConfig()
	InitCORSConfig()
	TTSConfigErr = InitTTSConfig()
}

func InitServerConfig() {
	Server.Port = os.Getenv("PORT")
	if Server.Port == "" {
		Server.Port = common.DefaultPort
	}
}

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

func normalizeOrigin(origin string) string {
	origin = strings.TrimSpace(origin)
	origin = strings.TrimRight(origin, "/")
	return strings.ToLower(origin)
}

// InitTTSConfig 读取火山 TTS 必填和可选配置,填充 TTSOptions 与 TTSTimeout。
// 必填项缺失时返回 error,/v1/audio/speech 路由会拒绝请求。
func InitTTSConfig() error {
	apiKey := os.Getenv("BYTEDANCE_TTS_API_KEY")
	resourceId := os.Getenv("BYTEDANCE_TTS_RESOURCE_ID")
	speaker := os.Getenv("BYTEDANCE_TTS_SPEAKER")
	missing := []string{}
	if apiKey == "" {
		missing = append(missing, "BYTEDANCE_TTS_API_KEY")
	}
	if resourceId == "" {
		missing = append(missing, "BYTEDANCE_TTS_RESOURCE_ID")
	}
	if speaker == "" {
		missing = append(missing, "BYTEDANCE_TTS_SPEAKER")
	}
	if len(missing) > 0 {
		return fmt.Errorf("缺少必需的环境变量: %v", missing)
	}

	model := os.Getenv("BYTEDANCE_TTS_MODEL")
	format := getEnvDefault("BYTEDANCE_TTS_FORMAT", "mp3")
	sampleRate := getEnvInt("BYTEDANCE_TTS_SAMPLE_RATE", 24000)
	bitRate := getEnvInt("BYTEDANCE_TTS_BIT_RATE", 0)
	modelType := getEnvInt("BYTEDANCE_TTS_MODEL_TYPE", 0)
	explicitLanguage := os.Getenv("BYTEDANCE_TTS_EXPLICIT_LANGUAGE")
	enableSubtitle := getEnvBool("BYTEDANCE_TTS_ENABLE_SUBTITLE", false)

	var adds *volcano.Additions
	if modelType != 0 || explicitLanguage != "" {
		adds = &volcano.Additions{}
		if modelType != 0 {
			v := modelType
			adds.ModelType = &v
		}
		if explicitLanguage != "" {
			adds.ExplicitLanguage = explicitLanguage
		}
	}

	TTSTimeout = common.DefaultTimeout
	if ts := os.Getenv("BYTEDANCE_TTS_TIMEOUT"); ts != "" {
		if d, err := time.ParseDuration(ts); err == nil {
			TTSTimeout = d
		} else {
			log.Printf("无效的超时设置 %q,使用默认值 %v", ts, TTSTimeout)
		}
	}

	TTSOptions = volcano.Options{
		APIKey:         apiKey,
		ResourceID:     resourceId,
		UID:            "uid",
		Speaker:        speaker,
		Model:          model,
		Format:         format,
		SampleRate:     sampleRate,
		BitRate:        bitRate,
		SpeechRate:     0,
		LoudnessRate:   0,
		EnableSubtitle: enableSubtitle,
		Additions:      adds,
	}
	return nil
}

func getEnvDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func getEnvInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("环境变量 %s=%q 不是合法整数,使用默认 %d", name, v, def)
		return def
	}
	return n
}

func getEnvBool(name string, def bool) bool {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		log.Printf("环境变量 %s=%q 不是合法 bool,使用默认 %v", name, v, def)
		return def
	}
	return b
}

// CheckEnvironmentVariables 返回 /health 用的环境变量状态快照。
func CheckEnvironmentVariables() map[string]interface{} {
	required := map[string]bool{
		"BYTEDANCE_TTS_API_KEY":     TTSOptions.APIKey != "",
		"BYTEDANCE_TTS_RESOURCE_ID": TTSOptions.ResourceID != "",
		"BYTEDANCE_TTS_SPEAKER":     TTSOptions.Speaker != "",
	}
	missing := []string{}
	for k, ok := range required {
		if !ok {
			missing = append(missing, k)
		}
	}
	optional := map[string]bool{
		"BYTEDANCE_TTS_MODEL":             TTSOptions.Model != "",
		"BYTEDANCE_TTS_FORMAT":            TTSOptions.Format != "mp3",
		"BYTEDANCE_TTS_SAMPLE_RATE":       TTSOptions.SampleRate != 24000,
		"BYTEDANCE_TTS_EXPLICIT_LANGUAGE": TTSOptions.Additions != nil && TTSOptions.Additions.ExplicitLanguage != "",
		"OPENAI_TTS_API_KEY":              len(Auth.APIKeys) > 0,
		"ALLOWED_ORIGINS":                 CORS.AllowAll || len(CORS.Origins) > 0,
		"PORT":                            Server.Port != common.DefaultPort,
	}
	return map[string]interface{}{
		"all_required_vars_set": len(missing) == 0,
		"missing_required_vars": missing,
		"required_vars_set":     required,
		"optional_vars_set":     optional,
	}
}

// LogStartupSummary 启动期一次性打印所有 Config 状态。
func LogStartupSummary() {
	log.Printf("=== 环境配置汇总 ===")
	log.Printf("服务端口: %s", Server.Port)

	if len(Auth.APIKeys) == 0 {
		log.Printf("OPENAI_TTS_API_KEY: 未设置(所有请求无需鉴权)")
	} else {
		log.Printf("OPENAI_TTS_API_KEY: 已设置 %d 个有效密钥", len(Auth.APIKeys))
	}

	if CORS.AllowAll {
		log.Printf("ALLOWED_ORIGINS: *(允许所有跨域;不可与鉴权共用)")
	} else if len(CORS.Origins) == 0 {
		log.Printf("ALLOWED_ORIGINS: 未设置(跨域请求将被拒绝)")
	} else {
		log.Printf("ALLOWED_ORIGINS: 已配置 %d 个允许的跨域来源白名单", len(CORS.Origins))
	}

	log.Printf("火山 TTS 必填项状态:")
	type ttsCheck struct {
		name  string
		value string
		ok    bool
	}
	checks := []ttsCheck{
		{"BYTEDANCE_TTS_API_KEY", maskAPIKey(TTSOptions.APIKey), TTSOptions.APIKey != ""},
		{"BYTEDANCE_TTS_RESOURCE_ID", TTSOptions.ResourceID, TTSOptions.ResourceID != ""},
		{"BYTEDANCE_TTS_SPEAKER", TTSOptions.Speaker, TTSOptions.Speaker != ""},
	}
	missingCount := 0
	for _, c := range checks {
		mark := "✓"
		if !c.ok {
			mark = "✗"
			missingCount++
		}
		val := c.value
		if val == "" {
			val = "(未设置)"
		}
		log.Printf("  %s %s: %s", mark, c.name, val)
	}

	if TTSConfigErr != nil {
		log.Printf("火山 TTS 整体: 初始化失败(%d 个必填项缺失),/v1/audio/speech 路由将全部返回 500", missingCount)
	} else {
		log.Printf("火山 TTS 整体: 初始化成功")
	}
}

func maskAPIKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "****" + key[len(key)-4:]
}

// CheckStaticFiles 检查 /dashboard 路由依赖的 health.html 是否存在。
func CheckStaticFiles() {
	if _, err := os.Stat("health.html"); os.IsNotExist(err) {
		log.Println("警告: health.html 不存在,/dashboard 路由将返回 404")
	}
}

// 保留 dto.ByteDanceTTSConfig 引用避免 import 警告;
// 新代码不应再使用这个类型,设置已在 TTSOptions 中。
var _ = dto.ByteDanceTTSConfig{}
