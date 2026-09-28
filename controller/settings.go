package controller

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/volcano-tts/tts-api/middleware"
	"github.com/volcano-tts/tts-api/setting"
)

// SettingsResponse 是 GET /api/settings 的响应。
// API key 永远打码(借用 setting.maskAPIKey 风格,前 4 后 4 中间 ****)。
type SettingsResponse struct {
	APIKey           string `json:"api_key"`           // 打码形式,例如 S_G8****naJ1
	APIKeySet        bool   `json:"api_key_set"`       // 是否已设置(用于前端判断要不要提示必填)
	AuthKey          string `json:"auth_key"`          // 鉴权 key 打码(客户端访问 + admin 登录用)
	AuthKeySet       bool   `json:"auth_key_set"`
	CORSAllowAll     bool   `json:"cors_allow_all"`    // 允许所有来源(*)
	CORSOrigins      string `json:"cors_origins"`      // 逗号分隔的白名单(原文,含大小写,trim 末尾 /)
	CORSConfigured   bool   `json:"cors_configured"`   // 是否配了 CORS(给 banner 用)
	DefaultResourceID string `json:"default_resource_id"`
	DefaultSpeaker   string `json:"default_speaker"`
	DefaultFormat    string `json:"default_format"`
	SampleRate       int    `json:"sample_rate"`
	Model            string `json:"model"`
	ModelType        int    `json:"model_type"`
	ExplicitLanguage string `json:"explicit_language"`
	EnableSubtitle   bool   `json:"enable_subtitle"`
	UpdatedAt        string `json:"updated_at"` // RFC3339,来自 settings.installed_at(沿用)
}

// SettingsGetHandler GET /api/settings
// 鉴权: RequireAdmin;store nil 时 503。
func SettingsGetHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := GetAdminStore()
	if s == nil {
		middleware.SendJSONError(w, http.StatusServiceUnavailable, "database not ready", "configuration_error", "db_not_ready")
		return
	}
	all, err := s.SettingsGetAll()
	if err != nil {
		log.Printf("[settings] getall: %v", err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "read settings failed", "server_error", "db_read_failed")
		return
	}

	resp := SettingsResponse{
		APIKey:            maskAPIKeyField(all["api_key"]),
		APIKeySet:         all["api_key"] != "",
		AuthKey:           maskAPIKeyField(all["auth_key"]),
		AuthKeySet:        all["auth_key"] != "",
		CORSAllowAll:      all["cors_allow_all"] == "1" || all["cors_allow_all"] == "true",
		CORSOrigins:       all["cors_origins"],
		CORSConfigured:    all["cors_allow_all"] == "1" || all["cors_allow_all"] == "true" || all["cors_origins"] != "",
		DefaultResourceID: all["default_resource_id"],
		DefaultSpeaker:    all["default_speaker"],
		DefaultFormat:     all["default_format"],
		Model:             all["model"],
		ExplicitLanguage:  all["explicit_language"],
	}
	if v, _ := s.SettingsGetInt("sample_rate", 0); v > 0 {
		resp.SampleRate = v
	}
	if v, _ := s.SettingsGetInt("model_type", 0); v > 0 {
		resp.ModelType = v
	}
	if v, _ := s.SettingsGetBool("enable_subtitle", false); v {
		resp.EnableSubtitle = true
	}
	if ts := all["installed_at"]; ts != "" {
		resp.UpdatedAt = ts
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

// SettingsUpdateRequest 是 PUT /api/settings 的 body(部分更新)。
// 字段都是可选;只更新非空 / 非零值。
type SettingsUpdateRequest struct {
	APIKey            *string `json:"api_key,omitempty"` // 用指针区分 "未传" vs "传空串"
	DefaultResourceID *string `json:"default_resource_id,omitempty"`
	DefaultSpeaker    *string `json:"default_speaker,omitempty"`
	DefaultFormat     *string `json:"default_format,omitempty"`
	SampleRate        *int    `json:"sample_rate,omitempty"`
	Model             *string `json:"model,omitempty"`
	ModelType         *int    `json:"model_type,omitempty"`
	ExplicitLanguage  *string `json:"explicit_language,omitempty"`
	EnableSubtitle    *bool   `json:"enable_subtitle,omitempty"`
}

// SettingsUpdateHandler PUT /api/settings
// 鉴权: RequireAdmin;store nil 时 503。
// 至少要改 1 个字段(空 body 返 400);api_key 修改走专用端点 /api/settings/api-key。
func SettingsUpdateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := GetAdminStore()
	if s == nil {
		middleware.SendJSONError(w, http.StatusServiceUnavailable, "database not ready", "configuration_error", "db_not_ready")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<16) // 64KB
	var body SettingsUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error", "bad_request")
		return
	}

	// 收集要更新的键值对
	updates := make(map[string]string)
	if body.DefaultResourceID != nil {
		v := strings.TrimSpace(*body.DefaultResourceID)
		if v == "" {
			middleware.SendJSONError(w, http.StatusBadRequest, "default_resource_id cannot be empty", "invalid_request_error", "missing_field")
			return
		}
		updates["default_resource_id"] = v
	}
	if body.DefaultSpeaker != nil {
		v := strings.TrimSpace(*body.DefaultSpeaker)
		if v == "" {
			middleware.SendJSONError(w, http.StatusBadRequest, "default_speaker cannot be empty", "invalid_request_error", "missing_field")
			return
		}
		// 校验音色在库中(避免 default_speaker 引用不存在的 voice)
		if _, err := s.VoiceGetByName(v); err != nil {
			middleware.SendJSONError(w, http.StatusBadRequest,
				fmt.Sprintf("default_speaker %q not found in voices table", v),
				"invalid_request_error", "default_speaker_missing")
			return
		}
		updates["default_speaker"] = v
	}
	if body.DefaultFormat != nil {
		v := strings.TrimSpace(*body.DefaultFormat)
		if !isValidFormat(v) {
			middleware.SendJSONError(w, http.StatusBadRequest,
				fmt.Sprintf("default_format %q invalid; valid: mp3/wav/opus/pcm/aac/flac", v),
				"invalid_request_error", "format_invalid")
			return
		}
		updates["default_format"] = v
	}
	if body.SampleRate != nil {
		v := *body.SampleRate
		if v < 8000 || v > 48000 {
			middleware.SendJSONError(w, http.StatusBadRequest,
				"sample_rate must be 8000-48000", "invalid_request_error", "sample_rate_invalid")
			return
		}
		updates["sample_rate"] = strconv.Itoa(v)
	}
	if body.Model != nil {
		updates["model"] = strings.TrimSpace(*body.Model)
	}
	if body.ModelType != nil {
		updates["model_type"] = strconv.Itoa(*body.ModelType)
	}
	if body.ExplicitLanguage != nil {
		updates["explicit_language"] = strings.TrimSpace(*body.ExplicitLanguage)
	}
	if body.EnableSubtitle != nil {
		updates["enable_subtitle"] = boolToStr(*body.EnableSubtitle)
	}

	if len(updates) == 0 {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"at least one field is required (use /api/settings/api-key to change api_key)",
			"invalid_request_error", "no_fields")
		return
	}

	// 写入 DB
	if err := s.SettingsSetBatch(updates); err != nil {
		log.Printf("[settings] update: %v", err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "write settings failed", "server_error", "db_write_failed")
		return
	}

	// 关键: 更新后**立刻刷新运行时缓存**,M3 要求"改设置实时生效"
	if err := setting.LoadRuntimeConfig(s); err != nil {
		log.Printf("[settings] reload runtime: %v", err)
		// 不返 500:DB 已写,只是 reload 失败;下次启动会生效
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"settings saved but runtime reload failed; restart required",
			"server_error", "reload_failed")
		return
	}

	log.Printf("[settings] updated %d fields, runtime reloaded", len(updates))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":         true,
		"updated":    len(updates),
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	})
}

