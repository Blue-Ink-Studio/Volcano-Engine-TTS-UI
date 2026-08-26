package middleware

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/volcano-tts/tts-api/setting"
)

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
