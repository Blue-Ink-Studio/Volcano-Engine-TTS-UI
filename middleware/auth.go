package middleware

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/volcano-tts/tts-api/setting"
)

// InitAPIKeys 已在 setting.InitAuthConfig 中完成,这里保留为 no-op 以维持现有调用顺序。
// 实际鉴权逻辑直接读 setting.Auth.APIKeys。
func InitAPIKeys() {
	// 配置由 setting 包统一加载,日志也由 setting.LogStartupSummary 输出。
	_ = setting.Auth
}

func ValidateAPIKey(r *http.Request) bool {
	if len(setting.Auth.APIKeys) == 0 {
		return true
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return false
	}

	if !strings.HasPrefix(authHeader, "Bearer ") {
		return false
	}

	token := strings.TrimPrefix(authHeader, "Bearer ")
	for _, validKey := range setting.Auth.APIKeys {
		if subtle.ConstantTimeCompare([]byte(token), []byte(validKey)) == 1 {
			return true
		}
	}
	return false
}

func SendJSONError(w http.ResponseWriter, statusCode int, message string, errType string, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	})
}