// SettingsAPIKeyRequest 是 PUT /api/settings/api-key 的 body。
type SettingsAPIKeyRequest struct {
	APIKey string `json:"api_key"`
}

// SettingsAPIKeyHandler PUT /api/settings/api-key
// 鉴权: RequireAdmin。专门改 api_key,因为它需要单独的安全处理(不能 mask,要走加密通道)。
func SettingsAPIKeyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := GetAdminStore()
	if s == nil {
		middleware.SendJSONError(w, http.StatusServiceUnavailable, "database not ready", "configuration_error", "db_not_ready")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1KB
	var body SettingsAPIKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error", "bad_request")
		return
	}
	key := strings.TrimSpace(body.APIKey)
	if key == "" {
		middleware.SendJSONError(w, http.StatusBadRequest, "api_key cannot be empty", "invalid_request_error", "missing_field")
		return
	}

	if err := s.SettingsSet("api_key", key); err != nil {
		log.Printf("[settings] api-key set: %v", err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "write api_key failed", "server_error", "db_write_failed")
		return
	}
	if err := setting.LoadRuntimeConfig(s); err != nil {
		log.Printf("[settings] reload after api-key: %v", err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"api_key saved but runtime reload failed", "server_error", "reload_failed")
		return
	}
	log.Printf("[settings] api_key updated, runtime reloaded")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// SettingsAuthKeyRequest 是 PUT /api/settings/auth-key 的 body。
// auth_key 是 admin 鉴权和 /v1/audio/speech 鉴权用的 key(火山上游 key 是 api_key,这是两套)。
type SettingsAuthKeyRequest struct {
	AuthKey string `json:"auth_key"`
}

