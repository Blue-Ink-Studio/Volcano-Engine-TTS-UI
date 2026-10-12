// controller/tokens.go — 阶段 3b 令牌管理接口
//
// API 契约严格按策划文档 §4.2。
//
// 安全要点(决策 #1):发令牌时服务端随机生成明文 key,**只在 201 响应里返回一次**;
// 之后任何查询(GET 列表/详情)只返回 key_prefix,明文不再可用。
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

// tokenView 对应契约 GET /users/{id}/tokens 的 items 元素。
// 注意:KeyHash 带 json:"-" 不会外泄;这里也不输出 key(明文)。
type tokenView struct {
	ID          int64  `json:"id"`
	UserID      int64  `json:"user_id"`
	Name        string `json:"name"`
	KeyPrefix   string `json:"key_prefix"`
	RemainQuota int64  `json:"remain_quota"`
	UsedQuota   int64  `json:"used_quota"`
	Unlimited   bool   `json:"unlimited"`
	Status      int    `json:"status"`
	CreatedAt   string `json:"created_at"`
}

func newTokenView(t store.Token) tokenView {
	return tokenView{
		ID:          t.ID,
		UserID:      t.UserID,
		Name:        t.Name,
		KeyPrefix:   t.KeyPrefix,
		RemainQuota: t.RemainQuota,
		UsedQuota:   t.UsedQuota,
		Unlimited:   t.Unlimited,
		Status:      t.Status,
		CreatedAt:   t.CreatedAt,
	}
}

// ---- GET /api/admin/users/{id}/tokens -------------------------------

// TokensListHandler GET /api/admin/users/{id}/tokens → {items:[...]}
// 契约未定义分页(user 下 token 数通常很少),全量返回。
func TokensListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}
	uid := parseIDFromPath(w, r)
	if uid == 0 {
		return
	}
	if !userExistsActive(w, s, uid) {
		return
	}
	tokens, err := s.TokenListByUser(uid)
	if err != nil {
		log.Printf("[tokens] list user=%d: %v", uid, err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"list tokens failed", "server_error", "db_read_failed")
		return
	}
	items := make([]tokenView, 0, len(tokens))
	for _, t := range tokens {
		items = append(items, newTokenView(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// ---- POST /api/admin/users/{id}/tokens ------------------------------

type tokenCreateReq struct {
	Name string `json:"name"`
	// Unlimited=true 表示该 token 不计费(但仍受欠费停服约束,决策 X24)。
	Unlimited *bool `json:"unlimited"`
	// LimitQuota 是**可选准入限额**(nil = 不限额度)。
	// STAGE4 起取代旧的 `quota`(那写的是已冻结的 remain_quota)。
	LimitQuota *int64 `json:"limit_quota"`
}

// TokensCreateHandler POST /api/admin/users/{id}/tokens
// → 201 {id,name,key_prefix,limit_quota,unlimited,status,key}
// **key 明文仅此一次**(决策 #1:服务端随机生成)。
func TokensCreateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}
	uid := parseIDFromPath(w, r)
	if uid == 0 {
		return
	}
	if !userExistsActive(w, s, uid) {
		return
	}
	var req tokenCreateReq
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
	unlimited := false
	if req.Unlimited != nil {
		unlimited = *req.Unlimited
	}
	// limit_quota 省略 / null 都表示"不限额度"(D18);给了值则必须 >= 0
	if req.LimitQuota != nil && *req.LimitQuota < 0 {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"limit_quota must be >= 0", "invalid_request_error", "bad_quota")
		return
	}

	id, plainKey, err := s.TokenCreateWithKey(uid, req.Name, unlimited, req.LimitQuota)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInvalid):
			middleware.SendJSONError(w, http.StatusBadRequest,
				err.Error(), "invalid_request_error", "bad_request")
		case errors.Is(err, store.ErrDuplicate):
			// 32 字节随机 key 撞库:理论不可能,但按 409 明确回报而不是 500
			middleware.SendJSONError(w, http.StatusConflict,
				"token key collision, please retry", "server_error", "duplicate_name")
		default:
			log.Printf("[tokens] create user=%d: %v", uid, err)
			middleware.SendJSONError(w, http.StatusInternalServerError,
				"create token failed", "server_error", "db_write_failed")
		}
		return
	}
	t, err := s.TokenGet(id)
	if err != nil {
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"read back failed", "server_error", "db_read_failed")
		return
	}
	// 明文 key 只在此响应出现一次(日志也刻意不打 key)
	log.Printf("[tokens] created token_id=%d user_id=%d prefix=%s unlimited=%v limit_quota=%v (明文 key 仅本次响应返回)",
		t.ID, uid, t.KeyPrefix, t.Unlimited, t.LimitQuota)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":           t.ID,
		"name":         t.Name,
		"key_prefix":   t.KeyPrefix,
		"limit_quota":  t.LimitQuota,
		"used_quota":   t.UsedQuota,
		"remain_quota": t.RemainQuota, // 冻结的历史值,只读展示
		"unlimited":    t.Unlimited,
		"status":       t.Status,
		"key":          plainKey, // ← 仅此一次
	})
}

