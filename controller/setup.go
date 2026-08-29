package controller

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/volcano-tts/tts-api/common"
	"github.com/volcano-tts/tts-api/installer"
	"github.com/volcano-tts/tts-api/middleware"
	"github.com/volcano-tts/tts-api/setting"
	"github.com/volcano-tts/tts-api/store"
)

// SetupAPIState 是 setup 控制器需要的状态:
//   - Store:  db 访问,可能为 nil(自愈回退后 store 已关闭,等待重新 setup)
//   - DBPath: 用于安装完成时写 lock
type SetupAPIState struct {
	Store  *store.Store
	DBPath string
	Token  string
}

// 全局 setup 状态,在 main.go 启动时通过 SetSetupState 注入。
// 进程内只有一个二进制实例,全局变量是合适的。
var setupState SetupAPIState

// SetSetupState 注入 setup 控制器所需的 store + dbPath,启动期调用一次。
func SetSetupState(s *store.Store, dbPath string) {
	setupState.Store = s
	setupState.DBPath = dbPath
}

// GetSetupStore 供 router/main 注入的 store 访问函数。
func GetSetupStore() *store.Store { return setupState.Store }

// GetSetupDBPath 供 router/main 注入的 dbPath 访问函数。
func GetSetupDBPath() string { return setupState.DBPath }

// SetupStatusHandler GET /api/setup/status
// 始终返回当前模式,无论安装与否;用于部署探针 + 引导页判断。
func SetupStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	resp := map[string]any{
		"installed": installer.GetMode() == installer.ModeNormal,
		"mode":      installer.GetMode().String(),
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// SetupPrefillHandler GET /api/setup/prefill
// 仅在安装模式有响应;返回旧 env 变量值,便于引导页预填,实现平滑迁移。
func SetupPrefillHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if installer.GetMode() != installer.ModeSetup {
		http.Error(w, "not in setup mode", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	resp := map[string]any{
		"settings": prefillFromEnv(),
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// prefillFromEnv 读 BYTEDANCE_TTS_* 等旧 env,作为引导页预填值。
// 读不到就返回空串,前端会用默认值。
func prefillFromEnv() map[string]string {
	get := func(k string) string { return os.Getenv(k) }
	return map[string]string{
		"api_key":            "", // API key 永不回显,即便 env 里有;必须让用户重新输入
		"default_resource_id": get("BYTEDANCE_TTS_RESOURCE_ID"),
		"default_speaker":    get("BYTEDANCE_TTS_SPEAKER"),
		"default_format":     get("BYTEDANCE_TTS_FORMAT"),
		"sample_rate":        get("BYTEDANCE_TTS_SAMPLE_RATE"),
		"model":              get("BYTEDANCE_TTS_MODEL"),
		"model_type":         get("BYTEDANCE_TTS_MODEL_TYPE"),
		"explicit_language":  get("BYTEDANCE_TTS_EXPLICIT_LANGUAGE"),
		"enable_subtitle":    get("BYTEDANCE_TTS_ENABLE_SUBTITLE"),
		"timeout":            get("BYTEDANCE_TTS_TIMEOUT"),
	}
}

// SetupRequestBody 是 POST /api/setup 的请求体结构。
type SetupRequestBody struct {
	Token    string            `json:"token"`
	Settings map[string]string `json:"settings"`
	Voices   []SetupVoice      `json:"voices"`
}

// SetupVoice 是 POST /api/setup 里 voices 数组的条目。
type SetupVoice struct {
	Name       string `json:"name"`
	Speaker    string `json:"speaker"`
	ResourceID string `json:"resource_id"`
	Model      string `json:"model"`
	Language   string `json:"language"`
}

// SetupSubmitHandler POST /api/setup
// 校验 token → 校验字段 → 写 settings → 写 voices → 写 lock。
// 必须在安装模式才接受;装完后永久 404。
func SetupSubmitHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 安装完成后此端点永久关闭(防止被误触)
	if installer.GetMode() != installer.ModeSetup {
		http.NotFound(w, r)
		return
	}

	// 解析 body
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB
	var body SetupRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error", "bad_request")
		return
	}

	// token 校验(常量时间比较防计时攻击)
	if setting.SetupToken == "" || !secureEqualString(body.Token, setting.SetupToken) {
		log.Printf("[setup] token 校验失败 - 客户端=%s", middleware.GetClientIP(r))
		middleware.SendJSONError(w, http.StatusUnauthorized, "invalid setup token", "authentication_error", "invalid_token")
		return
	}

	// 校验 settings 必填项
	if err := validateSetupSettings(body.Settings); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "missing_field")
		return
	}

	// 校验 voices
	if err := validateSetupVoices(body.Voices); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_voice")
		return
	}

	// 取 store:必须为非 nil(自愈回退后 store 是 nil,这种状态下不接 setup,要求重启)
	s := GetSetupStore()
	if s == nil {
		middleware.SendJSONError(w, http.StatusServiceUnavailable, "database not ready, please restart service", "configuration_error", "db_not_ready")
		return
	}

	// 写 settings(包含 initialized=1)
	settingsKV := make(map[string]string, len(body.Settings)+1)
	for k, v := range body.Settings {
		settingsKV[k] = v
	}
	settingsKV["initialized"] = "1"
	settingsKV["installed_at"] = time.Now().UTC().Format(time.RFC3339)
	if err := s.SettingsSetBatch(settingsKV); err != nil {
		log.Printf("[setup] 写 settings 失败: %v", err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "failed to write settings", "server_error", "db_write_failed")
		return
	}

	// 立即把 auth_key 灌到 setting.Auth.APIKeys,这样后续 /v1/audio/speech 和 /admin
	// 在本进程内能立刻用新 key(无需等 LoadRuntimeConfig)。
	authKey := strings.TrimSpace(body.Settings["auth_key"])
	if authKey != "" {
		setting.Auth.APIKeys = []string{authKey}
	}

	// 清空旧 voices 再插入(假设是首次安装;若不是,name 冲突会变成 409)
	// 这里选择 "清空+插入" 语义,符合"setup 是首次安装"的产品定位
	// 如果想保留旧 voices,可以改成 UPSERT,但 M1 不做
	if existing, _ := s.VoiceList(true); len(existing) > 0 {
		// 留作未来:如果是非首次 setup(M2 加 reset 功能),这里需要更精细处理
		log.Printf("[setup] 检测到 %d 条已存在 voices,本次将跳过清空(name 冲突由 ErrDuplicate 处理)", len(existing))
	}
	inserted := 0
	for _, v := range body.Voices {
		_, err := s.VoiceInsert(store.Voice{
			Name:       v.Name,
			Speaker:    v.Speaker,
			ResourceID: v.ResourceID,
			Model:      v.Model,
			Language:   v.Language,
			Enabled:    true,
		})
		if err != nil {
			log.Printf("[setup] 插入 voice %q 失败: %v", v.Name, err)
			// 不回滚 settings(用户重启后会重新 setup)
			// 但已插入的 voices 会留着,下次 setup 会撞 ErrDuplicate
			// 安全:把 ErrDuplicate 视作可继续(用户重复 setup 同一组 voice)
			if err == store.ErrDuplicate {
				continue
			}
			middleware.SendJSONError(w, http.StatusInternalServerError,
				fmt.Sprintf("failed to insert voice %q: %v", v.Name, err),
				"server_error", "voice_insert_failed")
			return
		}
		inserted++
	}
	log.Printf("[setup] 写入 settings=%d, voices=%d/%d", len(settingsKV), inserted, len(body.Voices))

	// 写 lock(原子):从这一刻起,/api/setup 永久关闭
	if err := installer.CreateLock(GetSetupDBPath()); err != nil {
		log.Printf("[setup] 写 lock 失败: %v", err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "failed to create install lock", "server_error", "lock_write_failed")
		return
	}
	// 切到正常模式(本进程内)
	installer.SetMode(installer.ModeNormal)
	log.Printf("[setup] 安装完成!后续请求将进入正常模式")

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":       true,
		"message":  "installed",
		"redirect": "/admin",
		"settings": len(settingsKV),
		"voices":   inserted,
	})
}

