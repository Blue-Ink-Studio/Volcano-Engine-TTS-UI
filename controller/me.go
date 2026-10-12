// controller/me.go — STAGE4 普通用户自助接口(/api/me/*,JWT 鉴权)。
//
// 全部接口都以**会话身份里的 user_id** 为边界:
//   - 任何筛选参数都不得指定 user_id(越权面,决策 X22)
//   - 资源归属由 store 层的 *Owned 方法在 SQL 里强制(越权返 404,不泄露 id 存在性)
//
// ⚠️ 一处刻意的权限收紧:自助接口**不允许**设置 token 的 unlimited 字段。
// unlimited = 完全不计费,若放开,普通用户可自建免死金牌令牌,整套计费失效。
// 该字段只由 admin 侧接口掌握。
package controller

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/volcano-tts/tts-api/middleware"
	"github.com/volcano-tts/tts-api/store"
)

// sessionCallerOr401 取会话身份;缺失时按 401 处理(正常情况下 ValidateSession 已保证存在)。
func sessionCallerOr401(w http.ResponseWriter, r *http.Request) *middleware.SessionCaller {
	sc := middleware.SessionCallerFromContext(r.Context())
	if sc == nil {
		middleware.SendJSONError(w, http.StatusUnauthorized, "session token required", "authentication_error", middleware.SessionCodeRequired)
		return nil
	}
	return sc
}

// decodeMeBody 解析请求体(限 64KB,与 settings 侧口径一致)。
func decodeMeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error", "bad_request")
		return false
	}
	return true
}

// ---- GET /api/me ----

// MeHandler GET /api/me → {name,balance,is_admin,status,created_at}
//
// status 是 **int**(1=enabled 2=disabled 3=deleted),与现有接口口径一致,
// 中文文案由前端映射。
func MeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sc := sessionCallerOr401(w, r)
	if sc == nil {
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}
	u, err := s.UserGet(sc.UserID)
	if err != nil {
		sendNotFound(w, "user not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":       u.Name,
		"balance":    u.Balance,
		"role":       u.Role,       // STAGE5(P9)
		"user_group": u.UserGroup,  // STAGE5(P9)
		"is_admin":   u.IsAdmin,    // 派生自 role
		"status":     u.Status,
		"created_at": u.CreatedAt,
	})
}

// ---- POST /api/me/password ----

type mePasswordReq struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// MePasswordHandler POST /api/me/password → {token}
//
// 决策 X9:改密成功后 token_version +1(踢掉其它设备上的旧 token),
// 同时**返回一枚新 token**,避免当前用户被自己当场踢下线。
func MePasswordHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sc := sessionCallerOr401(w, r)
	if sc == nil {
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}
	var req mePasswordReq
	if !decodeMeBody(w, r, &req) {
		return
	}
	if err := ValidatePassword(req.New); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "weak_password")
		return
	}
	u, err := s.UserGet(sc.UserID)
	if err != nil {
		sendNotFound(w, "user not found")
		return
	}
	if !CheckPassword(u.PasswordHash, req.Old) {
		middleware.SendJSONError(w, http.StatusUnauthorized, "old password is incorrect", "authentication_error", "invalid_password")
		return
	}
	hash, err := HashPassword(req.New)
	if err != nil {
		log.Printf("[me] change password hash user=%d: %v", u.ID, err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "hash password failed", "server_error", "internal_error")
		return
	}
	ver, err := s.UserSetPassword(u.ID, hash)
	if err != nil {
		log.Printf("[me] change password user=%d: %v", u.ID, err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "set password failed", "server_error", "db_write_failed")
		return
	}
	tok, err := middleware.IssueSessionToken(u.ID, u.IsAdmin, ver)
	if err != nil {
		log.Printf("[me] issue token after password change user=%d: %v", u.ID, err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "issue session failed", "server_error", "internal_error")
		return
	}
	log.Printf("[me] 改密成功 user_id=%d(旧 token 已失效)", u.ID)
	writeJSON(w, http.StatusOK, map[string]any{"token": tok})
}

// ---- 令牌管理 ----