// ---- PATCH /api/admin/tokens/{id} ----------------------------------

type tokenUpdateReq struct {
	Name      *string `json:"name"`
	Unlimited *bool   `json:"unlimited"`
	Status    *int    `json:"status"`
	// LimitQuota 三态(决策 X6):省略=不改、null=清为不限、数字=设为该值。
	LimitQuota store.OptInt64 `json:"limit_quota"`
}

// TokensUpdateHandler PATCH /api/admin/tokens/{id}
// 指针语义:nil=不改;limit_quota 三态见 store.OptInt64。
func TokensUpdateHandler(w http.ResponseWriter, r *http.Request) {
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
	var req tokenUpdateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"invalid JSON body", "invalid_request_error", "bad_request")
		return
	}
	patch := store.TokenUpdatePatch{
		Name:       req.Name,
		Unlimited:  req.Unlimited,
		Status:     req.Status,
		LimitQuota: req.LimitQuota,
	}
	if err := s.TokenUpdate(id, patch); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			sendNotFound(w, "token not found")
		case errors.Is(err, store.ErrInvalid):
			middleware.SendJSONError(w, http.StatusBadRequest,
				err.Error(), "invalid_request_error", "bad_request")
		default:
			log.Printf("[tokens] update id=%d: %v", id, err)
			middleware.SendJSONError(w, http.StatusInternalServerError,
				"update failed", "server_error", "db_write_failed")
		}
		return
	}
	t, err := s.TokenGet(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			sendNotFound(w, "token not found")
			return
		}
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"read back failed", "server_error", "db_read_failed")
		return
	}
	writeJSON(w, http.StatusOK, newTokenView(*t))
}

// ---- DELETE /api/admin/tokens/{id} ---------------------------------

// TokensDeleteHandler DELETE /api/admin/tokens/{id} → 204
// 物理删 token;usage 流水保留(决策 #5:审计不删)。
func TokensDeleteHandler(w http.ResponseWriter, r *http.Request) {
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
	if err := s.TokenDelete(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			sendNotFound(w, "token not found")
			return
		}
		log.Printf("[tokens] delete id=%d: %v", id, err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"delete failed", "server_error", "db_write_failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- helpers -------------------------------------------------------

// userExistsActive 校验 user 存在且未软删;不满足时直接写出 404 并返回 false。
// 令牌接口都挂在 user 下,统一先过这道校验。
func userExistsActive(w http.ResponseWriter, s *store.Store, userID int64) bool {
	u, err := s.UserGet(userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			sendNotFound(w, "user not found")
			return false
		}
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"get user failed", "server_error", "db_read_failed")
		return false
	}
	if u.Status == store.UserDeleteStatus {
		sendNotFound(w, "user not found")
		return false
	}
	return true
}
