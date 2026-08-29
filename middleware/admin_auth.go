package middleware

import (
	"log"
	"net/http"
	"strings"

	"github.com/volcano-tts/tts-api/common"
	"github.com/volcano-tts/tts-api/setting"
)

// RequireAdmin 是 /admin 路由的鉴权中间件,复用 OPENAI_TTS_API_KEY。
// 行为:
//   - Auth.APIKeys 为空 → 所有请求放行(等同无鉴权)
//   - Authorization 头 Bearer token 在列表中 → 放行
//   - 其它 → 401 + JSON {error: 'unauthorized', code: 'admin_auth_failed'}
//
// 设计: 与现有 /v1/audio/speech 用的 setting.Auth 共享同一份 keys,
// 用户只用管一个 env 变量(OPENAI_TTS_API_KEY)。
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 预检: 跨域/OPTIONS 直接放行(让浏览器能发 preflight)
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		keys := setting.Auth.APIKeys
		if len(keys) == 0 {
			// 没配 admin key,等同无鉴权
			next.ServeHTTP(w, r)
			return
		}

		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			denyAdmin(w, r)
			return
		}
		token := strings.TrimSpace(auth[len(prefix):])
		if !inAPIKeyList(token, keys) {
			denyAdmin(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// inAPIKeyList 用常量时间比较,防 token 计时攻击。
// 单个 key 也走同一条路径,无差别处理。
func inAPIKeyList(token string, keys []string) bool {
	if token == "" {
		return false
	}
	match := false
	for _, k := range keys {
		if common.SecureEqualString(token, k) {
			match = true
			// 不 break,继续遍历,保持时间恒定
		}
	}
	return match
}

// denyAdmin 写 401 + JSON 错误体,记录客户端 IP。
func denyAdmin(w http.ResponseWriter, r *http.Request) {
	log.Printf("[admin_auth] 鉴权失败 - 路径=%s 客户端=%s", r.URL.Path, GetClientIP(r))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("WWW-Authenticate", `Bearer realm="tts-admin"`)
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"code":"admin_auth_failed","message":"unauthorized","type":"authentication_error"}}`))
}
