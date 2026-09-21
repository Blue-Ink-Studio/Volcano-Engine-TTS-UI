package setting

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/volcano-tts/tts-api/adapter/volcano"
	"github.com/volcano-tts/tts-api/common"
	"github.com/volcano-tts/tts-api/telemetry"
)

// 全部环境变量读取的单一入口:其它包不允许直接 os.Getenv,只读这里的全局 Config。
//
// 并发模型:
//   - TTSOptions / TTSTimeout / TTSConfigErr / Auth.APIKeys / CORS 是运行期可被
//     LoadRuntimeConfig(由 PUT /api/settings 触发)整体替换的"运行时配置快照"。
//     struct 整体赋值不是原子的,若 TTS 请求正在读,可能拿到半写状态。
//   - 解决: 用 ttsMu(RWMutex)统一保护这些字段;读路径用 Get* 拿快照(RLock),
//     写路径用 Set* 整体替换(Lock)。读多写少,RWMutex 读不互斥,不会显著拖慢热路径。
//   - Server.Port / TrustedProxyHops / SetupToken 仅启动期写、运行期读,无并发修改,
//     保持原样不加锁。
var (
	ttsMu       sync.RWMutex
	ttsOptions  volcano.Options
	ttsTimeout  time.Duration = common.DefaultTimeout
	ttsConfigErr error
	authAPIKeys []string
	// corsAllowAll / corsOrigins 拆成两个独立字段,各自在 RLock 下读取,
	// 避免 CORSConfig 整体读时被 Lock 阻塞热路径。
	corsAllowAll bool
	corsOrigins  []string
)

// GetTTSOptions 读 TTSOptions 快照(值类型,使用方可以放心使用,不会被并发写破坏)。
// 提供给业务侧(controller)的统一读取入口;LoadRuntimeConfig 写入时用 SetTTSOptions 整体替换。
// 业务侧不要缓存这个值跨 goroutine 使用(应该每次调用时重新拿)。
func GetTTSOptions() volcano.Options {
	ttsMu.RLock()
	defer ttsMu.RUnlock()
	return ttsOptions
}

// SetTTSOptions 整体替换 TTSOptions。LoadRuntimeConfig 写路径专用。
func SetTTSOptions(o volcano.Options) {
	ttsMu.Lock()
	defer ttsMu.Unlock()
	ttsOptions = o
}

// GetTTSTimeout 读当前超时;controller 用它派生 context。
func GetTTSTimeout() time.Duration {
	ttsMu.RLock()
	defer ttsMu.RUnlock()
	return ttsTimeout
}

// SetTTSTimeout 整体替换超时。
func SetTTSTimeout(d time.Duration) {
	ttsMu.Lock()
	defer ttsMu.Unlock()
	ttsTimeout = d
}

// GetTTSConfigErr 读运行时配置错误;nil 表示就绪。
func GetTTSConfigErr() error {
	ttsMu.RLock()
	defer ttsMu.RUnlock()
	return ttsConfigErr
}

// SetTTSConfigErr 设运行时配置错误;nil 表示清错。
func SetTTSConfigErr(err error) {
	ttsMu.Lock()
	defer ttsMu.Unlock()
	ttsConfigErr = err
}

// GetAuthAPIKeys 读鉴权 key 列表;返回拷贝防止业务侧持有底层 slice 后被并发写破坏。
// 外部不应直接读 Auth.APIKeys,统一走 Get*。
func GetAuthAPIKeys() []string {
	ttsMu.RLock()
	defer ttsMu.RUnlock()
	if len(authAPIKeys) == 0 {
		return nil
	}
	out := make([]string, len(authAPIKeys))
	copy(out, authAPIKeys)
	return out
}

// SetAuthAPIKeys 整体替换鉴权 key 列表;入参被复制以防外部后续修改影响内部状态。
func SetAuthAPIKeys(keys []string) {
	ttsMu.Lock()
	defer ttsMu.Unlock()
	if len(keys) == 0 {
		authAPIKeys = nil
		return
	}
	out := make([]string, len(keys))
	copy(out, keys)
	authAPIKeys = out
}

