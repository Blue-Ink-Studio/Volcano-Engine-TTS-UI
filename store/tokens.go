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
	return insertToken(s.db, t)
}

// sqlExecer 抽象 db 与 tx 的 Exec,让 insertToken 可被普通插入与事务复用。
type sqlExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// insertToken 是 TokenInsert 与 SeedDefaultTokens 共用的核心插入(UNIQUE 冲突 → ErrDuplicate)。
func insertToken(ex sqlExecer, t Token) (int64, error) {
	res, err := ex.Exec(`
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

// TokenCountEnabled 统计 enabled token 数。
func (s *Store) TokenCountEnabled() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM tokens WHERE status = 1`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: token count enabled: %w", err)
	}
	return n, nil
}

// TokenCount 统计 tokens 表总行数(不限 status)。seed 触发判断用:全表空才 seed。
func (s *Store) TokenCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM tokens`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: token count: %w", err)
	}
	return n, nil
}

// SeedDefaultTokens 原子 seed:把 authKeys 逐个插成 defaultUserName 下的 unlimited token。
// 单一事务:default user + 全部 tokens 成功才 commit,任一失败整体回滚(决策 #3)。
//
// 行为契约(参考 SetupApply 事务风格):
//   - 默认 user 不存在则建(name=defaultUserName,status=1),已存在则复用;
//   - token 已存在(key_hash UNIQUE 冲突)→ 静默跳过(ErrDuplicate,事务继续),
//     兼容"同一 key 重复 seed"场景;
//   - 其它 token 错误 → 整体回滚;
//   - 全部成功 → tx.Commit,返回实际插入的 token 数(不含被 ErrDuplicate 跳过的)。
func (s *Store) SeedDefaultTokens(authKeys []string, defaultUserName string) (int, error) {
	name := strings.TrimSpace(defaultUserName)
	if name == "" {
		return 0, fmt.Errorf("%w: default user name is required", ErrInvalid)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: seed default tokens begin: %w", err)
	}
	// defer Rollback:Commit 成功时 Rollback 返 sql.ErrTxDone,无害。
	defer func() {
		_ = tx.Rollback()
	}()

	// 1) 建/复用 default user(INSERT OR IGNORE + SELECT id,不依赖 UserInsert 的 db.Exec)
	var uid int64
	if _, err := tx.Exec(`INSERT OR IGNORE INTO users (name, status, quota, used_quota, created_at, updated_at)
	                      VALUES (?, 1, 0, 0, datetime('now'), datetime('now'))`, name); err != nil {
		return 0, fmt.Errorf("store: seed default user insert: %w", err)
	}
	if err := tx.QueryRow(`SELECT id FROM users WHERE name = ?`, name).Scan(&uid); err != nil {
		return 0, fmt.Errorf("store: seed default user select: %w", err)
	}

	// 2) 逐个插 token;ErrDuplicate 跳过,其它整体回滚
	seeded := 0
	for _, key := range authKeys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		t := Token{
			UserID:    uid,
			Name:      "seeded",
			KeyHash:   HashKey(key),
			KeyPrefix: KeyPrefix(key),
			Unlimited: true, // 3a 不扣费,seed 出来全 unlimited
			Status:    1,
		}
		if _, err := insertToken(tx, t); err != nil {
			if errors.Is(err, ErrDuplicate) {
				continue
			}
			return seeded, fmt.Errorf("store: seed token (prefix=%s): %w", t.KeyPrefix, err)
		}
		seeded++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: seed default tokens commit: %w", err)
	}
	return seeded, nil
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
