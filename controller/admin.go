package controller

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/mux"
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
	Mode              string                 `json:"mode"`
	Installed         bool                   `json:"installed"`
	DBPath            string                 `json:"db_path"`
	LockPath          string                 `json:"lock_path"`
	Version           string                 `json:"version"`
	Commit            string                 `json:"commit"`
	UptimeSeconds     int64                  `json:"uptime_seconds"`
	StartTime         string                 `json:"start_time"`
	VoiceCount        int                    `json:"voice_count"`
	VoiceEnabledCount int                    `json:"voice_enabled_count"`
	Memory            map[string]interface{} `json:"memory"`
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
type AdminVoiceCreateRequest struct {
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

	v := store.Voice{
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
		switch err {
		case store.ErrDuplicate:
			middleware.SendJSONError(w, http.StatusConflict,
				fmt.Sprintf("voice name %q already exists", v.Name),
				"invalid_request_error", "voice_duplicate")
		default:
			log.Printf("[admin] voice insert: %v", err)
			middleware.SendJSONError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "voice_invalid")
		}
		return
	}
	created, _ := s.VoiceGet(id)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(created)
}

// AdminVoiceDeleteHandler DELETE /api/voices/{name}
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
	name := mux.Vars(r)["name"]
	if name == "" {
		middleware.SendJSONError(w, http.StatusBadRequest, "missing voice name", "invalid_request_error", "bad_request")
		return
	}

	v, err := s.VoiceGetByName(name)
	if err != nil {
		if err == store.ErrNotFound {
			http.Error(w, "voice not found", http.StatusNotFound)
			return
		}
		log.Printf("[admin] voice lookup: %v", err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "lookup failed", "server_error", "db_read_failed")
		return
	}
	if err := s.VoiceDelete(v.ID); err != nil {
		switch err {
		case store.ErrInUse:
			middleware.SendJSONError(w, http.StatusConflict,
				fmt.Sprintf("voice %q is referenced by default_speaker; remove the default first", name),
				"invalid_request_error", "voice_in_use")
		case store.ErrNotFound:
			http.Error(w, "voice not found", http.StatusNotFound)
		default:
			log.Printf("[admin] voice delete: %v", err)
			middleware.SendJSONError(w, http.StatusInternalServerError, "delete failed", "server_error", "db_write_failed")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "deleted": name})
}

// AdminVoiceToggleRequest 是 PATCH /api/voices/{name}/toggle 的 body。
type AdminVoiceToggleRequest struct {
	Enabled bool `json:"enabled"`
}

// AdminVoiceToggleHandler PATCH /api/voices/{name}/toggle
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
	name := mux.Vars(r)["name"]
	if name == "" {
		middleware.SendJSONError(w, http.StatusBadRequest, "missing voice name", "invalid_request_error", "bad_request")
		return
	}
	v, err := s.VoiceGetByName(name)
	if err != nil {
		if err == store.ErrNotFound {
			http.Error(w, "voice not found", http.StatusNotFound)
			return
		}
		log.Printf("[admin] voice lookup: %v", err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "lookup failed", "server_error", "db_read_failed")
		return
	}
	var body AdminVoiceToggleRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error", "bad_request")
		return
	}
	if err := s.VoiceToggleEnabled(v.ID, body.Enabled); err != nil {
		log.Printf("[admin] voice toggle: %v", err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "toggle failed", "server_error", "db_write_failed")
		return
	}
	updated, _ := s.VoiceGet(v.ID)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(updated)
}
