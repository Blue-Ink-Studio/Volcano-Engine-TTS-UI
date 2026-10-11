// controller/httpjson.go — controller 层共用的 JSON/查询参数小工具。
//
// 抽出来是为了让各 handler 文件(channels/users/tokens)共用同一套
// 分页解析、JSON 写出与 404 语义,避免三处各写一份慢慢漂移。
package controller

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/volcano-tts/tts-api/middleware"
)

// writeJSON 写 JSON 响应;编码失败时尽力返回 500(若 header 未发)。
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// header 已发,无法改状态码;记日志即可
		log.Printf("[controller] writeJSON encode failed: %v", err)
	}
}

// sendNotFound 统一 404 错误体(code=not_found,契约 §4.3)。
func sendNotFound(w http.ResponseWriter, message string) {
	middleware.SendJSONError(w, http.StatusNotFound, message, "invalid_request_error", "not_found")
}

// parseIntQuery 解析整型查询参数;缺失或非法时返回 def。
// 上限/下限的裁剪由调用方(parsePagination)负责。
func parseIntQuery(r *http.Request, key string, def int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return v
}
