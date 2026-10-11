// controller/users.go — 阶段 3b 用户管理接口(读 + 写)
//
// API 契约严格按策划文档 §4.1;字段名/层级/错误码都以契约为准
// (前端将严格依赖该契约实现,故响应只输出契约列出的字段)。
//
// 统一约定:
//   - 分页 limit 默认 20、上限 100;offset >= 0
//   - 错误体 {"error":{message,type,code}}(middleware.SendJSONError)
//   - 404 code=not_found;409 code=duplicate_name
package controller

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/volcano-tts/tts-api/middleware"
	"github.com/volcano-tts/tts-api/store"
)

// 分页统一口径(策划 §4 开头)。
const (
	defaultPageLimit = 20
	maxPageLimit     = 100
)

// parsePagination 解析 limit/offset,套用统一默认值与上限。
func parsePagination(r *http.Request) (limit, offset int) {
	limit = parseIntQuery(r, "limit", defaultPageLimit)
	offset = parseIntQuery(r, "offset", 0)
	if limit <= 0 {
		limit = defaultPageLimit
	}
	if limit > maxPageLimit {
		limit = maxPageLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// ---- 响应视图(只含契约字段) -----------------------------------------

// userListItem 对应契约 GET /users 的 items 元素。
type userListItem struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     int    `json:"status"`
	UsedQuota  int64  `json:"used_quota"`
	TokenCount int    `json:"token_count"`
	CreatedAt  string `json:"created_at"`
}

// userDetail 对应契约 GET /users/{id} 与 PATCH 的响应。
type userDetail struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Status    int    `json:"status"`
	UsedQuota int64  `json:"used_quota"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func newUserDetail(u *store.User) userDetail {
	return userDetail{
		ID:        u.ID,
		Name:      u.Name,
		Status:    u.Status,
		UsedQuota: u.UsedQuota,
		CreatedAt: u.CreatedAt,
		UpdatedAt: u.UpdatedAt,
	}
}

// ---- GET /api/admin/users -------------------------------------------

// UsersListHandler GET /api/admin/users?limit&offset&search
// 返回 {items:[...], total, limit, offset};search 匹配 name(模糊)。
func UsersListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}
	limit, offset := parsePagination(r)
	search := strings.TrimSpace(r.URL.Query().Get("search"))

	rows, total, err := s.UserSearch(search, limit, offset)
	if err != nil {
		log.Printf("[users] search: %v", err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"list users failed", "server_error", "db_read_failed")
		return
	}
	items := make([]userListItem, 0, len(rows))
	for _, it := range rows {
		items = append(items, userListItem{
			ID:         it.ID,
			Name:       it.Name,
			Status:     it.Status,
			UsedQuota:  it.UsedQuota,
			TokenCount: it.TokenCount,
			CreatedAt:  it.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":  items,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// ---- POST /api/admin/users ------------------------------------------

type userCreateReq struct {
	Name   string `json:"name"`
	Status *int   `json:"status"`
}

// UsersCreateHandler POST /api/admin/users → 201 {id,name,status,created_at}
func UsersCreateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}
	var req userCreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"invalid JSON body", "invalid_request_error", "bad_request")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"name is required", "invalid_request_error", "missing_name")
		return
	}
	status := 1
	if req.Status != nil {
		status = *req.Status
	}
	if status != 1 && status != 2 {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"status must be 1 (enabled) or 2 (disabled)", "invalid_request_error", "bad_status")
		return
	}
	id, err := s.UserInsert(store.User{Name: req.Name, Status: status})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrDuplicate):
			middleware.SendJSONError(w, http.StatusConflict,
				"user name already exists", "invalid_request_error", "duplicate_name")
		case errors.Is(err, store.ErrInvalid):
			middleware.SendJSONError(w, http.StatusBadRequest,
				err.Error(), "invalid_request_error", "bad_request")
		default:
			log.Printf("[users] insert: %v", err)
			middleware.SendJSONError(w, http.StatusInternalServerError,
				"insert failed", "server_error", "db_write_failed")
		}
		return
	}
	u, err := s.UserGet(id)
	if err != nil {
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"read back failed", "server_error", "db_read_failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         u.ID,
		"name":       u.Name,
		"status":     u.Status,
		"created_at": u.CreatedAt,
	})
}

// ---- GET /api/admin/users/{id} --------------------------------------

// UsersGetHandler GET /api/admin/users/{id} → 200 userDetail / 404
func UsersGetHandler(w http.ResponseWriter, r *http.Request) {
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
	u, err := s.UserGet(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			sendNotFound(w, "user not found")
			return
		}
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"get user failed", "server_error", "db_read_failed")
		return
	}
	writeJSON(w, http.StatusOK, newUserDetail(u))
}

// ---- PATCH /api/admin/users/{id} -----------------------------------

type userUpdateReq struct {
	Name   *string `json:"name"`
	Status *int    `json:"status"`
}

// UsersUpdateHandler PATCH /api/admin/users/{id}
// 指针语义:nil=不改;改名重名 → 409 duplicate_name;软删用户 → 404。
func UsersUpdateHandler(w http.ResponseWriter, r *http.Request) {
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
	var req userUpdateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"invalid JSON body", "invalid_request_error", "bad_request")
		return
	}
	patch := store.UserUpdatePatch{Name: req.Name, Status: req.Status}
	if err := s.UserUpdate(id, patch); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			sendNotFound(w, "user not found")
		case errors.Is(err, store.ErrDuplicate):
			middleware.SendJSONError(w, http.StatusConflict,
				"user name already exists", "invalid_request_error", "duplicate_name")
		case errors.Is(err, store.ErrInvalid):
			middleware.SendJSONError(w, http.StatusBadRequest,
				err.Error(), "invalid_request_error", "bad_request")
		default:
			log.Printf("[users] update id=%d: %v", id, err)
			middleware.SendJSONError(w, http.StatusInternalServerError,
				"update failed", "server_error", "db_write_failed")
		}
		return
	}
	u, err := s.UserGet(id)
	if err != nil {
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"read back failed", "server_error", "db_read_failed")
		return
	}
	writeJSON(w, http.StatusOK, newUserDetail(u))
}

// ---- DELETE /api/admin/users/{id} ----------------------------------

// UsersDeleteHandler DELETE /api/admin/users/{id} → 204(软删 status=3)
// 决策 #2:表数据与 usage 流水全部保留,仅置状态;重复删 → 404。
func UsersDeleteHandler(w http.ResponseWriter, r *http.Request) {
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
	if err := s.UserSoftDelete(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			sendNotFound(w, "user not found")
			return
		}
		log.Printf("[users] soft delete id=%d: %v", id, err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"delete failed", "server_error", "db_write_failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- GET /api/admin/users/{id}/usage -------------------------------

// UserUsageHandler GET /api/admin/users/{id}/usage?limit&offset
// 返回 {usage:[...], totals:{...}, total, limit, offset}(契约 §4.1)。
func UserUsageHandler(w http.ResponseWriter, r *http.Request) {
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
	// 先确认 user 存在(软删用户视为不存在 → 404)
	u, err := s.UserGet(id)
	if err != nil || u.Status == store.UserDeleteStatus {
		if err == nil || errors.Is(err, store.ErrNotFound) {
			sendNotFound(w, "user not found")
			return
		}
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"get user failed", "server_error", "db_read_failed")
		return
	}

	limit, offset := parsePagination(r)

	usages, err := s.UsageListByUser(id, limit, offset)
	if err != nil {
		log.Printf("[users] usage list id=%d: %v", id, err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"list usage failed", "server_error", "db_read_failed")
		return
	}
	totals, err := s.UsageTotalsByUser(id)
	if err != nil {
		log.Printf("[users] usage totals id=%d: %v", id, err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"totals failed", "server_error", "db_read_failed")
		return
	}
	total, err := s.UsageCountByUser(id)
	if err != nil {
		log.Printf("[users] usage count id=%d: %v", id, err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"count failed", "server_error", "db_read_failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"usage":  usages,
		"totals": totals,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}
