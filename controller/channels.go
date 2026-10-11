// controller/channels.go — 阶段 2 渠道管理 HTTP 接口
//
// 全部挂 /api/admin/channels*,由 router.Setup() 统一套 RequireAdmin 中间件;
// 本文件只关心 handler 业务逻辑,不做鉴权。
//
// 关键设计决策(见 阶段2渠道管理(12条决策已固化).md):
//   - #1  routerPtr 用 atomic.Pointer;SetRouter 恒收非 nil
//   - #2  PATCH 指针语义(非凭据字段 *T,nil=不改;credentials 缺省=不改)
//   - #3  PATCH name 重名 → ErrChannelDuplicate → 409
//   - #4  凭据 DB 列不变(字符串),controller marshal/unmarshal 对象 ↔ 字符串
//   - #5  POST/PATCH 入库前先 FromStoreChannel 校验凭据,保证 Reload 一定过
//   - #7  测活 /test 用 setting.GetTTSRequest() 默认 voice 模板 + 真合成 + [test-only] 日志
//   - #8  GET 列表/详情 credentials 脱敏(api_key → "***");编辑 api_key 留空=不改
//   - #9  401 鉴权失败 code=admin_required(由 denyAdmin 改,见 middleware/admin_auth.go)
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/volcano-tts/tts-api/adapter/provider"
	"github.com/volcano-tts/tts-api/adapter/route"
	"github.com/volcano-tts/tts-api/dto"
	"github.com/volcano-tts/tts-api/middleware"
	"github.com/volcano-tts/tts-api/setting"
	"github.com/volcano-tts/tts-api/store"
)

// ---- DTO(本文件专用) -----------------------------------------------

// channelView 是 store.Channel 的对外视图(列表/详情回显时 credentials 脱敏)。
// 与 store.Channel 同名字段用 json tag 重命名,避免和 store 包内部结构混淆。
type channelView struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	Provider        string `json:"provider"`
	CredentialsJSON string `json:"credentials_json"` // 已脱敏(列表/详情)
	Voices          string `json:"voices"`
	Priority        int    `json:"priority"`
	Weight          int    `json:"weight"`
	Status          int    `json:"status"`
	AutoBan         bool   `json:"auto_ban"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

// channelCreateReq 是 POST /api/admin/channels 的请求体。
// Credentials 用 *Credentials 指针以便区分"未提供"和"空对象"(决策 #2 简化版:
// 决策 #2 说"credentials 缺省=不改",这里 POST 场景下 credentials 必填,
// 所以用值类型即可;但 nil 指针仍可表达"客户端没传 credentials 字段"的语义)。
type channelCreateReq struct {
	Name        string                `json:"name"`
	Provider    string                `json:"provider"`
	Credentials *provider.Credentials `json:"credentials"`
	Voices      string                `json:"voices"`
	Priority    int                   `json:"priority"`
	Weight      int                   `json:"weight"`
	Status      int                   `json:"status"`
}

// channelUpdateReq 是 PATCH /api/admin/channels/{id} 的请求体。
// 所有字段都是指针:nil=不改,非 nil=改成这个值(决策 #2 指针语义)。
type channelUpdateReq struct {
	Name            *string                `json:"name"`
	Credentials     *provider.Credentials  `json:"credentials"` // 整体替换(缺省=不改)
	Voices          *string                `json:"voices"`
	Priority        *int                   `json:"priority"`
	Weight          *int                   `json:"weight"`
	Status          *int                   `json:"status"`
	AutoBan         *bool                  `json:"auto_ban"`
}

type channelStatusReq struct {
	Status *int `json:"status"` // 1=enabled, 2=disabled
}

// channelTestResp 是 POST /{id}/test 的响应。
type channelTestResp struct {
	OK         bool   `json:"ok"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

// ---- 凭据转换(决策 #4) -------------------------------------------