// GetCORSAllowAll 读 CORS 是否放行所有来源。
func GetCORSAllowAll() bool {
	ttsMu.RLock()
	defer ttsMu.RUnlock()
	return corsAllowAll
}

// GetCORSOrigins 读 CORS 白名单;返回拷贝防止业务侧持有后被并发写破坏。
func GetCORSOrigins() []string {
	ttsMu.RLock()
	defer ttsMu.RUnlock()
	if len(corsOrigins) == 0 {
		return nil
	}
	out := make([]string, len(corsOrigins))
	copy(out, corsOrigins)
	return out
}

// SetCORS 整体替换 CORS 配置;LoadRuntimeConfig 和 PUT /api/settings/cors 写路径专用。
func SetCORS(allowAll bool, origins []string) {
	ttsMu.Lock()
	defer ttsMu.Unlock()
	corsAllowAll = allowAll
	if len(origins) == 0 {
		corsOrigins = nil
		return
	}
	out := make([]string, len(origins))
	copy(out, origins)
	corsOrigins = out
}

// ServerConfig HTTP 服务监听配置。
type ServerConfig struct {
	Port string
}

var Server ServerConfig

// TrustedProxyHops 由 middleware.InitRateLimiter 在启动期写入,
// 表示当前 XFF 解析模式:0=启发式,N>0=精确 N 跳。
// setting.LogStartupSummary 读这个字段以展示运行期配置,
// 不直接调用 middleware(避免循环 import)。
//
// 【并发】仅启动期被 InitRateLimiter 写一次,运行期只读,无并发问题,不加锁。
var TrustedProxyHops int

// SetupToken 是安装模式下的初始化凭证。
//   - 若 TTS_ADMIN_KEY 环境变量非空,用其值(用户可复现,便于脚本化安装)
//   - 若 TTS_ADMIN_KEY 为空,启动时随机生成 32 字节十六进制,
//     打印到日志(/api/setup 提交时必须带这个 token)
//
// 安装完成后,/api/setup 端点永久关闭,SetupToken 失去意义但保留在内存。
//
// 【并发】仅 setup 阶段使用,运行期不会再写,无并发问题,不加锁。
var SetupToken string

// SetupTokenSource 标记 SetupToken 的来源,便于日志区分。
//   "env"      = 来自 TTS_ADMIN_KEY
//   "ephemeral"= 启动时随机生成(每次启动变)
//   ""         = 未设置
var SetupTokenSource string

// InitAllConfigs 集中初始化所有配置,启动期调用一次。
func InitAllConfigs() {
	InitServerConfig()
	InitAuthConfig()
	InitCORSConfig()
	InitSetupToken()
	// InitTTSConfig 不再这里调 — 改为启动期从 store 加载(LoadRuntimeConfig)。
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
		SetAuthAPIKeys(nil)
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
	SetAuthAPIKeys(keys)
}

func InitCORSConfig() {
	raw := os.Getenv("ALLOWED_ORIGINS")
	allowAll := false
	var origins []string
	if raw != "" {
		for _, p := range strings.Split(raw, ",") {
			o := strings.TrimSpace(p)
			if o == "" {
				continue
			}
			if o == "*" {
				allowAll = true
				continue
			}
			origins = append(origins, normalizeOrigin(o))
		}
	}
	SetCORS(allowAll, origins)
}

func normalizeOrigin(origin string) string {
	origin = strings.TrimSpace(origin)
	origin = strings.TrimRight(origin, "/")
	return strings.ToLower(origin)
}

// SplitOriginsForCORS 解析逗号/换行/空格分隔的 origins 列表,
// 全部小写、trim 末尾 / 后面统一比较。导出供 controller 复用
// (PUT /api/settings/cors 写完立即刷新 CORS 用)。
func SplitOriginsForCORS(s string) []string {
	return splitAndLowerOrigins(s)
}

