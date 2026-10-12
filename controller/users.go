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
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
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
// STAGE4(§8.3)追加 balance / is_admin:admin 列表要能直接看到钱包与角色。
type userListItem struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     int    `json:"status"`
	UsedQuota  int64  `json:"used_quota"`
	TokenCount int    `json:"token_count"`
	Balance    int64  `json:"balance"`
	IsAdmin    bool   `json:"is_admin"`
	CreatedAt  string `json:"created_at"`
}

// userDetail 对应契约 GET /users/{id} 与 PATCH 的响应。
// STAGE4(§8.3)追加 balance / is_admin。
type userDetail struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Status    int    `json:"status"`
	UsedQuota int64  `json:"used_quota"`
	Balance   int64  `json:"balance"`
	IsAdmin   bool   `json:"is_admin"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func newUserDetail(u *store.User) userDetail {
	return userDetail{
		ID:        u.ID,
		Name:      u.Name,
		Status:    u.Status,
		UsedQuota: u.UsedQuota,
		Balance:   u.Balance,
		IsAdmin:   u.IsAdmin,
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
			Balance:    it.Balance,
			IsAdmin:    it.IsAdmin,
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

// userCreateReq 是建号入参。
// STAGE4(D26)扩展 balance / is_admin / password:
//   - password 不传 → password_hash 留空,该账号走"首次设密"流程(D8/D23)
//   - balance > 0 → 走 UserCreateWithBalance 记 charge_logs(type=grant, D33)
type userCreateReq struct {
	Name     string  `json:"name"`
	Status   *int    `json:"status"`
	Balance  *int    `json:"balance"`
	IsAdmin  *bool   `json:"is_admin"`
	Password *string `json:"password"`
}

// UsersCreateHandler POST /api/admin/users
// → 201 {id,name,status,created_at,balance,is_admin}(§8.3:现有形状 + balance)
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

	// 口令可选:传了就必须合规(D19);不传则留空,走首次设密(D8)
	var hash string
	if req.Password != nil {
		if err := ValidatePassword(*req.Password); err != nil {
			middleware.SendJSONError(w, http.StatusBadRequest,
				err.Error(), "invalid_request_error", "weak_password")
			return
		}
		h, err := HashPassword(*req.Password)
		if err != nil {
			log.Printf("[users] create hash password: %v", err)
			middleware.SendJSONError(w, http.StatusInternalServerError,
				"hash password failed", "server_error", "internal_error")
			return
		}
		hash = h
	}

	isAdmin := false
	if req.IsAdmin != nil {
		isAdmin = *req.IsAdmin
	}
	// 负数余额无意义:按 0 处理(不写 charge_logs)
	balance := int64(0)
	if req.Balance != nil && *req.Balance > 0 {
		balance = int64(*req.Balance)
	}

	newUser := store.User{Name: req.Name, Status: status, PasswordHash: hash, IsAdmin: isAdmin}
	var id int64
	var err error
	if balance > 0 {
		// D33:admin 建号填的初始额度也要记账
		id, err = s.UserCreateWithBalance(newUser, balance, store.ChargeSourceAdminCreate, "建号初始额度")
	} else {
		id, err = s.UserInsert(newUser)
	}
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
	log.Printf("[users] 建号成功 id=%d name=%q is_admin=%v balance=%d", u.ID, u.Name, u.IsAdmin, u.Balance)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         u.ID,
		"name":       u.Name,
		"status":     u.Status,
		"created_at": u.CreatedAt,
		"balance":    u.Balance,
		"is_admin":   u.IsAdmin,
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
	Name    *string `json:"name"`
	Status  *int    `json:"status"`
	IsAdmin *bool   `json:"is_admin"` // STAGE4(D26):升降级
}

// guardLastAdmin 实施「最后一个 admin 保护」(D26 §8.3)。
//
// 判定口径:目标用户本身 `is_admin=1 且 status=1`,且当前**可用管理员总数 <= 1** 时,
// 拒绝把该行降级 / 禁用 / 删除 —— 否则系统将不再有管理员。
// 不需要保护的操作(改名、启用、充值等)直接放行,不查计数。
//
// 返回 true 表示已写出 409 last_admin(调用方直接 return)。
func guardLastAdmin(w http.ResponseWriter, s *store.Store, u *store.User, degrade, disable bool) bool {
	if u == nil || (!degrade && !disable) {
		return false
	}
	// 目标不是"可用管理员" → 不在保护范围(禁用中的 admin 可以随便降级/再禁用)
	if !u.IsAdmin || u.Status != 1 {
		return false
	}
	n, err := s.UserCountActiveAdmins()
	if err != nil {
		log.Printf("[users] count active admins: %v", err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"count admins failed", "server_error", "db_read_failed")
		return true
	}
	if n > 1 {
		return false
	}
	log.Printf("[users] 拒绝操作:user_id=%d 是唯一可用管理员(D26)", u.ID)
	middleware.SendJSONError(w, http.StatusConflict,
		"at least one enabled admin must remain", "invalid_request_error", "last_admin")
	return true
}

// UsersUpdateHandler PATCH /api/admin/users/{id}
// 指针语义:nil=不改;改名重名 → 409 duplicate_name;软删用户 → 404;
// 动到唯一管理员(D26)→ 409 last_admin。
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

	// 先取当前行:既做 404 判定,也作为 D26 保护的判据
	cur, err := s.UserGet(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			sendNotFound(w, "user not found")
			return
		}
		log.Printf("[users] update preload id=%d: %v", id, err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"get user failed", "server_error", "db_read_failed")
		return
	}
	if cur.Status == store.UserDeleteStatus {
		sendNotFound(w, "user not found")
		return
	}
	degrade := req.IsAdmin != nil && !*req.IsAdmin
	disable := req.Status != nil && *req.Status == 2 // 2 = disabled(1=enabled, 3=deleted 走 DELETE)
	if guardLastAdmin(w, s, cur, degrade, disable) {
		return
	}

	patch := store.UserUpdatePatch{Name: req.Name, Status: req.Status, IsAdmin: req.IsAdmin}
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
// D26:唯一可用管理员不可删除 → 409 last_admin。
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
	cur, err := s.UserGet(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			sendNotFound(w, "user not found")
			return
		}
		log.Printf("[users] delete preload id=%d: %v", id, err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"get user failed", "server_error", "db_read_failed")
		return
	}
	if guardLastAdmin(w, s, cur, false, true) {
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

// ---- POST /api/admin/users/{id}/charge(§8.3,D25) --------------------

type userChargeReq struct {
	Delta  *int64 `json:"delta"`
	Remark string `json:"remark"`
}

// UsersChargeHandler POST /api/admin/users/{id}/charge
//
// body {delta,remark?} → 200 {balance}(变更后余额)。
//   - delta == 0(或缺失)→ 400 no_fields(空操作无意义)
//   - delta > 0 → charge_logs.type=charge
//   - delta < 0 → charge_logs.type=manual_adjust(D25)且**不允许扣成负**
//   - source 恒为 manual;余额与流水由 store 保证同事务(X18)
//
// 用户不存在 / 已软删 → 404;会扣成负 → 400 insufficient_balance。
func UsersChargeHandler(w http.ResponseWriter, r *http.Request) {
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
	var req userChargeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"invalid JSON body", "invalid_request_error", "bad_request")
		return
	}
	if req.Delta == nil || *req.Delta == 0 {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"delta must be a non-zero integer", "invalid_request_error", "no_fields")
		return
	}
	delta := *req.Delta
	typ := store.ChargeTypeCharge
	if delta < 0 {
		typ = store.ChargeTypeManualAdjust // D25:扣款/纠错记为手动调整
	}
	remark := strings.TrimSpace(req.Remark)

	balance, err := s.UserAddBalance(id, delta, typ, store.ChargeSourceManual, remark)
	switch {
	case errors.Is(err, store.ErrInsufficientBalance):
		middleware.SendJSONError(w, http.StatusBadRequest,
			"insufficient balance: the adjustment would make the balance negative",
			"invalid_request_error", "insufficient_balance")
		return
	case errors.Is(err, store.ErrNotFound):
		sendNotFound(w, "user not found")
		return
	case err != nil:
		log.Printf("[users] charge id=%d delta=%d: %v", id, delta, err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"charge failed", "server_error", "db_write_failed")
		return
	}
	log.Printf("[users] charge id=%d delta=%d type=%s balance=%d", id, delta, typ, balance)
	writeJSON(w, http.StatusOK, map[string]any{"balance": balance})
}

// ---- POST /api/admin/users/{id}/password-reset(§8.3,D9) -------------

// adminResetPasswordLen 是后端随机生成的临时密码长度(契约 §8.3:16 位)。
const adminResetPasswordLen = 16

// adminResetCharset 是随机密码字符集:A-Za-z0-9。
const adminResetCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// generateRandomPassword 用 crypto/rand 生成 n 位随机密码。
//
// 与 store.generateRedeemCode 同款**拒绝采样**:丢弃 >= 248 的字节
// (248 = 62×4),避免 v%62 的模偏差 —— 临时密码同样是凭据,分布要均匀。
func generateRandomPassword(n int) (string, error) {
	const maxAccept = 256 - (256 % len(adminResetCharset)) // 248
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("controller: generate random password: %w", err)
		}
		for _, v := range buf {
			if int(v) >= maxAccept {
				continue
			}
			out = append(out, adminResetCharset[int(v)%len(adminResetCharset)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}

type userPasswordResetReq struct {
	NewPassword *string `json:"new_password"`
}

// UsersPasswordResetHandler POST /api/admin/users/{id}/password-reset
//
// body {new_password?} → 200 {password}(**最终生效的明文**)。
//   - 未传 → 后端生成 16 位随机密码,仅在本次响应返回一次
//   - 传了 → 走 ValidatePassword(D19),不合规 → 400 weak_password
//
// 落库走 UserSetPassword(D9):token_version+1,该用户所有旧 JWT 立刻失效。
// ⚠️ 明文密码**绝不写日志**(日志只记 user_id 与"是否随机生成")。
func UsersPasswordResetHandler(w http.ResponseWriter, r *http.Request) {
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
	var req userPasswordResetReq
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"invalid JSON body", "invalid_request_error", "bad_request")
		return
	}

	// 先确认用户存在:免得白跑一次 bcrypt,也让 404 更明确
	u, err := s.UserGet(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			sendNotFound(w, "user not found")
			return
		}
		log.Printf("[users] password-reset preload id=%d: %v", id, err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"get user failed", "server_error", "db_read_failed")
		return
	}
	if u.Status == store.UserDeleteStatus {
		sendNotFound(w, "user not found")
		return
	}

	generated := req.NewPassword == nil
	plain := ""
	if generated {
		pw, err := generateRandomPassword(adminResetPasswordLen)
		if err != nil {
			log.Printf("[users] password-reset generate id=%d: %v", id, err)
			middleware.SendJSONError(w, http.StatusInternalServerError,
				"generate password failed", "server_error", "internal_error")
			return
		}
		plain = pw
	} else {
		plain = *req.NewPassword
		if err := ValidatePassword(plain); err != nil {
			middleware.SendJSONError(w, http.StatusBadRequest,
				err.Error(), "invalid_request_error", "weak_password")
			return
		}
	}

	hash, err := HashPassword(plain)
	if err != nil {
		log.Printf("[users] password-reset hash id=%d: %v", id, err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"hash password failed", "server_error", "internal_error")
		return
	}
	ver, err := s.UserSetPassword(id, hash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			sendNotFound(w, "user not found")
			return
		}
		log.Printf("[users] password-reset set id=%d: %v", id, err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"set password failed", "server_error", "db_write_failed")
		return
	}
	// 刻意不打明文密码;仅记目标用户与来源
	log.Printf("[users] password-reset ok id=%d name=%q generated=%v token_version=%d(明文仅本次响应返回)",
		u.ID, u.Name, generated, ver)
	writeJSON(w, http.StatusOK, map[string]any{"password": plain})
}