// meTokenView 是自助接口的令牌视图(决策 X8:精简 DTO,不含 key_hash)。
type meTokenView struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	KeyPrefix   string `json:"key_prefix"`
	LimitQuota  *int64 `json:"limit_quota"`  // null = 不限
	UsedQuota   int64  `json:"used_quota"`   // 限额预占计数器(含历史用量)
	RemainQuota int64  `json:"remain_quota"` // 冻结的历史值,只读
	Unlimited   bool   `json:"unlimited"`
	Status      int    `json:"status"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func newMeTokenView(t store.Token) meTokenView {
	return meTokenView{
		ID: t.ID, Name: t.Name, KeyPrefix: t.KeyPrefix,
		LimitQuota: t.LimitQuota, UsedQuota: t.UsedQuota, RemainQuota: t.RemainQuota,
		Unlimited: t.Unlimited, Status: t.Status,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

// MeTokensListHandler GET /api/me/tokens → {items,total,limit,offset}
func MeTokensListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sc := sessionCallerOr401(w, r)
	if sc == nil {
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}
	limit, offset := parsePagination(r)
	rows, err := s.TokenListByUser(sc.UserID)
	if err != nil {
		log.Printf("[me] tokens list user=%d: %v", sc.UserID, err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "list tokens failed", "server_error", "db_read_failed")
		return
	}
	// TokenListByUser 不参与分页(单用户名下 token 数量很小),这里在内存里切页,
	// 但仍返回统一信封,保持前端分页组件一致。
	total := len(rows)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	items := make([]meTokenView, 0, end-offset)
	for _, t := range rows[offset:end] {
		items = append(items, newMeTokenView(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": total, "limit": limit, "offset": offset,
	})
}

type meTokenCreateReq struct {
	Name string `json:"name"`
	// LimitQuota:省略或 null 都表示"不限额度"(决策 D18)。
	LimitQuota *int64 `json:"limit_quota"`
}

// MeTokensCreateHandler POST /api/me/tokens → 201 {...,key}
//
// 明文 key **仅此一次**返回(决策 #1)。刻意不接受 unlimited:
// 那是"完全不计费"的开关,只应由管理员掌握(见文件头注释)。
func MeTokensCreateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sc := sessionCallerOr401(w, r)
	if sc == nil {
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}
	var req meTokenCreateReq
	if !decodeMeBody(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		middleware.SendJSONError(w, http.StatusBadRequest, "name is required", "invalid_request_error", "missing_field")
		return
	}
	if req.LimitQuota != nil && *req.LimitQuota < 0 {
		middleware.SendJSONError(w, http.StatusBadRequest, "limit_quota must be >= 0", "invalid_request_error", "bad_quota")
		return
	}
	id, plainKey, err := s.TokenCreateWithKey(sc.UserID, name, false, req.LimitQuota)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInvalid):
			middleware.SendJSONError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "bad_request")
		case errors.Is(err, store.ErrDuplicate):
			middleware.SendJSONError(w, http.StatusConflict, "token key collision, please retry", "server_error", "duplicate_key")
		default:
			log.Printf("[me] token create user=%d: %v", sc.UserID, err)
			middleware.SendJSONError(w, http.StatusInternalServerError, "create token failed", "server_error", "db_write_failed")
		}
		return
	}
	t, err := s.TokenGetOwned(id, sc.UserID)
	if err != nil {
		middleware.SendJSONError(w, http.StatusInternalServerError, "read back failed", "server_error", "db_read_failed")
		return
	}
	log.Printf("[me] 创建令牌 user_id=%d token_id=%d prefix=%s unlimited=false limit_quota=%v (明文 key 仅本次返回)",
		sc.UserID, t.ID, t.KeyPrefix, t.LimitQuota)
	view := newMeTokenView(*t)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": view.ID, "name": view.Name, "key_prefix": view.KeyPrefix,
		"limit_quota": view.LimitQuota, "used_quota": view.UsedQuota,
		"remain_quota": view.RemainQuota, "unlimited": view.Unlimited,
		"status": view.Status, "created_at": view.CreatedAt,
		"key": plainKey, // ← 仅此一次
	})
}

type meTokenPatchReq struct {
	Name   *string `json:"name"`
	Status *int    `json:"status"`
	// LimitQuota 三态(决策 X6):省略=不改、null=清为不限、数字=设为该值。
	LimitQuota store.OptInt64 `json:"limit_quota"`
}

// MeTokensPatchHandler PATCH /api/me/tokens/{id} → 200 令牌对象
func MeTokensPatchHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sc := sessionCallerOr401(w, r)
	if sc == nil {
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
	var req meTokenPatchReq
	if !decodeMeBody(w, r, &req) {
		return
	}
	if req.Name == nil && req.Status == nil && !req.LimitQuota.Set {
		middleware.SendJSONError(w, http.StatusBadRequest, "at least one field is required", "invalid_request_error", "no_fields")
		return
	}
	// 刻意不传 Unlimited:自助接口不得开启"完全不计费"
	patch := store.TokenUpdatePatch{
		Name:       req.Name,
		Status:     req.Status,
		LimitQuota: req.LimitQuota,
	}
	if err := s.TokenUpdateOwned(id, sc.UserID, patch); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			sendNotFound(w, "token not found")
		case errors.Is(err, store.ErrInvalid):
			middleware.SendJSONError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "bad_request")
		default:
			log.Printf("[me] token patch id=%d user=%d: %v", id, sc.UserID, err)
			middleware.SendJSONError(w, http.StatusInternalServerError, "update failed", "server_error", "db_write_failed")
		}
		return
	}
	t, err := s.TokenGetOwned(id, sc.UserID)
	if err != nil {
		sendNotFound(w, "token not found")
		return
	}
	writeJSON(w, http.StatusOK, newMeTokenView(*t))
}

// MeTokensDeleteHandler DELETE /api/me/tokens/{id} → 204
func MeTokensDeleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sc := sessionCallerOr401(w, r)
	if sc == nil {
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
	if err := s.TokenDeleteOwned(id, sc.UserID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			sendNotFound(w, "token not found")
			return
		}
		log.Printf("[me] token delete id=%d user=%d: %v", id, sc.UserID, err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "delete failed", "server_error", "db_write_failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- 调用流水 ----

// meUsageItem 是调用流水的门户视图(决策 X8:不含 user_id/request_id/channel_id)。
type meUsageItem struct {
	ID          int64  `json:"id"`
	TokenID     int64  `json:"token_id"`
	TokenName   string `json:"token_name"`
	Model       string `json:"model"`
	Characters  int    `json:"characters"`
	AudioMS     int64  `json:"audio_ms"`
	AudioBytes  int    `json:"audio_bytes"`
	PreConsumed int64  `json:"pre_consumed"`
	ActualQuota int64  `json:"actual_quota"`
	Status      int    `json:"status"` // 0=进行中 1=已结算 2=已退款
	CreatedAt   string `json:"created_at"`
}

// MeUsageHandler GET /api/me/usage?limit&offset&token_id&status&from&to
// → {items,total,limit,offset}
func MeUsageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sc := sessionCallerOr401(w, r)
	if sc == nil {
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}
	limit, offset := parsePagination(r)

	f := store.UsageFilter{UserID: sc.UserID, Limit: limit, Offset: offset}
	if raw := strings.TrimSpace(r.URL.Query().Get("token_id")); raw != "" {
		v, err := parseOptionalInt64(raw)
		if err != nil {
			middleware.SendJSONError(w, http.StatusBadRequest, "token_id must be an integer", "invalid_request_error", "invalid_filter")
			return
		}
		f.TokenID = &v
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
		v, err := parseOptionalInt(raw)
		if err != nil || v < 0 || v > 2 {
			middleware.SendJSONError(w, http.StatusBadRequest, "status must be 0, 1 or 2", "invalid_request_error", "invalid_filter")
			return
		}
		f.Status = &v
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("from")); raw != "" {
		if !isDateOnly(raw) {
			middleware.SendJSONError(w, http.StatusBadRequest, "from must be YYYY-MM-DD", "invalid_request_error", "invalid_filter")
			return
		}
		f.From = raw
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("to")); raw != "" {
		if !isDateOnly(raw) {
			middleware.SendJSONError(w, http.StatusBadRequest, "to must be YYYY-MM-DD", "invalid_request_error", "invalid_filter")
			return
		}
		f.To = raw
	}

	rows, total, err := s.UsageListFiltered(f)
	if err != nil {
		log.Printf("[me] usage list user=%d: %v", sc.UserID, err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "list usage failed", "server_error", "db_read_failed")
		return
	}

	// token_name 用一次本用户令牌列表做映射(不额外 JOIN,避免改动既有查询)
	names := map[int64]string{}
	if toks, err := s.TokenListByUser(sc.UserID); err == nil {
		for _, t := range toks {
			names[t.ID] = t.Name
		}
	}
	items := make([]meUsageItem, 0, len(rows))
	for _, u := range rows {
		items = append(items, meUsageItem{
			ID: u.ID, TokenID: u.TokenID, TokenName: names[u.TokenID],
			Model: u.Model, Characters: u.Characters, AudioMS: u.AudioMS, AudioBytes: u.AudioBytes,
			PreConsumed: u.PreConsumed, ActualQuota: u.ActualQuota,
			Status: u.Status, CreatedAt: u.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": total, "limit": limit, "offset": offset,
	})
}

// ---- 统计与趋势 ----

// MeStatsHandler GET /api/me/stats?days=14 → {days,totals,trend}
//
// trend 由后端按**北京时间日界**聚合且**缺日补 0**(决策 X5/X17):
// 前端柱状图需要连续日期轴,而分页拉全量自己聚合会受 limit=100 上限影响。
func MeStatsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sc := sessionCallerOr401(w, r)
	if sc == nil {
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}
	days := parseIntQuery(r, "days", 14)
	if days <= 0 {
		days = 14
	}
	if days > 90 {
		days = 90
	}

	totals, err := s.UsageTotalsByUser(sc.UserID)
	if err != nil {
		log.Printf("[me] stats totals user=%d: %v", sc.UserID, err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "read stats failed", "server_error", "db_read_failed")
		return
	}
	trend, err := s.UsageTrendByUser(sc.UserID, days)
	if err != nil {
		log.Printf("[me] stats trend user=%d: %v", sc.UserID, err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "read trend failed", "server_error", "db_read_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"days":   days,
		"totals": totals,
		"trend":  trend,
	})
}

// ---- 余额流水 ----

// MeBalanceLogsHandler GET /api/me/balance/logs?limit&offset&type
// → {items,total,limit,offset}
//
// ⚠️ 本表**只记余额来源**(充值/兑换/手动调整/赠送);调用消费在 /api/me/usage。
// 门户必须同时展示两者,否则钱包历史不完整(决策 D25/D33)。
func MeBalanceLogsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sc := sessionCallerOr401(w, r)
	if sc == nil {
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}
	limit, offset := parsePagination(r)

	typ := strings.TrimSpace(r.URL.Query().Get("type"))
	if typ != "" && !isValidChargeType(typ) {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"type must be one of charge/redeem/manual_adjust/grant", "invalid_request_error", "invalid_filter")
		return
	}

	rows, total, err := s.ChargeLogListByUser(sc.UserID, typ, limit, offset)
	if err != nil {
		log.Printf("[me] balance logs user=%d: %v", sc.UserID, err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "list balance logs failed", "server_error", "db_read_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": rows, "total": total, "limit": limit, "offset": offset,
	})
}

// ---- 充值码兑换 ----

type meRedeemReq struct {
	Code string `json:"code"`
}

// MeRedeemHandler POST /api/me/redeem → {quota,balance}
//
// ⚠️ 逃生通道(决策 X24):本接口**不受余额校验影响** —— 欠费用户必须还能用
// 充值码自救。实现时切勿在此加余额判断。
func MeRedeemHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sc := sessionCallerOr401(w, r)
	if sc == nil {
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}
	var req meRedeemReq
	if !decodeMeBody(w, r, &req) {
		return
	}
	code := store.NormalizeRedeemCode(req.Code) // trim + 大写(决策 #21)
	if code == "" {
		middleware.SendJSONError(w, http.StatusBadRequest, "code is required", "invalid_request_error", "missing_field")
		return
	}
	quota, balance, err := s.RedemptionRedeemAtomic(code, sc.UserID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		middleware.SendJSONError(w, http.StatusNotFound, "invalid redemption code", "invalid_request_error", "invalid_code")
		return
	case errors.Is(err, store.ErrRedeemCodeUsed):
		middleware.SendJSONError(w, http.StatusConflict, "redemption code already used", "invalid_request_error", "code_used")
		return
	case errors.Is(err, store.ErrInsufficientBalance):
		// 理论上不会发生(加余额不会为负),保守映射为 400
		middleware.SendJSONError(w, http.StatusBadRequest, "balance update rejected", "invalid_request_error", "bad_request")
		return
	case err != nil:
		log.Printf("[me] redeem user=%d: %v", sc.UserID, err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "redeem failed", "server_error", "db_write_failed")
		return
	}
	log.Printf("[me] 兑换成功 user_id=%d quota=%d 余额=%d", sc.UserID, quota, balance)
	writeJSON(w, http.StatusOK, map[string]any{"quota": quota, "balance": balance})
}

// ---- 小工具 ----

func isValidChargeType(t string) bool {
	switch t {
	case store.ChargeTypeCharge, store.ChargeTypeRedeem, store.ChargeTypeManualAdjust, store.ChargeTypeGrant:
		return true
	}
	return false
}

// isDateOnly 校验 YYYY-MM-DD(严格:解析后回格式化必须一致,挡住 2026-02-31 之类)。
func isDateOnly(s string) bool {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return false
	}
	return t.Format("2006-01-02") == s
}

// parseOptionalInt64 / parseOptionalInt 解析筛选参数;非法值必须报错,
// 不能静默忽略(决策 X22:非法筛选值 → 400 invalid_filter)。
func parseOptionalInt64(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }
func parseOptionalInt(s string) (int, error)     { return strconv.Atoi(s) }