// splitAndLowerOrigins 解析逗号/换行/空格分隔的 origins 列表,
// 全部小写、trim 末尾 / 后面统一比较(只在本包内用,外部用 SplitOriginsForCORS)。
// 实现细节:用 strings.FieldsFunc 切分,首尾 trim,末尾去 /。
func splitAndLowerOrigins(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '\n' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimRight(strings.TrimSpace(p), "/"))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// LoadRuntimeConfig 从 store 加载 TTS 全局配置到 TTSOptions / TTSTimeout / Auth.APIKeys 内存。
// 启动期(master 模式)调一次,或 PUT /api/settings 后调一次(改完立即生效)。
////
// 与原 InitTTSConfig 的区别:
//   - 不再读 BYTEDANCE_TTS_* env;全部从 store.Settings 读
//   - 必填项(api_key / default_resource_id / default_speaker)缺失时返 error
//   - 失败时 TTSConfigErr 被设置,/v1/audio/speech 路由会返 503
//   - 成功时清空 TTSConfigErr
//
// 【并发】所有写都走 Set* 系列函数,在 ttsMu.Lock 下完成;运行中的 TTS 请求
// 用 Get* 拿快照不会读到半写状态。
//
// 字段映射(原 env → store key):
//   BYTEDANCE_TTS_API_KEY        → api_key
//   BYTEDANCE_TTS_RESOURCE_ID    → default_resource_id
//   BYTEDANCE_TTS_SPEAKER        → default_speaker
//   BYTEDANCE_TTS_MODEL          → model
//   BYTEDANCE_TTS_FORMAT         → default_format (默认 mp3)
//   BYTEDANCE_TTS_SAMPLE_RATE    → sample_rate (默认 24000)
//   BYTEDANCE_TTS_BIT_RATE       → bit_rate (默认 0)
//   BYTEDANCE_TTS_MODEL_TYPE     → model_type (默认 0)
//   BYTEDANCE_TTS_EXPLICIT_LANGUAGE → explicit_language
//   BYTEDANCE_TTS_ENABLE_SUBTITLE   → enable_subtitle (默认 false)
//   BYTEDANCE_TTS_TIMEOUT        → TTSTimeout (默认 30s)
//
// 鉴权 key(auth_key)优先级:DB > env OPENAI_TTS_API_KEY
//   - 首次启动(无 DB 数据):用 env,保证向后兼容
//   - 已 install:用 DB,env 不再读
//   - DB 没 auth_key 但 env 有(env fallback):仍用 env
func LoadRuntimeConfig(s Store) error {
	all, err := s.SettingsGetAll()
	if err != nil {
		wrapped := fmt.Errorf("read settings failed: %w", err)
		SetTTSConfigErr(wrapped)
		return wrapped
	}

	apiKey := all["api_key"]
	resourceId := all["default_resource_id"]
	speaker := all["default_speaker"]
	missing := []string{}
	if apiKey == "" {
		missing = append(missing, "api_key")
	}
	if resourceId == "" {
		missing = append(missing, "default_resource_id")
	}
	if speaker == "" {
		missing = append(missing, "default_speaker")
	}
	if len(missing) > 0 {
		wrapped := fmt.Errorf("missing required settings: %v", missing)
		SetTTSConfigErr(wrapped)
		return wrapped
	}

	// 【BUG 修复 · 第二轮】default_speaker 是 voice **名字**(如 "chun"),
	// 不是火山 speaker ID (如 "S_G8tEKnaJ1")。原代码直接把 voice 名当
	// speaker ID 用,导致调 /v1/audio/speech 不传 voice 时火山 55000000。
	//
	// 字段优先级:
	//   - speaker     ←  从 default_speaker 这个 voice 查表拿真 ID (必查)
	//   - resource_id ←  settings 里的(用户偏好,不被 voice 行覆盖)
	//   - model       ←  voice 行的优先,settings 里的次之
	//
	// 之前 f5563e6 把 resourceId 也覆盖了,导致用户在 setup 设的
	// default_resource_id 永远没机会生效。这次只覆盖 speaker,不动 resourceId。
	var voiceModel string
	if vSpeaker, _, vModel, found, vErr := s.GetVoiceForTTS(speaker); vErr == nil && found {
		speaker = vSpeaker
		if vModel != "" {
			voiceModel = vModel
		}
		// 找不到 voice 时不报错 — 保持原值(向后兼容)
	}

	model := all["model"]
	if voiceModel != "" {
		model = voiceModel // voice 行有 model 时优先用 voice 的
	}
	format := all["default_format"]
	if format == "" {
		format = "mp3"
	}
	sampleRate, _ := s.SettingsGetInt("sample_rate", 24000)
	bitRate, _ := s.SettingsGetInt("bit_rate", 0)
	modelType, _ := s.SettingsGetInt("model_type", 0)
	explicitLanguage := all["explicit_language"]
	enableSubtitle, _ := s.SettingsGetBool("enable_subtitle", false)

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

	timeout := common.DefaultTimeout
	if v, err := s.SettingsGetDuration("timeout", common.DefaultTimeout); err == nil {
		timeout = v
	} else {
		timeout = common.DefaultTimeout
	}
	SetTTSTimeout(timeout)

	SetTTSOptions(volcano.Options{
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
	})

	// 鉴权 key:DB > env(向后兼容)
	authKey := all["auth_key"]
	if authKey == "" {
		authKey = os.Getenv("OPENAI_TTS_API_KEY")
	}
	if authKey != "" {
		SetAuthAPIKeys([]string{authKey})
	} else {
		SetAuthAPIKeys(nil)
	}

	// CORS 配置:DB > env
	// cors_allow_all (bool): 允许所有来源(*)
	// cors_origins (string): 逗号分隔白名单
	// 同源豁免由 middleware/cors.go 的 isSameOrigin 负责,DB 这里只管跨域名单
	corsAllowAll, _ := s.SettingsGetBool("cors_allow_all", false)
	if !corsAllowAll {
		// env 兜底
		if v := strings.ToLower(strings.TrimSpace(os.Getenv("CORS_ALLOW_ALL"))); v == "1" || v == "true" || v == "yes" {
			corsAllowAll = true
		}
	}
	originsStr := strings.TrimSpace(all["cors_origins"])
	if originsStr == "" {
		originsStr = os.Getenv("ALLOWED_ORIGINS")
	}
	if corsAllowAll {
		SetCORS(true, nil)
	} else if originsStr != "" {
		SetCORS(false, SplitOriginsForCORS(originsStr))
	} else {
		SetCORS(false, nil)
	}

	SetTTSConfigErr(nil)
	return nil
}