// validateSetupSettings 校验必填项。
// auth_key (鉴权) 也是必填 — 让 setup 成为"单一配置入口",
// 用户装完不用再回去设 OPENAI_TTS_API_KEY env。
func validateSetupSettings(m map[string]string) error {
	required := []string{"api_key", "auth_key", "default_resource_id", "default_speaker"}
	var missing []string
	for _, k := range required {
		if strings.TrimSpace(m[k]) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required fields: %v", missing)
	}
	return nil
}

// validateSetupVoices 校验音色列表;至少 1 条。
// 详细合法性(白名单、speaker 非空)由 store.VoiceInsert 负责。
func validateSetupVoices(vs []SetupVoice) error {
	if len(vs) == 0 {
		return fmt.Errorf("at least one voice is required")
	}
	names := make(map[string]struct{}, len(vs))
	for i, v := range vs {
		if strings.TrimSpace(v.Name) == "" {
			return fmt.Errorf("voices[%d]: name is required", i)
		}
		if strings.TrimSpace(v.Speaker) == "" {
			return fmt.Errorf("voices[%d] (%s): speaker is required", i, v.Name)
		}
		if strings.TrimSpace(v.ResourceID) == "" {
			return fmt.Errorf("voices[%d] (%s): resource_id is required", i, v.Name)
		}
		if _, dup := names[v.Name]; dup {
			return fmt.Errorf("voices[%d]: duplicate name %q", i, v.Name)
		}
		names[v.Name] = struct{}{}
	}
	return nil
}

// secureEqualString wraps common.SecureEqualString 保持向后兼容(原文件内已有调用)。
func secureEqualString(a, b string) bool { return common.SecureEqualString(a, b) }
