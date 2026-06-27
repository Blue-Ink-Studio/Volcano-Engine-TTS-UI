package middleware

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
)

var validAPIKeys []string

func InitAPIKeys() {
	apiKey := os.Getenv("OPENAI_TTS_API_KEY")
	if apiKey != "" {
		validAPIKeys = strings.Split(apiKey, ",")
		for i, k := range validAPIKeys {
			validAPIKeys[i] = strings.TrimSpace(k)
		}
		log.Printf("已配置 %d 个有效的 API 密钥", len(validAPIKeys))
	} else {
		log.Println("警告: OPENAI_TTS_API_KEY环境变量未设置，所有请求将无需认证即可访问")
		log.Println("如需启用API密钥验证，请设置 OPENAI_TTS_API_KEY 环境变量（多个密钥用逗号分隔）")
	}
}

func ValidateAPIKey(r *http.Request) bool {
	if len(validAPIKeys) == 0 {
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
	for _, validKey := range validAPIKeys {
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