// Store 是 LoadRuntimeConfig 需要的最小接口(避免 setting 包 import store 产生 cycle)。
//
// 重要:VoiceGetByName 用 4 个返回值(speaker/resourceID/model/found/err)
// 而非 (VoiceRef, error),这样 store 包不需要 import setting 包
// 就能实现这个接口(避免循环 import)。
type Store interface {
	SettingsGetAll() (map[string]string, error)
	SettingsGetInt(key string, def int) (int, error)
	SettingsGetBool(key string, def bool) (bool, error)
	SettingsGetDuration(key string, def time.Duration) (time.Duration, error)
	// GetVoiceForTTS 给定 voice 名字,返 (speaker_id, resource_id, model, found, err)。
	//   - found=false 表示 voice 不存在(此时 err=nil,返回值是空串)
	//   - err!=nil 是真错误(db 失败等)
	//   - 找到时返 voice 行的真实字段值
	GetVoiceForTTS(name string) (speaker, resourceID, model string, found bool, err error)
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

// InitSetupToken 加载或生成安装模式下的初始化凭证。
//   - TTS_ADMIN_KEY 存在:用其值,SetupTokenSource="env"
//   - TTS_ADMIN_KEY 空:随机生成 16 字节 = 32 字符 hex,SetupTokenSource="ephemeral",打印到日志
func InitSetupToken() {
	v := os.Getenv("TTS_ADMIN_KEY")
	if v != "" {
		SetupToken = v
		SetupTokenSource = "env"
		return
	}
	// 临时 token:16 字节随机 = 32 字符 hex,够用且短
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 极端情况:随机源失败,降级为时间戳(不应发生)
		log.Printf("[setup] 生成一次性 token 失败,使用时间戳: %v", err)
		SetupToken = fmt.Sprintf("dev-%d", time.Now().UnixNano())
		SetupTokenSource = "ephemeral"
		return
	}
	SetupToken = hex.EncodeToString(b)
	SetupTokenSource = "ephemeral"
	log.Printf("[setup] 一次性安装 token(仅打印一次,公网部署请设置 TTS_ADMIN_KEY): %s", SetupToken)
}

