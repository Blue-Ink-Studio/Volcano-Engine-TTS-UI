// middleware/jwt.go — WebUI 会话凭证(JWT)的签发与校验。
//
// 两条鉴权线互不干扰(文档 §6):
//   - 业务线 ValidateAPIKey / AuthenticateWithStore:查 tokens 表(API Key)
//   - 会话线 ValidateSession(本文件 + session.go):查 JWT(WebUI 登录态)
//
// secret 来源与持久化见决策 X19 / X27:
//   - 默认落在**与 DB 同目录**的 jwt_secret,避免 CWD 不同导致每次重启换 secret
//     (那会让全体用户被动登出);
//   - 可用 TTS_JWT_SECRET_PATH 覆盖路径(与 TTS_DB_PATH / TTS_ADMIN_KEY 前缀一致);
//   - 文件为空或过短 → **fail-fast**,绝不静默重新生成。
package middleware

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// JWTAlg 是**唯一**允许的签名算法。
	// 解析时必须用 jwt.WithValidMethods 限定,防 alg 混淆攻击(如 alg=none)。
	JWTAlg = "HS256"

	// JWTTTL 是会话凭证有效期(决策 D9:24h;过期后前端 401 跳登录,本期不做无感刷新)。
	JWTTTL = 24 * time.Hour

	// jwtSecretRandBytes 是自动生成 secret 的随机字节数;以 hex 落盘 ⇒ 64 字符。
	jwtSecretRandBytes = 32

	// jwtSecretMinLen 是 secret 文件内容(trim 后)的最小字符数。
	jwtSecretMinLen = 32

	// EnvJWTSecretPath 覆盖 secret 文件路径(可选)。
	EnvJWTSecretPath = "TTS_JWT_SECRET_PATH"

	jwtSecretFileName = "jwt_secret"
)

// SessionClaims 是 WebUI 会话的 JWT claims(§4 JWT claims 契约)。
//
// Subject 承载 user_id(字符串);Ver 对应 users.token_version —— 改密 +1 后
// 旧 token 的 ver 不再匹配,ValidateSession 直接判 401 session_revoked(决策 D9)。
type SessionClaims struct {
	IsAdmin bool  `json:"is_admin"`
	Ver     int64 `json:"ver"`
	jwt.RegisteredClaims
}

// UserID 从 Subject 解析 user_id;解析失败返回 0。
func (c *SessionClaims) UserID() int64 {
	if c == nil {
		return 0
	}
	id, err := strconv.ParseInt(strings.TrimSpace(c.Subject), 10, 64)
	if err != nil {
		return 0
	}
	return id
}

var (
	jwtSecretOnce sync.Once
	jwtSecret     []byte
	jwtSecretErr  error
	jwtSecretPath string
)

// ResolveJWTSecretPath 决定 secret 文件路径。
//
// 顺序:env TTS_JWT_SECRET_PATH(非空)→ 与 DB 同目录的 jwt_secret。
// dbPath 为相对路径(如 "tts.db")时取 "." —— 与 DB 的相对基准保持一致。
func ResolveJWTSecretPath(dbPath string) string {
	if p := strings.TrimSpace(os.Getenv(EnvJWTSecretPath)); p != "" {
		return p
	}
	dir := filepath.Dir(strings.TrimSpace(dbPath))
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, jwtSecretFileName)
}

// InitJWTSecret 在启动期加载或生成 JWT secret。
//
// 失败**必须**由调用方 fail-fast(main 中 log.Fatalf):带着不可用的 secret 继续
// 服务,会让所有登录请求在签名时 panic 或静默全部失败。
func InitJWTSecret(dbPath string) error {
	jwtSecretOnce.Do(func() {
		path := ResolveJWTSecretPath(dbPath)
		jwtSecretPath = path
		secret, err := loadOrCreateJWTSecret(path)
		if err != nil {
			jwtSecretErr = err
			return
		}
		jwtSecret = secret
		log.Printf("[middleware] JWT secret 就绪: path=%s bytes=%d", path, len(secret))
	})
	return jwtSecretErr
}

