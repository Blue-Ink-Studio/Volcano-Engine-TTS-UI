package setting

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/volcano-tts/tts-api/adapter/volcano"
	"github.com/volcano-tts/tts-api/common"
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
	ttsMu    sync.RWMutex
	ttsTimeout time.Duration = common.DefaultTimeout
	// defaultFormat / defaultSampleRate 是**渠道无关**的输出默认值。
	//
	// STAGE6 起不再有"全局 TTS 配置"(api_key/resource_id/default_speaker 全部
	// 下沉到渠道),但"客户端没指定 response_format 时用什么"仍是全局默认,
	// 所以这里保留 format 与 sample rate 两个中性项。
	defaultFormat     string = "mp3"
	defaultSampleRate int    = 24000
	// globalExtras 是渠道无关的全局调音参数(bit_rate / additions 等),
	// 由 controller 透传进 provider.Request.Extra,再交给具体 adapter 解释。
	// 用 map[string]string 而非火山类型,避免把厂商结构暴露给主干。
	globalExtras map[string]string
	authAPIKeys  []string
	// adminAPIKeys 是**管理接口专用**凭证(admin_key)。
	// 与 authAPIKeys(业务侧,/v1/audio/speech 用)分离:
	//   - admin_key 为空时回退到 auth_key/env,老部署行为完全不变
	//   - admin_key 配了之后,业务 key 无法访问 /api/admin/*,权限隔离成立
	// 详见 docs/IMPLEMENT_v0.3.0.md 阶段 1。
	adminAPIKeys []string
	// adminKeySource 记录管理凭证的来源,仅用于启动摘要与排障。
	adminKeySource string
	// corsAllowAll / corsOrigins 拆成两个独立字段,各自在 RLock 下读取,
	// 避免 CORSConfig 整体读时被 Lock 阻塞热路径。
	corsAllowAll bool
	corsOrigins  []string
)

// GetTTSOptions / GetTTSRequest / SetTTSOptions —— STAGE6 已**删除**。
//
// 它们的作用是把"全局 volcano.Options"桥接成 provider.Request 交给主干,带来两个误导:
//  1. 每个请求的 Credentials.Scope 里都恒有一个 resource_id,即使渠道是 OpenAI;
//  2. 主干以为存在"全局音色/全局凭证"这条路径,而 STAGE5 之后它已经死了
//     (setup 不再写 api_key/default_speaker)。
//
// STAGE6(B8)改为:provider.Request 由 **controller 按渠道 + 音色行现场构造**,
// 厂商维度全部由 adapter 从 Credentials.Scope / Request.Extra 自行解释。
// 本包只保留渠道无关的默认值:GetDefaultFormat / GetDefaultSampleRate / GetGlobalExtras。

// GetDefaultFormat 返回默认输出格式(客户端未指定 response_format 时回退用)。
//
// STAGE6:不再来自 volcano.Options —— 格式是渠道无关概念,恒有值(默认 mp3)。
func GetDefaultFormat() string {
	ttsMu.RLock()
	defer ttsMu.RUnlock()
	if defaultFormat == "" {
		return "mp3"
	}
	return defaultFormat
}

// GetDefaultSampleRate 返回默认采样率(渠道无关;无配置时 24000)。
// adapter 可自行忽略(OpenAI/Azure 固定 24k),火山用它决定上游采样率。
func GetDefaultSampleRate() int {
	ttsMu.RLock()
	defer ttsMu.RUnlock()
	if defaultSampleRate <= 0 {
		return 24000
	}
	return defaultSampleRate
}

// GetGlobalExtras 返回全局调音参数(渠道无关)的拷贝。
//
// controller 把它塞进 provider.Request.Extra;具体含义由 adapter 解释
// (火山读 bit_rate / additions 等,OpenAI/Azure 忽略)。
// 返回拷贝,防止调用方持有底层 map 被并发写破坏。
func GetGlobalExtras() map[string]string {
	ttsMu.RLock()
	defer ttsMu.RUnlock()
	if len(globalExtras) == 0 {
		return nil
	}
	out := make(map[string]string, len(globalExtras))
	for k, v := range globalExtras {
		out[k] = v
	}
	return out
}