// CheckEnvironmentVariables 返回 /health 用的环境变量状态快照。
func CheckEnvironmentVariables() map[string]interface{} {
	// 一次性拿所有需要的快照,缩短锁占用窗口;后续只读本地变量。
	opts := GetTTSOptions()
	authKeys := GetAuthAPIKeys()
	allowAll := GetCORSAllowAll()
	origins := GetCORSOrigins()

	required := map[string]bool{
		"BYTEDANCE_TTS_API_KEY":     opts.APIKey != "",
		"BYTEDANCE_TTS_RESOURCE_ID": opts.ResourceID != "",
		"BYTEDANCE_TTS_SPEAKER":     opts.Speaker != "",
	}
	missing := []string{}
	for k, ok := range required {
		if !ok {
			missing = append(missing, k)
		}
	}
	optional := map[string]bool{
		"BYTEDANCE_TTS_MODEL":             opts.Model != "",
		"BYTEDANCE_TTS_FORMAT":            opts.Format != "mp3",
		"BYTEDANCE_TTS_SAMPLE_RATE":       opts.SampleRate != 24000,
		"BYTEDANCE_TTS_EXPLICIT_LANGUAGE": opts.Additions != nil && opts.Additions.ExplicitLanguage != "",
		"OPENAI_TTS_API_KEY":              len(authKeys) > 0,
		"ALLOWED_ORIGINS":                 allowAll || len(origins) > 0,
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

	authKeys := GetAuthAPIKeys()
	if len(authKeys) == 0 {
		log.Printf("OPENAI_TTS_API_KEY: 未设置(所有请求无需鉴权)")
	} else {
		log.Printf("OPENAI_TTS_API_KEY: 已设置 %d 个有效密钥", len(authKeys))
	}

	allowAll := GetCORSAllowAll()
	origins := GetCORSOrigins()
	if allowAll {
		log.Printf("ALLOWED_ORIGINS: *(允许所有跨域;不可与鉴权共用)")
	} else if len(origins) == 0 {
		log.Printf("ALLOWED_ORIGINS: 未设置(跨域请求将被拒绝)")
	} else {
		log.Printf("ALLOWED_ORIGINS: 已配置 %d 个允许的跨域来源白名单", len(origins))
	}

	if h := TrustedProxyHops; h == 0 {
		log.Printf("TRUSTED_PROXY_HOPS: 启发式模式(默认,XFF 链尾第一个公网 IP)")
	} else {
		log.Printf("TRUSTED_PROXY_HOPS: 精确模式,信任 %d 跳反代", h)
	}

	// 一次性拿 TTSOptions 快照;后续只读本地变量,避免在多个 log.Printf 调用之间
	// 被 LoadRuntimeConfig 整体替换导致打印出不连贯的数据。
	opts := GetTTSOptions()
	log.Printf("火山 TTS 必填项状态:")
	type ttsCheck struct {
		name  string
		value string
		ok    bool
	}
	checks := []ttsCheck{
		{"BYTEDANCE_TTS_API_KEY", maskAPIKey(opts.APIKey), opts.APIKey != ""},
		{"BYTEDANCE_TTS_RESOURCE_ID", telemetry.MaskResourceID(opts.ResourceID), opts.ResourceID != ""},
		// speaker 是火山复刻音色 ID(用户付费资产),日志里打码,避免明文落盘
		{"BYTEDANCE_TTS_SPEAKER", telemetry.MaskSpeaker(opts.Speaker), opts.Speaker != ""},
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

	if err := GetTTSConfigErr(); err != nil {
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
