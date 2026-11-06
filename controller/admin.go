package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/volcano-tts/tts-api/adapter/provider"
	"github.com/volcano-tts/tts-api/installer"
	"github.com/volcano-tts/tts-api/middleware"
	"github.com/volcano-tts/tts-api/store"
	"github.com/volcano-tts/tts-api/version"
)

// SetAdminStore 注入 admin 控制器需要的 store;main 启动期调一次。
// store 可能在自愈回退后为 nil,GetAdminStore 返 nil 时 controller 应返 503。
var adminStore *store.Store

// SetAdminStore 在 main 启动期调,设置 admin 用的 store 句柄。
func SetAdminStore(s *store.Store) { adminStore = s }

// GetAdminStore admin 控制器用,获取已注入的 store;nil 表示服务在 setup 模式。
func GetAdminStore() *store.Store { return adminStore }

// metricsTextWriter 是 admin 端点写 Prometheus 文本的回调,
// 由 main 启动期注入(避免 controller → metrics → controller 循环)。
type metricsTextWriter func(w http.ResponseWriter) error

var (
	metricsTextWriterMu sync.RWMutex
	metricsTextWriterFn metricsTextWriter
)

// SetMetricsTextWriter 注入 Prometheus 文本写入函数;main 启动期调一次。
func SetMetricsTextWriter(fn metricsTextWriter) {
	metricsTextWriterMu.Lock()
	metricsTextWriterFn = fn
	metricsTextWriterMu.Unlock()
}

// AdminOverviewResponse 是 GET /api/admin/overview 的响应体。
type AdminOverviewResponse struct {
	Mode              string `json:"mode"`
	Installed         bool   `json:"installed"`
	DBPath            string `json:"db_path"`
	LockPath          string `json:"lock_path"`
	Version           string `json:"version"`
	Commit            string `json:"commit"`
	UptimeSeconds     int64  `json:"uptime_seconds"`
	StartTime         string `json:"start_time"`
	VoiceCount        int    `json:"voice_count"`
	VoiceEnabledCount int    `json:"voice_enabled_count"`
	// STAGE5(§7.3 / N5):渠道计数。前端判"要不要显示无渠道引导"必须用
	// **channel_enabled_count**,不能用 channel_count —— 渠道全被禁用时总数仍非零,
	// 而服务实际不可用(调用会 503 no_channel)。
	ChannelCount        int                    `json:"channel_count"`
	ChannelEnabledCount int                    `json:"channel_enabled_count"`
	Memory              map[string]interface{} `json:"memory"`
}

// AdminOverviewHandler GET /api/admin/overview
// 鉴权: RequireAdmin;store 为 nil 时仍可服务,但 voice 字段为 0。
func AdminOverviewHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	resp := AdminOverviewResponse{
		Mode:          installer.GetMode().String(),
		Installed:     installer.GetMode() == installer.ModeNormal,
		Version:       version.Version,
		Commit:        version.Commit,
		StartTime:     startTime.Format(time.RFC3339),
		UptimeSeconds: int64(time.Since(startTime).Seconds()),
		Memory:        collectMemorySnapshot(),
	}

	if s := GetAdminStore(); s != nil {
		if p, err := s.Path(); err == nil {
			resp.DBPath = p
		}
		resp.LockPath = installer.LockPath(resp.DBPath)
		if n, err := s.VoiceCount(); err == nil {
			resp.VoiceCount = n
		}
		if n, err := s.VoiceCountEnabled(); err == nil {
			resp.VoiceEnabledCount = n
		}
		// STAGE5:渠道总数取自 DB;enabled 数取自**内存路由表**(与
		// `/v1/audio/speech` 的 no_channel 判定同源,两处口径必然一致)。
		if n, err := s.ChannelCount(); err == nil {
			resp.ChannelCount = n
		}
		resp.ChannelEnabledCount = CountEnabledChannels(GetRouter().Snapshot())
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("[admin] overview encode failed: %v", err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "encode failed", "server_error", "encode_failed")
	}
}