// marshalCredentials 把 provider.Credentials 对象序列化成 JSON 字符串(入库格式)。
// 决策 #4:DB 列存字符串(controller 转换,store 不感知)。
func marshalCredentials(c provider.Credentials) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// unmarshalCredentials 反序列化(回显校验用)。
func unmarshalCredentials(raw string) (provider.Credentials, error) {
	var c provider.Credentials
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return c, err
	}
	return c, nil
}

// maskCredentialsJson 脱敏:反序列化 → APIKey 替换为 "***" → 重序列化。
// 反序列化失败(脏数据)时原样返 raw,不阻断 GET(回显是次要,主流程能跑就行)。
func maskCredentialsJson(raw string) string {
	c, err := unmarshalCredentials(raw)
	if err != nil {
		return raw
	}
	if c.APIKey != "" {
		c.APIKey = "***"
	}
	masked, err := marshalCredentials(c)
	if err != nil {
		return raw
	}
	return masked
}

// ---- Reload(决策 #5) -----------------------------------------------

// loadChannelsFromStoreStrict 严格版:任一条 FromStoreChannel 错即整体 fail。
// 仅 main 启动期调用,目的是 fail-fast,避免带着脏数据跑起来。
//
// 导出(LoadChannelsFromStoreStrict)供 main 启动期复用;运行期 Reload
// 走宽松版 reloadChannels(本文件内),决策 B 故意不抽共用。
func LoadChannelsFromStoreStrict() ([]route.Channel, error) {
	s := GetAdminStore()
	if s == nil {
		return nil, errors.New("store not initialized")
	}
	chs, err := s.ChannelList(true) // 全量(含 disabled)
	if err != nil {
		return nil, fmt.Errorf("load channels: %w", err)
	}
	out := make([]route.Channel, 0, len(chs))
	for _, sc := range chs {
		rc, convErr := route.FromStoreChannel(sc)
		if convErr != nil {
			return nil, fmt.Errorf("convert channel id=%d name=%s: %w", sc.ID, sc.Name, convErr)
		}
		out = append(out, rc)
	}
	return out, nil
}

// reloadChannels 宽松版:某条脏数据 → 跳过 + log,不整体崩。
// 写操作(POST/PATCH/DELETE/status)成功后调,触发 router 热更新。
func reloadChannels() {
	s := GetAdminStore()
	if s == nil {
		log.Printf("[channels] reload skipped: store not initialized")
		return
	}
	chs, err := s.ChannelList(true)
	if err != nil {
		log.Printf("[channels] reload: list failed (DB err, will retry on next write): %v", err)
		return
	}
	out := make([]route.Channel, 0, len(chs))
	for _, sc := range chs {
		rc, convErr := route.FromStoreChannel(sc)
		if convErr != nil {
			// 决策 #5 的兜底:防御历史脏数据/外部直改 DB;跳过单条,不整体崩
			log.Printf("[channels] reload: skip corrupted channel id=%d name=%s: %v",
				sc.ID, sc.Name, convErr)
			continue
		}
		out = append(out, rc)
	}
	SetRouter(route.NewRouter(out))
	log.Printf("[channels] router reloaded: total=%d enabled=%d", len(out), CountEnabledChannels(out))
}

// CountEnabledChannels 统计启用 channel 数;main 启动摘要用。
// 导出供 main 复用(决策 B 启动期不抽共用,但统计是简单 helper,导出比
// 重复实现干净)。
func CountEnabledChannels(chs []route.Channel) int {
	n := 0
	for _, c := range chs {
		if c.Enabled() {
			n++
		}
	}
	return n
}

// ---- helpers ------------------------------------------------------

