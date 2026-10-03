package middleware

import (
	"log"
	"net/http"
	"strings"

	"github.com/volcano-tts/tts-api/common"
	"github.com/volcano-tts/tts-api/setting"
)

// RequireAdmin 是管理接口(/api/admin/*、/api/voices*、/api/settings*)的鉴权中间件。
//
// 凭证来源:setting.GetAdminKeys(),优先级 admin_key(DB) > auth_key(DB) > OPENAI_TTS_API_KEY(env)。
// 与业务侧鉴权(middleware.ValidateAPIKey,只看 auth_key)分离,实现权限隔离:
// 配置了独立 admin_key 后,业务调用方持有的 key 无法访问管理接口。
//
// 行为:
//   - OPTIONS 预检 → 放行(浏览器预检不带 Authorization)
//   - 未配置任何凭证 → **拒绝**(401)。这是刻意设计:v0.3.0 之前这里直接放行,
//     导致管理接口在"没配 key"的部署上完全裸奔,并让后续给 /metrics、/health
//     套本中间件的加固形同虚设。启动期已由 main.go 做 fail-fast 校验。
//   - Authorization 头 Bearer token 命中凭证列表 → 放行
//   - 其它 → 401 + JSON {error: {code: 'admin_auth_failed'}}
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 预检: 跨域/OPTIONS 直接放行(让浏览器能发 preflight)
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		keys := setting.GetAdminKeys()
		if len(keys) == 0 {
			// 没配管理凭证 → 拒绝(旧行为是放行,见上方注释说明为何改掉)
			log.Printf("[admin_auth] 拒绝:未配置管理凭证(admin_key/auth_key/OPENAI_TTS_API_KEY 均为空) - 路径=%s 客户端=%s",
				r.URL.Path, GetClientIP(r))
			denyAdmin(w, r)
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