// JWTSecretPath 返回本次实际使用的 secret 路径(只读,便于日志/诊断)。
func JWTSecretPath() string { return jwtSecretPath }

// loadOrCreateJWTSecret 读取现有 secret;不存在才生成。返回的字节即签名密钥。
func loadOrCreateJWTSecret(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		secret := bytes.TrimSpace(data)
		if len(secret) < jwtSecretMinLen {
			// 存在但为空/过短:拒绝静默替换 —— 换 secret 等于把所有人踢下线,
			// 必须让运维显式处理(X19)。
			return nil, fmt.Errorf(
				"middleware: jwt secret %q is too short (%d chars, need >= %d); refusing to regenerate silently",
				path, len(secret), jwtSecretMinLen)
		}
		return secret, nil

	case errors.Is(err, os.ErrNotExist):
		return createJWTSecret(path)

	default:
		return nil, fmt.Errorf("middleware: read jwt secret %q: %w", path, err)
	}
}

// createJWTSecret 生成 32 字节随机 secret 并以 hex 原子落盘。
//
// 为什么用 hex 文本而不是裸字节:读取侧要 TrimSpace,裸二进制里出现空白字节会被削掉,
// 导致"同一个文件读出不同 secret"这种极难排查的问题。
//
// 原子写:临时文件 → 0644(实际由 umask/Win 决定)→ Rename。
// **不自动创建父目录**(X19):路径写错时宁可启动失败,也不要把凭证落到意外位置。
func createJWTSecret(path string) ([]byte, error) {
	dir := filepath.Dir(path)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("middleware: jwt secret dir %q does not exist; refusing to create it (set %s or create the dir)", dir, EnvJWTSecretPath)
	}

	raw := make([]byte, jwtSecretRandBytes)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("middleware: generate jwt secret: %w", err)
	}
	secret := []byte(hex.EncodeToString(raw)) // 64 字符

	tmp, err := os.CreateTemp(dir, ".jwt_secret-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("middleware: create temp jwt secret in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := tmp.Write(secret); err != nil {
		cleanup()
		return nil, fmt.Errorf("middleware: write jwt secret: %w", err)
	}
	tryChmod0600(tmpName) // Windows 上基本是 no-op(D30 已接受)
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return nil, fmt.Errorf("middleware: close jwt secret: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return nil, fmt.Errorf("middleware: install jwt secret %q: %w", path, err)
	}
	log.Printf("[middleware] 已生成新的 JWT secret: %s(请勿删除;删除或改动会使所有登录态失效)", path)
	return secret, nil
}

// tryChmod0600 尽力把文件权限收紧到 0600。
// Windows 上 Go 的 Chmod 基本是 no-op(只影响只读位)—— 决策 D30 已接受(生产以 Linux 为主)。
func tryChmod0600(path string) {
	if err := os.Chmod(path, 0o600); err != nil {
		log.Printf("[middleware] 收紧 jwt secret 权限失败(%s): %v", path, err)
	}
}

// IssueSessionToken 为用户签发会话 JWT。
func IssueSessionToken(userID int64, isAdmin bool, tokenVersion int64) (string, error) {
	if len(jwtSecret) == 0 {
		return "", errors.New("middleware: jwt secret not initialized")
	}
	now := time.Now()
	claims := &SessionClaims{
		IsAdmin: isAdmin,
		Ver:     tokenVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.FormatInt(userID, 10),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(JWTTTL)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(jwtSecret)
}

// ParseSessionToken 解析并校验会话 JWT(签名算法、有效期)。
//
// **必须**带 WithValidMethods 限定算法:否则攻击者可改 header 的 alg 绕过校验
// (alg 混淆 / alg=none)。
func ParseSessionToken(raw string) (*SessionClaims, error) {
	if len(jwtSecret) == 0 {
		return nil, errors.New("middleware: jwt secret not initialized")
	}
	claims := &SessionClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) {
		return jwtSecret, nil
	}, jwt.WithValidMethods([]string{JWTAlg}))
	if err != nil {
		return nil, err
	}
	return claims, nil
}
