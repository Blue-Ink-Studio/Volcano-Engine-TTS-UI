// controller/redemptions.go — STAGE4 管理端充值码接口(§8.3)。
//
// 契约:
//
//	POST /api/admin/redemptions {quota,count,remark?} → 201 {codes:[...]}
//	     quota < 1            → 400 invalid_quota
//	     count < 1 或 > 1000  → 400 invalid_count(X20:store.RedeemMaxBatch)
//	GET  /api/admin/redemptions?limit&offset&used= → {items,total,limit,offset}
//	     used 三态:""/"0"/"1";其它值 → 400 invalid_filter
//
// 明文码列表中**含 code 明文** —— 这是管理端有意的行为(admin 要分发码);
// 但日志里绝不打码明文。兑换侧(用户)走 /api/me/redeem。
package controller

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/volcano-tts/tts-api/middleware"
	"github.com/volcano-tts/tts-api/store"
)

// ---- POST /api/admin/redemptions ------------------------------------

type redemptionCreateReq struct {
	Quota  int64  `json:"quota"`
	Count  int    `json:"count"`
	Remark string `json:"remark"`
}

// RedemptionsCreateHandler POST /api/admin/redemptions → 201 {codes:[...]}
//
// 批量生成同面额充值码;生成失败整体回滚(不留半批,X20)。
func RedemptionsCreateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16) // 64KB
	var req redemptionCreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"invalid JSON body", "invalid_request_error", "bad_request")
		return
	}
	if req.Quota < 1 {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"quota must be >= 1", "invalid_request_error", "invalid_quota")
		return
	}
	if req.Count < 1 || req.Count > store.RedeemMaxBatch {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"count must be between 1 and "+strconv.Itoa(store.RedeemMaxBatch),
			"invalid_request_error", "invalid_count")
		return
	}

	codes, err := s.RedemptionCreateBatch(req.Quota, req.Count, strings.TrimSpace(req.Remark))
	if err != nil {
		if errors.Is(err, store.ErrInvalid) {
			middleware.SendJSONError(w, http.StatusBadRequest,
				err.Error(), "invalid_request_error", "bad_request")
			return
		}
		log.Printf("[redemptions] create quota=%d count=%d: %v", req.Quota, req.Count, err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"create redemptions failed", "server_error", "db_write_failed")
		return
	}
	// 刻意不打明文码(码等同现金);只记数量与面额
	log.Printf("[redemptions] 生成成功 count=%d quota=%d(明文码仅在本次响应返回)", len(codes), req.Quota)
	writeJSON(w, http.StatusCreated, map[string]any{"codes": codes})
}

// ---- GET /api/admin/redemptions -------------------------------------

// RedemptionsListHandler GET /api/admin/redemptions?limit&offset&used=
// used 三态:""(全部)/"0"(未使用)/"1"(已使用);其它值 → 400 invalid_filter。
func RedemptionsListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}
	limit, offset := parsePagination(r)
	used := strings.TrimSpace(r.URL.Query().Get("used"))
	switch used {
	case "", "0", "1":
	default:
		middleware.SendJSONError(w, http.StatusBadRequest,
			`used must be "" (all), "0" (unused) or "1" (used)`,
			"invalid_request_error", "invalid_filter")
		return
	}

	items, total, err := s.RedemptionList(used, limit, offset)
	if err != nil {
		log.Printf("[redemptions] list used=%q: %v", used, err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"list redemptions failed", "server_error", "db_read_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":  items,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}