// SetGlobalExtras 整体替换全局调音参数(LoadRuntimeConfig 写路径专用)。
func SetGlobalExtras(m map[string]string) {
	ttsMu.Lock()
	defer ttsMu.Unlock()
	if len(m) == 0 {
		globalExtras = nil
		return
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	globalExtras = out
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

// GetTTSConfigErr —— STAGE6 已**删除**。
//
// 它承载的是"全局 TTS 必填项(api_key / default_resource_id / default_speaker)
// 是否齐备"的判定。STAGE5 起 setup 不再写这三项(凭证由渠道自带),于是它
// **恒非 nil**,导致 /health 在 normal 模式永久 503(K8s readiness 恒红)。
//
// 渠道化之后"全局 TTS 配置"这个概念本身就不存在了,因此整个判定被移除:
//   - /health 只按 store 是否就绪判 503
//   - 上游凭证缺失由渠道层在请求时自然暴露(no_channel / unknown_model / 上游 401)
//
// 保留本注释是为了让后来者知道这里曾有东西、以及为什么删掉。

// GetAuthAPIKeys 读鉴权 key 列表;返回拷贝防止业务侧持有底层 slice 后被并发写破坏。

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

// GetAdminKeys 读**管理接口专用**凭证列表;返回拷贝防止业务侧持有底层 slice。
// 供 middleware.RequireAdmin 使用;业务侧鉴权请用 GetAuthAPIKeys。
func GetAdminKeys() []string {
	ttsMu.RLock()
	defer ttsMu.RUnlock()
	if len(adminAPIKeys) == 0 {
		return nil
	}
	out := make([]string, len(adminAPIKeys))
	copy(out, adminAPIKeys)
	return out
}

// SetAdminKeys 整体替换管理凭证;入参被复制。source 仅用于启动摘要展示。
func SetAdminKeys(keys []string, source string) {
	ttsMu.Lock()
	defer ttsMu.Unlock()
	adminKeySource = source
	if len(keys) == 0 {
		adminAPIKeys = nil
		return
	}
	out := make([]string, len(keys))
	copy(out, keys)
	adminAPIKeys = out
}

// GetAdminKeySource 返回管理凭证来源:admin_key / auth_key / env / ""(未配置)。
func GetAdminKeySource() string {
	ttsMu.RLock()
	defer ttsMu.RUnlock()
	return adminKeySource
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

// InitAuthConfig 【STAGE5 已退役】。
//
// 原职责:把环境变量 `OPENAI_TTS_API_KEY`(逗号分隔)灌进业务鉴权 key 列表。
//
// STAGE5 契约 §8 退役了 `OPENAI_TTS_API_KEY`,且**不做过渡兼容**(当前无生产环境),
// 因此这里不再读环境变量,业务鉴权 key 列表恒为空 ——
// 业务鉴权完全由 `tokens` 表(用户自建令牌)承担,见 middleware/auth.go 的
// AuthenticateWithStore 分支与 store.TokenGetByKey。
//
// 保留函数本身是为了不破坏 InitAllConfigs 的调用顺序(初始化序列的可读性
// 比省一行调用更重要)。
func InitAuthConfig() {
	SetAuthAPIKeys(nil)
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
		return fmt.Errorf("read settings failed: %w", err)
	}

	// STAGE6:**删除"全局 TTS 必填项"判定**。
	//
	// 旧逻辑要求 api_key / default_resource_id / default_speaker 三项齐备,缺一
	// 就 SetTTSConfigErr 并让 /v1/audio/speech 返 503。
	//
	// 但 STAGE5 起 setup **刻意不写**这三项(凭证由渠道自带),于是它们永远缺失
	// ⇒ GetTTSConfigErr() 恒非 nil ⇒ **/health 在 normal 模式永久 503**,
	// K8s readiness / Docker HEALTHCHECK 恒红(这个 bug 从 STAGE5 一直带到现在)。
	//
	// 渠道化之后"全局 TTS 配置"这个概念本就不该存在:缺凭证由渠道层在请求时
	// 自然暴露(no_channel / unknown_model / 上游 401),不需要启动期判定。

	format := all["default_format"]
	if format == "" {
		format = "mp3"
	}
	sampleRate, _ := s.SettingsGetInt("sample_rate", 24000)
	ttsMu.Lock()
	defaultFormat = format
	defaultSampleRate = sampleRate
	ttsMu.Unlock()

	// 全局调音参数(渠道无关,由 adapter 自行解释)。
	// 这里只做"把 settings 里的火山风格开关翻译成 Request.Extra 的键"这件事,
	// 不再构造 volcano.Options / provider.Request。
	bitRate, _ := s.SettingsGetInt("bit_rate", 0)
	modelType, _ := s.SettingsGetInt("model_type", 0)
	explicitLanguage := all["explicit_language"]
	enableSubtitle, _ := s.SettingsGetBool("enable_subtitle", false)

	extras := map[string]string{}
	if bitRate != 0 {
		extras["bit_rate"] = strconv.Itoa(bitRate)
	}
	if enableSubtitle {
		extras["enable_subtitle"] = "1"
	}
	// additions 是火山专属的 JSON 字符串;为空时不写这个键。
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
	// 默认不过滤 emoji(火山默认过滤会截断 emoji 之后的文本,用户实测)。
	// 与 adapter/volcano.optionsFromRequest 的默认值保持一致。
	if adds == nil {
		d := true
		adds = &volcano.Additions{DisableEmojiFilter: &d}
	} else if adds.DisableEmojiFilter == nil {
		d := true
		adds.DisableEmojiFilter = &d
	}
	if b, err := json.Marshal(adds); err == nil {
		extras["additions"] = string(b)
	}
	SetGlobalExtras(extras)

	timeout := common.DefaultTimeout
	if v, err := s.SettingsGetDuration("timeout", common.DefaultTimeout); err == nil {
		timeout = v
	}
	SetTTSTimeout(timeout)

	// 鉴权 key:【STAGE5 已退役】。
	//
	// 旧逻辑是 "auth_key(DB) > OPENAI_TTS_API_KEY(env)",作为"业务调用凭证"。
	// STAGE5(契约 §8)把两者一并退役:业务凭证只来自 `tokens` 表(用户自建令牌),
	// 一个"全局 key"既能调业务又没有身份,和多用户体系根本不相容。
	//
	// 因此这里恒置空。注意这不影响 /v1/audio/speech:
	// 正常模式下走的是 middleware.AuthenticateWithStore(查 tokens 表),
	// GetAuthAPIKeys() 只在安装模式的兜底分支里被使用。
	SetAuthAPIKeys(nil)

	// 管理凭证:【STAGE5 起只有 admin_key(DB) 一个来源】。
	//
	// 旧优先级是 admin_key(DB) > auth_key(DB) > OPENAI_TTS_API_KEY(env),
	// 后两者已退役。admin_key 现在定位为**可选的运维后门**(契约 D3d):
	// 管理入口以 owner 账号 + JWT 为主,admin_key 用于"owner 忘密码"时自救。
	if all["admin_key"] != "" {
		SetAdminKeys([]string{all["admin_key"]}, "admin_key")
	} else {
		SetAdminKeys(nil, "")
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

	return nil
}

// Store 是 LoadRuntimeConfig 需要的最小接口(避免 setting 包 import store 产生 cycle)。
//
// 重要:接口只保留**渠道无关**的设置读取 —— STAGE6 起 setting 不再解析音色
// (音色归属渠道,由 controller 用 VoiceResolve(channelID, name) 解析),
// 因此原来的 GetVoiceForTTS 已从接口中移除,store 包不再需要为它做适配。
type Store interface {
	SettingsGetAll() (map[string]string, error)
	SettingsGetInt(key string, def int) (int, error)
	SettingsGetBool(key string, def bool) (bool, error)
	SettingsGetDuration(key string, def time.Duration) (time.Duration, error)
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

// InitSetupToken 生成安装模式下的**一次性**初始化凭证。
//
// STAGE5(契约 D2/D3):**只随机生成,不再读 `TTS_ADMIN_KEY` 环境变量**。
//
// 为什么去掉 env 分支:env 渠道意味着"一个环境变量就能决定谁能完成安装"——
// 在多用户体系落地后,安装凭证应当是**进程自己产生、只出现在启动日志里**的一次性秘密,
// 而不是可以被部署脚本/镜像长期携带的静态值。
func InitSetupToken() {
	// 16 字节随机 = 32 字符 hex:够用且短,便于从日志里复制
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
	log.Printf("[setup] 一次性安装 token(仅本次启动有效,从本行复制): %s", SetupToken)
}

// CheckEnvironmentVariables —— STAGE6 已**删除**。
//
// 它原本给 /health 的 config_status 提供"火山必填项是否齐备"的快照,
// 而那套判定随渠道化一起消失了(见 README / 契约 §9)。
// /health 只剩 db_not_ready 一个 503 条件。

// LogStartupSummary 启动期一次性打印所有 Config 状态。
func LogStartupSummary() {
	log.Printf("=== 环境配置汇总 ===")
	log.Printf("服务端口: %s", Server.Port)

	authKeys := GetAuthAPIKeys()
	if len(authKeys) == 0 {
		// STAGE5(契约 §8):OPENAI_TTS_API_KEY / auth_key 已退役,这里恒为空是**预期状态**。
		log.Printf("业务鉴权: 全局 key 已退役(STAGE5),凭证只来自 tokens 表(用户自建令牌)")
	} else {
		log.Printf("业务鉴权: 检测到 %d 个全局 key(⚠️ 非预期状态,STAGE5 起不应再有)", len(authKeys))
	}

	// v0.3.0 的 admin_key > auth_key > env 优先级在 STAGE5 收敛为**只有 admin_key**;
	// admin_key 现在是可选的运维后门,管理入口以 owner 账号 + JWT 为主。
	switch GetAdminKeySource() {
	case "admin_key":
		log.Printf("管理凭证: 使用独立 admin_key(业务 key 无法访问管理接口)")
	case "auth_key":
		log.Printf("管理凭证: ⚠️ 来源为已退役的 auth_key —— 请改设 admin_key,或清空它以只依赖 owner 账号")
	default:
		log.Printf("管理凭证: 未配置 admin_key(正常状态);管理入口依赖 owner 账号登录;" +
			"建议在 /dashboard 设置 admin_key 作为紧急恢复凭证")
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

	// STAGE6:不再打印"火山 TTS 必填项状态"。
	//
	// 那三项(api_key / default_resource_id / default_speaker)自 STAGE5 起就不再由
	// setup 写入,每次启动都会打印三行 ✗,把排障引向"配置坏了"的错误方向 ——
	// 实际上凭证在渠道里,服务是正常的。这里改为打印渠道无关的默认值。
	log.Printf("TTS 默认值: 格式=%s 采样率=%d 超时=%v(凭证由渠道自带,不在此校验)",
		GetDefaultFormat(), GetDefaultSampleRate(), GetTTSTimeout())
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
