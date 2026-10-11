package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Token 是 tokens 表的一行。
type Token struct {
	ID          int64  `json:"id"`
	UserID      int64  `json:"user_id"`
	Name        string `json:"name"`
	KeyHash     string `json:"-"`              // 不暴露 hash 给前端
	KeyPrefix   string `json:"key_prefix"`     // 明文前 8 字符,对账识别
	RemainQuota int64  `json:"remain_quota"`   // 3b 用
	UsedQuota   int64  `json:"used_quota"`     // 3b 用
	Unlimited   bool   `json:"unlimited"`      // 1=不限
	Status      int    `json:"status"`         // 1=enabled 2=disabled
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// HashKey 返回 sha256(key) 的 hex 编码(64 字符小写)。key_hash 列存这个值。
// 决策 #1:用 sha256 + hex 字符串,索引友好。
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// KeyPrefix 返回 key 的前 8 字符(明文)。key_prefix 列存这个值,仅用于对账识别。
// key 长度 < 8 时返整个 key。
func KeyPrefix(key string) string {
	if len(key) <= 8 {
		return key
	}
	return key[:8]
}

// TokenInsert 新建 token;name + user_id 不强唯一(允许同 user 多 token),
// 但 key_hash UNIQUE → 同 key 不能插两次。
func (s *Store) TokenInsert(t Token) (int64, error) {
	t.Name = strings.TrimSpace(t.Name)
	t.KeyHash = strings.TrimSpace(t.KeyHash)
	t.KeyPrefix = strings.TrimSpace(t.KeyPrefix)
	if t.UserID == 0 {
		return 0, fmt.Errorf("%w: user_id is required", ErrInvalid)
	}
	if t.Name == "" {
		return 0, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	if t.KeyHash == "" {
		return 0, fmt.Errorf("%w: key_hash is required", ErrInvalid)
	}
	if t.Status == 0 {
		t.Status = 1
	}
	if t.Status != 1 && t.Status != 2 {
		return 0, fmt.Errorf("%w: status must be 1 or 2", ErrInvalid)
	}
	res, err := s.db.Exec(`
		INSERT INTO tokens (user_id, name, key_hash, key_prefix, remain_quota, used_quota, unlimited, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))`,
		t.UserID, t.Name, t.KeyHash, t.KeyPrefix, t.RemainQuota, t.UsedQuota,
		boolToInt(t.Unlimited), t.Status)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrDuplicate
		}
		return 0, fmt.Errorf("store: token insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: token insert lastid: %w", err)
	}
	return id, nil
}

// TokenGetByKeyHash 按 hash 查 token(无 JOIN user;调用方按 user_id 再查 user 状态)。
// 未命中 → ErrNotFound。
//
// 注意:不在此函数内 JOIN users,因为 status 校验分两步:
//   1) token.status=1  → 否则 403 token_disabled
//   2) user.status=1   → 否则 403 user_disabled
// 中间件顺序读两步,语义清晰。
func (s *Store) TokenGetByKeyHash(hash string) (*Token, error) {
	row := s.db.QueryRow(`SELECT id, user_id, name, key_hash, key_prefix, remain_quota, used_quota, unlimited, status, created_at, updated_at
	                     FROM tokens WHERE key_hash = ?`, strings.TrimSpace(hash))
	t, err := scanToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: token get by hash: %w", err)
	}
	return &t, nil
}

// TokenCountEnabled 统计 enabled token 数(供 main 启动 seed 决策:空才 seed)。
func (s *Store) TokenCountEnabled() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM tokens WHERE status = 1`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: token count enabled: %w", err)
	}
	return n, nil
}

func scanToken(r scanner) (Token, error) {
	var t Token
	var unlimited int
	err := r.Scan(&t.ID, &t.UserID, &t.Name, &t.KeyHash, &t.KeyPrefix,
		&t.RemainQuota, &t.UsedQuota, &unlimited, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return t, err
	}
	t.Unlimited = unlimited != 0
	return t, nil
}