// SettingsAuthKeyHandler PUT /api/settings/auth-key
// 鉴权: RequireAdmin。改完立即刷新鉴权 key 列表(setting.SetAuthAPIKeys,进程内生效),
// 下一个请求就用新 key — admin 自己改完要等下一次请求才能验证(避免改完立刻自踢)。
func SettingsAuthKeyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := GetAdminStore()
	if s == nil {
		middleware.SendJSONError(w, http.StatusServiceUnavailable, "database not ready", "configuration_error", "db_not_ready")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var body SettingsAuthKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error", "bad_request")
		return
	}
	key := strings.TrimSpace(body.AuthKey)
	if key == "" {
		middleware.SendJSONError(w, http.StatusBadRequest, "auth_key cannot be empty", "invalid_request_error", "missing_field")
		return
	}
	if err := s.SettingsSet("auth_key", key); err != nil {
		log.Printf("[settings] auth-key set: %v", err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "write auth_key failed", "server_error", "db_write_failed")
		return
	}
	// 立即生效:不重新 LoadRuntimeConfig(那会覆盖其它字段),
	// 只单独刷新 Auth.APIKeys
	setting.SetAuthAPIKeys([]string{key})
	log.Printf("[settings] auth_key updated, runtime active (next request uses new key)")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// SettingsCORSRequest 是 PUT /api/settings/cors 的 body。
// 两个字段都可选(至少给一个),用指针区分"未传"和"传空串":
//   - allow_all 指针: nil=未传(不动)  *true=开 *false=关
//   - origins  字符串: nil=未传(不动)  ""=传空串(清空)  "url1\nurl2"=覆盖
// 这样用户能精确表达意图(保留 / 改 / 清空),不会被 0/"" 歧义坑死。
type SettingsCORSRequest struct {
	AllowAll *bool   `json:"allow_all,omitempty"`
	Origins  *string `json:"origins,omitempty"` // *string 区分"未传(nil)"和"传空串"
}

// SettingsCORSHandler PUT /api/settings/cors
// 鉴权: RequireAdmin。改完立即刷新 CORS(setting.SetCORS,进程内生效,跨域请求从下个请求开始按新配置)。
// 同源豁免由 middleware/cors.go 的 isSameOrigin 处理,不在这里管。
func SettingsCORSHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := GetAdminStore()
	if s == nil {
		middleware.SendJSONError(w, http.StatusServiceUnavailable, "database not ready", "configuration_error", "db_not_ready")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var body SettingsCORSRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error", "bad_request")
		return
	}
	// 至少要给一个字段(allow_all 或 origins)
	// 指针为 nil 表示"未传",不计入
	if body.AllowAll == nil && body.Origins == nil {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"at least one of allow_all / origins required",
			"invalid_request_error", "no_fields")
		return
	}

	updates := map[string]string{}
	if body.AllowAll != nil {
		updates["cors_allow_all"] = boolToStr(*body.AllowAll)
	}
	if body.Origins != nil {
		// *Origins == "" 表示用户要清空(保留 nil 表示"不动")
		origins := *body.Origins
		if origins != "" {
			// 校验每个 origin 至少像 http(s)://... (防止用户填乱字符)
			for _, line := range strings.Split(origins, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				low := strings.ToLower(line)
				if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") {
					middleware.SendJSONError(w, http.StatusBadRequest,
						fmt.Sprintf("invalid origin: %q (must start with http:// or https://)", line),
						"invalid_request_error", "origin_invalid")
					return
				}
			}
		}
		// 空串也能存(表示"清空");trim/lower 在 LoadRuntimeConfig 那侧做
		updates["cors_origins"] = origins
	}
	if err := s.SettingsSetBatch(updates); err != nil {
		log.Printf("[settings] cors set: %v", err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "write cors failed", "server_error", "db_write_failed")
		return
	}

	// 立即刷新 CORS(setting.SetCORS),跨域请求从下个请求开始按新配置生效
	// 复用 LoadRuntimeConfig 的解析逻辑(只取 cors 部分,避免覆盖其它运行时字段)
	corsAllowAll, _ := s.SettingsGetBool("cors_allow_all", false)
	originsStr := ""
	if v, _, _ := s.SettingsGet("cors_origins"); v != "" {
		originsStr = v
	}
	if corsAllowAll {
		setting.SetCORS(true, nil)
	} else if originsStr != "" {
		setting.SetCORS(false, setting.SplitOriginsForCORS(originsStr))
	} else {
		setting.SetCORS(false, nil)
	}
	log.Printf("[settings] cors updated (allow_all=%v origins=%q), runtime active", setting.GetCORSAllowAll(), originsStr)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":           true,
		"allow_all":    setting.GetCORSAllowAll(),
		"origins":      originsStr,
		"cors_active":  true,
	})
}

// maskAPIKeyField 复用 setting 包的打码风格(前 4 后 4 中间 ****)。
// 单独导出版本避免从 setting 包拉整个 APIKeyMask 之类的工具(那个是 unexported)。
func maskAPIKeyField(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 4 {
		return "****"
	}
	// 仿 setting.maskAPIKey: 但这里打的是 TTS 服务用的 key,可能含字母数字和连字符
	return s[:4] + "****" + s[len(s)-4:]
}

func isValidFormat(s string) bool {
	switch s {
	case "mp3", "wav", "opus", "pcm", "aac", "flac", "":
		return true
	}
	return false
}

func boolToStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
