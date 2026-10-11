// controller/users.go — 阶段 3a 用户/用量查询接口
//
// 现阶段只读(user 列表、单个 user 详情、user 的 usage 流水);
// user/token 的写操作(增删改)放阶段 3b 跟 quota 一起做,免得分两次。
package controller

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
	"github.com/volcano-tts/tts-api/middleware"
	"github.com/volcano-tts/tts-api/store"
)

// UsersListHandler GET /api/admin/users
// 列出所有 user(含 disabled);供 admin 仪表盘。
func UsersListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}
	users, err := s.UserList()
	if err != nil {
		log.Printf("[users] list: %v", err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"list users failed", "server_error", "db_read_failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"users": users, "total": len(users)})
}

// UsersGetHandler GET /api/admin/users/{id}
// 单个 user 详情(可后补;本阶段先全写)。
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
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"get user failed", "server_error", "db_read_failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(u)
}

// UserUsageHandler GET /api/admin/users/{id}/usage?limit=100&offset=0
// 该 user 的 usage 流水 + 总和。
// 决策 #9:limit/offset 形式,默认 limit=100,上限 1000。
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
	// 先确认 user 存在(避免空 user 返空列表让人误以为"用户没问题")
	if _, err := s.UserGet(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"get user failed", "server_error", "db_read_failed")
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}

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

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"user_id": id,
		"usage":   usages,
		"totals":  totals,
		"total":   total,
		"limit":   limit,
		"offset":  offset,
	})
}

// ---- helpers -----------------------------------------------------

// 防 unused mux 引用(mux 在 parseIDFromPath 用)
var _ = mux.Vars
