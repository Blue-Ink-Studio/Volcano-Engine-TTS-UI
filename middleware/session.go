// middleware/session.go — WebUI 会话鉴权中间件与身份上下文。
//
// 与业务线(API Key)严格分离(文档 §6 / §6.1):
//   - ValidateSession:只接受 JWT,服务 /api/me/*
//   - RequireAdminOrJWT:接受 admin_key **或** admin JWT,服务 /api/admin/* 与 /dashboard*
package middleware

import (
	"context"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/volcano-tts/tts-api/setting"
	"github.com/volcano-tts/tts-api/store"
)

// SessionCaller 是**会话线**识别出的身份。
//
// 为什么不复用业务线的 Caller:Caller 带 TokenID,而 JWT 身份没有 token 概念。
// 若图省事把 JWT 身份塞进 Caller、让 TokenID 留 0,会撞上 billing.PreConsume 的
// `tokenID == 0` 校验,产生"看起来像 bug"的行为。两条线各用各的结构(§6.1)。
//
// STAGE5:角色以 **DB 里的 role 为准**(契约 §6.1)——
// resolveSession 每次请求都回查用户行,所以改角色**立即生效**;
// JWT claims 里的 is_admin 只作展示,不参与鉴权(勿改成按 claims 判权)。
type SessionCaller struct {
	UserID       int64
	Name         string
	Role         string // owner / admin / user
	IsAdmin      bool   // 派生:RoleIsAdmin(Role)
	TokenVersion int64
}

type sessionCtxKey struct{}

// WithSessionCaller 把会话身份塞进 ctx(由 ValidateSession / RequireAdminOrJWT 调用)。
func WithSessionCaller(ctx context.Context, c *SessionCaller) context.Context {
	return context.WithValue(ctx, sessionCtxKey{}, c)
}

// SessionCallerFromContext 取会话身份;nil 表示该请求未经会话鉴权。
func SessionCallerFromContext(ctx context.Context) *SessionCaller {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(sessionCtxKey{}).(*SessionCaller)
	return c
}

// sessionStore 由 main 启动期注入。
//
// 为什么用注入而不是让 middleware import controller:controller 已经 import
// middleware,反向依赖会形成 import cycle。
var sessionStore *store.Store

// SetSessionStore 注入会话鉴权所需的 store;main 启动期调用一次。
// 与 controller.SetAdminStore 并列。
func SetSessionStore(s *store.Store) { sessionStore = s }

// 会话鉴权失败的细分 code(文档 §6 / §17)。
const (
	SessionCodeRequired  = "session_required"
	SessionCodeInvalid   = "session_invalid"
	SessionCodeRevoked   = "session_revoked"
	SessionCodeDisabled  = "user_disabled"
	SessionCodeAdminOnly = "admin_required"
)

// bearerToken 取 `Authorization: Bearer <tok>` 的值;非该形式返回 ""。
func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return ""
	}
	return strings.TrimSpace(auth[len(prefix):])
}

// resolveSession 校验 JWT 并回查数据库状态,返回会话身份。
// 失败时返回 (nil, HTTP 状态, 错误码)。
//
// 校验链:JWT 可解析 → user 存在 → status=1 → token_version 匹配(改密踢旧,D9)。
func resolveSession(r *http.Request) (*SessionCaller, int, string) {
	raw := bearerToken(r)
	if raw == "" {
		return nil, http.StatusUnauthorized, SessionCodeRequired
	}
	claims, err := ParseSessionToken(raw)
	if err != nil {
		return nil, http.StatusUnauthorized, SessionCodeInvalid
	}
	uid := claims.UserID()
	if uid == 0 {
		return nil, http.StatusUnauthorized, SessionCodeInvalid
	}
	if sessionStore == nil {
		return nil, http.StatusServiceUnavailable, "db_not_ready"
	}
	u, err := sessionStore.UserGet(uid)
	if err != nil {
		return nil, http.StatusUnauthorized, SessionCodeInvalid
	}
	if u.Status != 1 {
		return nil, http.StatusForbidden, SessionCodeDisabled
	}
	if u.TokenVersion != claims.Ver {
		return nil, http.StatusUnauthorized, SessionCodeRevoked
	}
	return &SessionCaller{
		UserID:       u.ID,
		Name:         u.Name,
		Role:         u.Role,
		IsAdmin:      u.IsAdmin, // store 层已按 role 派生
		TokenVersion: u.TokenVersion,
	}, 0, ""
}

// sessionMessage 给出与 code 匹配的英文 message(前端主要读 code,message 便于日志/排查)。
func sessionMessage(code string) string {
	switch code {
	case SessionCodeRequired:
		return "session token required"
	case SessionCodeRevoked:
		return "session revoked; please sign in again"
	case SessionCodeDisabled:
		return "user disabled"
	case SessionCodeAdminOnly:
		return "admin privilege required"
	default:
		return "invalid session token"
	}
}

