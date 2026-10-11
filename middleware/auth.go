package middleware

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/volcano-tts/tts-api/setting"
	"github.com/volcano-tts/tts-api/store"
)

// AuthError 是 ValidateAPIKey 失败时的细分语义,供 controller 翻译成不同 HTTP code。
//   - CodeInvalidKey: 未知 / 不匹配(401)
//   - CodeTokenDisabled: token.status=2(403)
//   - CodeUserDisabled: user.status=2(403)
type AuthError struct {
	Code string
}

func (e *AuthError) Error() string { return "auth: " + e.Code }

const (
	CodeInvalidKey    = "invalid_api_key"
	CodeTokenDisabled = "token_disabled"
	CodeUserDisabled  = "user_disabled"
)

// Caller 是识别后的身份(塞进 request context)。
// 决策 B:结构体携带所有身份字段,以后扩展(分组、租户等)只改这里。
type Caller struct {
	UserID  int64
	TokenID int64
}

type authCtxKey struct{}

// WithCaller 把身份塞进 ctx(由中间件调用)。
func WithCaller(ctx context.Context, c *Caller) context.Context {
	return context.WithValue(ctx, authCtxKey{}, c)
}

// CallerFromContext 从 ctx 拿身份;nil 表示未识别(setup 模式或匿名)。
func CallerFromContext(ctx context.Context) *Caller {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(authCtxKey{}).(*Caller)
	return c
}

// ValidateAPIKey 业务鉴权入口(setup 模式 fallback,行为与阶段 0/1/2 一致)。
//
// 决策 #6:setup 模式(installer.ModeSetup)下,走 setting.GetAuthAPIKeys() 原逻辑:
//   - 无 key 配 → 放行(让 setup 流程内部能调通)
//   - 有 key 配 → Bearer token 常量时间比较
//
// 非 setup 模式(controller 已切到 AuthenticateWithStore),本函数不直接被调用,
// 但保留作为 fallback / 单测 helper / 未来需求(比如 401 时区分 setup/normal)。
func ValidateAPIKey(r *http.Request) bool {
	keys := setting.GetAuthAPIKeys()
	if len(keys) == 0 {
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
	for _, validKey := range keys {
		if subtle.ConstantTimeCompare([]byte(token), []byte(validKey)) == 1 {
			return true
		}
	}
	return false
}

// AuthenticateWithStore 业务鉴权(normal 模式)。
//
// 流程:
//   1) Bearer 头缺失或格式错 → CodeInvalidKey
//   2) sha256(token) 查 tokens 表 → 未命中 → CodeInvalidKey
//   3) token.status != 1 → CodeTokenDisabled
//   4) 查 user(SELECT 一次),user.status != 1 → CodeUserDisabled
//   5) 成功 → 返 *Caller(身份)
//
// 设计要点:
//   - 走常量时间 hash 比较(防止 hash 字典侧信道,虽然 hash 不直接暴露;但
//     库统一风格)
//   - 不在 SQL 里 JOIN users,因为 status 分两步校验语义清晰(决策 #3)
//   - caller 已经在前面对照过 token+user,调用方拿到 *Caller 即可放心用
func AuthenticateWithStore(r *http.Request, s *store.Store) (*Caller, *AuthError) {
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return nil, &AuthError{Code: CodeInvalidKey}
	}
	token := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	if token == "" {
		return nil, &AuthError{Code: CodeInvalidKey}
	}
	hash := store.HashKey(token)
	row, err := s.TokenGetByKeyHash(hash)
	if err != nil {
		// ErrNotFound → 401;其它 DB 错 → 同样视为鉴权失败(不要泄露 DB 错误)
		return nil, &AuthError{Code: CodeInvalidKey}
	}
	if row.Status != 1 {
		return nil, &AuthError{Code: CodeTokenDisabled}
	}
	user, err := s.UserGet(row.UserID)
	if err != nil {
		// user 找不到(应该不会,FK 保证;但防御)→ 当 invalid 处理
		return nil, &AuthError{Code: CodeInvalidKey}
	}
	if user.Status != 1 {
		return nil, &AuthError{Code: CodeUserDisabled}
	}
	return &Caller{UserID: user.ID, TokenID: row.ID}, nil
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