// viewFromStore 把 store.Channel 转成对外视图,可选脱敏。
func viewFromStore(c store.Channel, mask bool) channelView {
	creds := c.CredentialsJSON
	if mask {
		creds = maskCredentialsJson(creds)
	}
	return channelView{
		ID:              c.ID,
		Name:            c.Name,
		Provider:        c.Provider,
		CredentialsJSON: creds,
		Voices:          c.Voices,
		Priority:        c.Priority,
		Weight:          c.Weight,
		Status:          c.Status,
		AutoBan:         c.AutoBan,
		CreatedAt:       c.CreatedAt,
		UpdatedAt:       c.UpdatedAt,
	}
}

// requireStoreDB 拿 store,拿不到返 503。写操作前必调。
func requireStoreDB(w http.ResponseWriter) *store.Store {
	s := GetAdminStore()
	if s == nil {
		middleware.SendJSONError(w, http.StatusServiceUnavailable,
			"database not ready", "configuration_error", "db_not_ready")
		return nil
	}
	return s
}

// parseIDFromPath 解析 mux.Vars 的 {id} 段;非法 → 写 400 + 返 0。
func parseIDFromPath(w http.ResponseWriter, r *http.Request) int64 {
	v := mux.Vars(r)
	raw, ok := v["id"]
	if !ok {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"missing id", "invalid_request_error", "bad_request")
		return 0
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"invalid id", "invalid_request_error", "bad_request")
		return 0
	}
	return id
}

// ---- handlers -----------------------------------------------------

// ChannelsListHandler GET /api/admin/channels
func ChannelsListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}
	chs, err := s.ChannelList(true) // 含 disabled
	if err != nil {
		log.Printf("[channels] list: %v", err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"list channels failed", "server_error", "db_read_failed")
		return
	}
	views := make([]channelView, 0, len(chs))
	for _, c := range chs {
		views = append(views, viewFromStore(c, true)) // 脱敏
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"channels": views, "total": len(views)})
}

// ChannelsGetHandler GET /api/admin/channels/{id}
func ChannelsGetHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}
	id := parseIDFromPath(w, r)
	if id == 0 {
		return
	}
	c, err := s.ChannelGet(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "channel not found", http.StatusNotFound)
			return
		}
		log.Printf("[channels] get id=%d: %v", id, err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"get channel failed", "server_error", "db_read_failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(viewFromStore(*c, true)) // 脱敏
}

// ChannelsCreateHandler POST /api/admin/channels
// 决策 #5:入库前先 FromStoreChannel 校验凭据,失败 400 不入库。
func ChannelsCreateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}
	var req channelCreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"invalid JSON body", "invalid_request_error", "bad_request")
		return
	}

	// 必填校验
	req.Name = strings.TrimSpace(req.Name)
	req.Provider = strings.TrimSpace(req.Provider)
	if req.Name == "" {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"name is required", "invalid_request_error", "missing_name")
		return
	}
	if req.Provider == "" {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"provider is required", "invalid_request_error", "missing_provider")
		return
	}
	if req.Credentials == nil {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"credentials is required", "invalid_request_error", "missing_credentials")
		return
	}
	credsJSON, err := marshalCredentials(*req.Credentials)
	if err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"credentials marshal failed: "+err.Error(), "invalid_request_error", "bad_credentials")
		return
	}
	// 默认值对齐 store.ChannelInsert
	if req.Weight == 0 {
		req.Weight = 1
	}
	if req.Status == 0 {
		req.Status = 1
	}
	if req.Status != 1 && req.Status != 2 {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"status must be 1 or 2", "invalid_request_error", "bad_status")
		return
	}

	sc := store.Channel{
		Name:            req.Name,
		Provider:        req.Provider,
		CredentialsJSON: credsJSON,
		Voices:          req.Voices,
		Priority:        req.Priority,
		Weight:          req.Weight,
		Status:          req.Status,
	}

	// 决策 #5:入库前先 FromStoreChannel 校验凭据 → 反序列化 provider.Credentials + 字段自洽
	if _, convErr := route.FromStoreChannel(sc); convErr != nil {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"credentials validation failed: "+convErr.Error(),
			"invalid_request_error", "bad_credentials")
		return
	}

	id, err := s.ChannelInsert(sc)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrChannelDuplicate):
			middleware.SendJSONError(w, http.StatusConflict,
				"channel name already exists", "invalid_request_error", "channel_duplicate")
		case errors.Is(err, store.ErrInvalid):
			middleware.SendJSONError(w, http.StatusBadRequest,
				err.Error(), "invalid_request_error", "bad_request")
		default:
			log.Printf("[channels] insert: %v", err)
			middleware.SendJSONError(w, http.StatusInternalServerError,
				"insert failed", "server_error", "db_write_failed")
		}
		return
	}

	reloadChannels()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
}