// AdminMetricsHandler GET /api/admin/metrics
// 鉴权: RequireAdmin;返 Prometheus 文本。
func AdminMetricsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	metricsTextWriterMu.RLock()
	fn := metricsTextWriterFn
	metricsTextWriterMu.RUnlock()
	if fn == nil {
		// 启动期没注入,返 503 + 提示(不应该发生)
		middleware.SendJSONError(w, http.StatusServiceUnavailable,
			"metrics writer not initialized", "configuration_error", "metrics_not_ready")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if err := fn(w); err != nil {
		log.Printf("[admin] metrics write: %v", err)
	}
}

// AdminVoicesListResponse 是 GET /api/voices 的响应。
type AdminVoicesListResponse struct {
	Voices []store.Voice `json:"voices"`
	Total  int           `json:"total"`
}

// AdminVoicesListHandler GET /api/voices
// 鉴权: RequireAdmin;返所有 voice(包含 disabled)。
func AdminVoicesListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := GetAdminStore()
	if s == nil {
		middleware.SendJSONError(w, http.StatusServiceUnavailable, "database not ready", "configuration_error", "db_not_ready")
		return
	}
	vs, err := s.VoiceList(true)
	if err != nil {
		log.Printf("[admin] voice list: %v", err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "list voices failed", "server_error", "db_read_failed")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(AdminVoicesListResponse{Voices: vs, Total: len(vs)})
}

// AdminVoiceCreateRequest 是 POST /api/voices 的 body。
//
// STAGE6:必须带 channel_id(音色归属渠道)。resource_id 改为可选(非火山渠道没有)。
type AdminVoiceCreateRequest struct {
	ChannelID   int64  `json:"channel_id"`
	Name        string `json:"name"`
	Speaker     string `json:"speaker"`
	ResourceID  string `json:"resource_id"`
	Model       string `json:"model"`
	Language    string `json:"language"`
	Description string `json:"description"`
	Enabled     *bool  `json:"enabled,omitempty"`
}

// AdminVoiceCreateHandler POST /api/voices
// 鉴权: RequireAdmin;store nil 时 503。
func AdminVoiceCreateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := GetAdminStore()
	if s == nil {
		middleware.SendJSONError(w, http.StatusServiceUnavailable, "database not ready", "configuration_error", "db_not_ready")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	var body AdminVoiceCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error", "bad_request")
		return
	}

	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}

	// STAGE6:channel_id 必填(G5)。store 侧也会校验,这里提前给出明确 code。
	if body.ChannelID <= 0 {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"channel_id is required", "invalid_request_error", "invalid_channel")
		return
	}
	if _, chErr := s.ChannelGet(body.ChannelID); chErr != nil {
		if errors.Is(chErr, store.ErrNotFound) {
			middleware.SendJSONError(w, http.StatusBadRequest,
				fmt.Sprintf("channel %d does not exist", body.ChannelID),
				"invalid_request_error", "invalid_channel")
			return
		}
		log.Printf("[admin] voice create check channel: %v", chErr)
		middleware.SendJSONError(w, http.StatusInternalServerError, "channel lookup failed", "server_error", "db_read_failed")
		return
	}
	chID := body.ChannelID

	v := store.Voice{
		ChannelID:   &chID,
		Name:        body.Name,
		Speaker:     body.Speaker,
		ResourceID:  body.ResourceID,
		Model:       body.Model,
		Language:    body.Language,
		Description: body.Description,
		Enabled:     enabled,
	}
	id, err := s.VoiceInsert(v)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrDuplicate):
			middleware.SendJSONError(w, http.StatusConflict,
				fmt.Sprintf("voice name %q already exists in this channel", v.Name),
				"invalid_request_error", "voice_duplicate")
		case errors.Is(err, store.ErrInvalid):
			// 客户端输入不合法(name 格式 / speaker 缺失 / channel 不存在):400。
			// channel 相关的 ErrInvalid 优先翻成 invalid_channel,前端据此定位表单字段。
			msg := stripInvalidPrefix(err.Error())
			code := "voice_invalid"
			if strings.Contains(msg, "channel_id") || strings.Contains(msg, "channel") {
				code = "invalid_channel"
			}
			middleware.SendJSONError(w, http.StatusBadRequest, msg, "invalid_request_error", code)
		default:
			// 兜底:DB 连接失败 / Exec 错误 / LastInsertId 失败等都是服务端问题,
			// 返 500 + 通用 message,详细 err 走日志(不直接暴露给前端,避免信息泄漏)。
			log.Printf("[admin] voice insert: %v", err)
			middleware.SendJSONError(w, http.StatusInternalServerError,
				"failed to insert voice", "server_error", "voice_insert_failed")
		}
		return
	}
	created, _ := s.VoiceGet(id)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(created)
}