// denySession 写统一错误体(形状与 SendJSONError 一致)。
func denySession(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	log.Printf("[session_auth] 拒绝 - 路径=%s 客户端=%s code=%s", r.URL.Path, GetClientIP(r), code)
	SendJSONError(w, status, message, "authentication_error", code)
}

// ValidateSession 是 /api/me/* 的鉴权中间件:**只接受 JWT**。
// admin_key 在此不可用(决策 #14);admin 的 JWT 可用(决策 D28)。
func ValidateSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		sc, status, code := resolveSession(r)
		if sc == nil {
			denySession(w, r, status, code, sessionMessage(code))
			return
		}
		next.ServeHTTP(w, r.WithContext(WithSessionCaller(r.Context(), sc)))
	})
}

// RequireAdminOrJWT 是管理接口(/api/admin/*、/dashboard*)的统一鉴权层(决策 #16)。
//
// 判定顺序:先按 JWT 解析(会话线)→ 不通过再走 admin_key 常量时间比较(兼容线)。
//   - 合法 JWT 且 is_admin=true → 放行(身份进 ctx)
//   - 合法 JWT 但 is_admin=false → 403 admin_required(凭证有效、权限不够;不是 401)
//   - 合法 JWT 但用户被禁用 / 被改密踢旧 → 明确回报 user_disabled / session_revoked
//   - 两者皆不通过 → 401 admin_required(沿用 3b 的 code,前端零改动)
func RequireAdminOrJWT(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		if raw := bearerToken(r); raw != "" {
			if _, err := ParseSessionToken(raw); err == nil {
				// 是合法 JWT → 走会话线
				sc, status, code := resolveSession(r)
				switch {
				case sc != nil && sc.IsAdmin:
					next.ServeHTTP(w, r.WithContext(WithSessionCaller(r.Context(), sc)))
					return
				case sc != nil:
					denySession(w, r, http.StatusForbidden, SessionCodeAdminOnly, sessionMessage(SessionCodeAdminOnly))
					return
				case code != SessionCodeInvalid && code != SessionCodeRequired:
					// 可解析但用户不可用(禁用/改密踢旧/DB 未就绪)→ 明确回报
					denySession(w, r, status, code, sessionMessage(code))
					return
				}
				// 落到这里:JWT 可解析但回查判为 invalid(如 user 已删除)→ 保守回退 admin_key
			}
		}

		// 回退:admin_key 兼容通道
		keys := setting.GetAdminKeys()
		if len(keys) == 0 {
			denyAdmin(w, r)
			return
		}
		if !inAPIKeyList(bearerToken(r), keys) {
			denyAdmin(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// BearerTokenOf 导出 Authorization: Bearer <tok> 的取值,供 controller 复用
// (例如 setup-password 的 admin_key 通道需要自行取凭证做通道判断)。
func BearerTokenOf(r *http.Request) string { return bearerToken(r) }

// MatchesAdminKey 判断给定凭证是否命中当前管理凭证列表(常量时间比较)。
//
// 供 setup-password 的 admin_key 通道复用(决策 D23),避免在 controller 里
// 重写一遍比较逻辑而造成安全口径漂移。
func MatchesAdminKey(token string) bool {
	if token == "" {
		return false
	}
	keys := setting.GetAdminKeys()
	if len(keys) == 0 {
		return false
	}
	return inAPIKeyList(token, keys)
}

// RequireOwnerForRequest 是 **owner 权限的字段级判定入口**(STAGE5 定案 A2)。
//
// 为什么不写成中间件:本期唯一需要 owner 权限的操作是"把某人转让为 owner",
// 而它发生在**多用途端点** `PATCH /api/admin/users/{id}` 里 —— 该端点同时要允许
// admin 改 `user_group` / 把用户提为 admin。若给整个端点挂 RequireOwner,
// admin 连改 user_group 都会被 403,直接违反权限矩阵。
//
// 因此语义集中在这里、由 handler **按字段条件调用**:
//
//	admin_key 通道(通过 RequireAdminOrJWT 后 ctx 里没有 SessionCaller)⇒ 等价 owner,放行;
//	会话线 ⇒ 必须 role == 'owner'。
func RequireOwnerForRequest(r *http.Request) bool {
	sc := SessionCallerFromContext(r.Context())
	return sc == nil || sc.Role == store.RoleOwner
}

// IsLocalRequest 判断请求是否来自**本机**(决策 D32,用于首次设密通道)。
// ⚠️ 只读 socket peer(r.RemoteAddr),**绝不用** GetClientIP:
// 它在默认启发式模式下会解析 X-Forwarded-For,并在链上全是私网时兜底采信
// X-Real-IP;精确模式下按跳数回走且不过滤回环 —— 两条路都可能让攻击者
// 伪造出 127.0.0.1,从而绕过"仅本机可首次设密"的限制。
//
// 已知取舍:同机反代(如 nginx)时所有公网请求的 RemoteAddr 都是 127.0.0.1,
// 本机判定对所有人成立 —— 由 admin_key 通道 + "默认关闭注册"双防线兜底(D32 已接受)。
func IsLocalRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