// ChannelsUpdateHandler PATCH /api/admin/channels/{id}
// 决策 #2 指针语义:每个字段 nil=不改;name 重名 → 409;credentials 缺省=不改;
// 决策 #5 入库前校验。
func ChannelsUpdateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}
	id := parseIDFromPath(w, r)
	if id == 0 {
		return
	}
	var req channelUpdateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"invalid JSON body", "invalid_request_error", "bad_request")
		return
	}

	upd := store.ChannelUpdate{}

	// 字段直接传递(指针语义已经由 channelUpdateReq 表达)
	if req.Name != nil {
		n := strings.TrimSpace(*req.Name)
		if n == "" {
			middleware.SendJSONError(w, http.StatusBadRequest,
				"name cannot be empty", "invalid_request_error", "bad_request")
			return
		}
		upd.Name = &n
	}
	if req.Credentials != nil {
		// 决策 #2:提供完整对象 = 整体替换;marshal 后入库
		cj, err := marshalCredentials(*req.Credentials)
		if err != nil {
			middleware.SendJSONError(w, http.StatusBadRequest,
				"credentials marshal failed: "+err.Error(),
				"invalid_request_error", "bad_credentials")
			return
		}
		upd.CredentialsJSON = &cj
	}
	if req.Voices != nil {
		v := *req.Voices
		upd.Voices = &v
	}
	if req.Priority != nil {
		upd.Priority = req.Priority
	}
	if req.Weight != nil {
		upd.Weight = req.Weight
	}
	if req.Status != nil {
		upd.Status = req.Status
	}
	if req.AutoBan != nil {
		upd.AutoBan = req.AutoBan
	}

	// 决策 #5:若改凭据,先校验新凭据;同时把"旧行" + "新值" 在内存里合并后 FromStoreChannel 验证
	if upd.CredentialsJSON != nil {
		existing, err := s.ChannelGet(id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "channel not found", http.StatusNotFound)
				return
			}
			middleware.SendJSONError(w, http.StatusInternalServerError,
				"read failed", "server_error", "db_read_failed")
			return
		}
		merged := *existing
		merged.CredentialsJSON = *upd.CredentialsJSON
		if _, convErr := route.FromStoreChannel(merged); convErr != nil {
			middleware.SendJSONError(w, http.StatusBadRequest,
				"credentials validation failed: "+convErr.Error(),
				"invalid_request_error", "bad_credentials")
			return
		}
	}

	if err := s.ChannelUpdate(id, upd); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "channel not found", http.StatusNotFound)
		case errors.Is(err, store.ErrChannelDuplicate):
			middleware.SendJSONError(w, http.StatusConflict,
				"channel name already exists", "invalid_request_error", "channel_duplicate")
		case errors.Is(err, store.ErrInvalid):
			middleware.SendJSONError(w, http.StatusBadRequest,
				err.Error(), "invalid_request_error", "bad_request")
		default:
			log.Printf("[channels] update id=%d: %v", id, err)
			middleware.SendJSONError(w, http.StatusInternalServerError,
				"update failed", "server_error", "db_write_failed")
		}
		return
	}

	reloadChannels()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "updated": true})
}