// AdminVoiceDeleteHandler DELETE /api/voices/{id}
//
// STAGE6(M3):按 **id** 寻址。同名音色可属不同渠道,`{name}` 已无法定位唯一行。
func AdminVoiceDeleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := GetAdminStore()
	if s == nil {
		middleware.SendJSONError(w, http.StatusServiceUnavailable, "database not ready", "configuration_error", "db_not_ready")
		return
	}
	id, ok := parseVoiceIDPath(w, r)
	if !ok {
		return
	}

	// 先取一次:① 404 判断;② 删除成功后回显被删的名字/渠道,便于前端提示
	v, err := s.VoiceGet(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "voice not found", http.StatusNotFound)
			return
		}
		log.Printf("[admin] voice lookup id=%d: %v", id, err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "lookup failed", "server_error", "db_read_failed")
		return
	}

	if err := s.VoiceDelete(id); err != nil {
		// STAGE6:ErrInUse 已退役(全局 default_speaker 已废弃),删除不再被拒。
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "voice not found", http.StatusNotFound)
			return
		}
		log.Printf("[admin] voice delete id=%d: %v", id, err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "delete failed", "server_error", "db_write_failed")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "deleted": v.Name, "id": id})
}

// AdminVoiceToggleRequest 是 PATCH /api/voices/{id}/toggle 的 body。
type AdminVoiceToggleRequest struct {
	Enabled bool `json:"enabled"`
}

// AdminVoiceToggleHandler PATCH /api/voices/{id}/toggle
func AdminVoiceToggleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := GetAdminStore()
	if s == nil {
		middleware.SendJSONError(w, http.StatusServiceUnavailable, "database not ready", "configuration_error", "db_not_ready")
		return
	}
	id, ok := parseVoiceIDPath(w, r)
	if !ok {
		return
	}

	var body AdminVoiceToggleRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error", "bad_request")
		return
	}
	if err := s.VoiceToggleEnabled(id, body.Enabled); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "voice not found", http.StatusNotFound)
			return
		}
		log.Printf("[admin] voice toggle id=%d: %v", id, err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "toggle failed", "server_error", "db_write_failed")
		return
	}
	updated, _ := s.VoiceGet(id)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(updated)
}

// AdminProvidersHandler GET /api/admin/providers —— 返回已注册的上游适配器名。
//
// STAGE6(契约 §8.2):渠道管理页用它渲染 provider 下拉,避免前端硬编码列表
// (新增上游只需在 main.go 加一行空导入,前端自动就能选到)。
//
// 只服务**管理端**(G3):它挂 RequireAdminOrJWT,普通用户 JWT 调会 401 ——
// 所以"门户展示可用 model 列表"不能复用它,那是另一个(user-facing)接口,本期不做。
func AdminProvidersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"providers": provider.Names(),
	})
}

// parseVoiceIDPath 从 path 取并校验 voice id;失败已写好 400 响应,返回 ok=false。
func parseVoiceIDPath(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := mux.Vars(r)["id"]
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		middleware.SendJSONError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid voice id %q", raw), "invalid_request_error", "invalid_id")
		return 0, false
	}
	return id, true
}

// stripInvalidPrefix 把 wrap 后的 ErrInvalid 错误字符串中的 "store: voice invalid: " 前缀剥掉,
// 只保留底层 detail(例如 "speaker is required"),让前端能直接展示用户友好文案。
// 如果上游 wrap 形态变化(没匹配到前缀),原样返回,避免误删关键信息。
func stripInvalidPrefix(s string) string {
	const prefix = "store: voice invalid: "
	if strings.HasPrefix(s, prefix) {
		return s[len(prefix):]
	}
	return s
}