// ChannelsDeleteHandler DELETE /api/admin/channels/{id}
func ChannelsDeleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}
	id := parseIDFromPath(w, r)
	if id == 0 {
		return
	}
	if err := s.ChannelDelete(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "channel not found", http.StatusNotFound)
			return
		}
		log.Printf("[channels] delete id=%d: %v", id, err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"delete failed", "server_error", "db_write_failed")
		return
	}
	reloadChannels()
	w.WriteHeader(http.StatusNoContent)
}

// ChannelsStatusHandler POST /api/admin/channels/{id}/status
func ChannelsStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}
	id := parseIDFromPath(w, r)
	if id == 0 {
		return
	}
	var req channelStatusReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"invalid JSON body", "invalid_request_error", "bad_request")
		return
	}
	if req.Status == nil {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"status is required", "invalid_request_error", "missing_status")
		return
	}
	if err := s.ChannelSetStatus(id, *req.Status); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "channel not found", http.StatusNotFound)
		case errors.Is(err, store.ErrInvalid):
			middleware.SendJSONError(w, http.StatusBadRequest,
				err.Error(), "invalid_request_error", "bad_status")
		default:
			log.Printf("[channels] status id=%d: %v", id, err)
			middleware.SendJSONError(w, http.StatusInternalServerError,
				"set status failed", "server_error", "db_write_failed")
		}
		return
	}
	reloadChannels()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "status": *req.Status})
}

// ChannelsTestHandler POST /api/admin/channels/{id}/test
// 决策 #7:真合成(测通道账号通不通);用 setting.GetTTSRequest() 默认 voice 模板;
// 文本 "test" + format mp3;日志 [test-only] 供对账过滤。
func ChannelsTestHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}
	id := parseIDFromPath(w, r)
	if id == 0 {
		return
	}
	sc, err := s.ChannelGet(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "channel not found", http.StatusNotFound)
			return
		}
		log.Printf("[channels] test get id=%d: %v", id, err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"get failed", "server_error", "db_read_failed")
		return
	}
	rc, convErr := route.FromStoreChannel(*sc)
	if convErr != nil {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"channel credentials corrupted: "+convErr.Error(),
			"invalid_request_error", "bad_credentials")
		return
	}
	prov, ok := provider.Get(rc.Provider)
	if !ok {
		middleware.SendJSONError(w, http.StatusServiceUnavailable,
			"provider not registered: "+rc.Provider,
			"configuration_error", "provider_unavailable")
		return
	}

	// 测活模板:沿用 setting 快照的 default voice(已解析过 upstream speaker)+ 本渠道凭据
	// 决策 D:避免 voice 解析失败污染测活判断
	tmpl := setting.GetTTSRequest()
	tmpl.Text = "test"
	tmpl.Format = "mp3"
	tmpl.Credentials = rc.Credentials

	// 单独 ctx(短超时,不沿用 r.Context 防止 admin 客户端断开导致测活中断)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	start := time.Now()
	log.Printf("[channels] [test-only] start channel_id=%d provider=%s voice=%s text=%q - 客户端=%s",
		rc.ID, rc.Provider, tmpl.VoiceKey, tmpl.Text, middleware.GetClientIP(r))
	result, err := prov.Synthesize(ctx, tmpl, provider.NoopMetrics())
	dur := time.Since(start)
	if err != nil {
		log.Printf("[channels] [test-only] fail channel_id=%d duration=%v 错误=%v",
			rc.ID, dur, err)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(channelTestResp{
			OK:         false,
			DurationMs: dur.Milliseconds(),
			Error:      err.Error(),
		})
		return
	}
	log.Printf("[channels] [test-only] ok channel_id=%d duration=%v req_id=%s audio_bytes=%d",
		rc.ID, dur, result.ReqID, result.AudioBytes)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(channelTestResp{
		OK:         true,
		DurationMs: dur.Milliseconds(),
	})
}

// ---- 强一致性检查:防 dto 包未引用(若 unused 编译报警) ----
var _ = dto.SynthesisResult{}
