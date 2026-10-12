package store

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Redemption 是 redemptions 表的一行(充值码)。
//
// 一次性使用:used_by / used_at 非空即已兑换(决策 D20)。
// 本期不做过期(无 expires_at,决策 #5)。
type Redemption struct {
	ID        int64   `json:"id"`
	Code      string  `json:"code"`
	Quota     int64   `json:"quota"`
	UsedBy    *int64  `json:"used_by"` // NULL = 未使用
	UsedAt    *string `json:"used_at"`
	Remark    string  `json:"remark"`
	CreatedAt string  `json:"created_at"`
}

// ErrRedeemCodeUsed 表示充值码已被兑换过(→ HTTP 409 code_used)。
var ErrRedeemCodeUsed = errors.New("store: redemption code already used")

const (
	// RedeemCodeLen 是充值码长度(16 位大写字母 + 数字,决策 #5 / X20)。
	RedeemCodeLen = 16
	// RedeemMaxBatch 是单次批量生成的数量上限(X20)。
	RedeemMaxBatch = 1000
	// redeemGenRetry 是生成时命中 UNIQUE 冲突的重试次数(X20)。
	redeemGenRetry = 5
	// 字符集刻意排除易混淆字符?—— 决策要求"A-Z0-9",照此实现。
	redeemCodeCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
)

// generateRedeemCode 生成 RedeemCodeLen 位不可预测随机串。
//
// 用 crypto/rand + **拒绝采样**(丢弃 >= 252 的字节,因 252 = 36×7),
// 避免 v%36 的模偏差 —— 充值码等同现金,分布必须均匀。
func generateRedeemCode() (string, error) {
	out := make([]byte, 0, RedeemCodeLen)
	buf := make([]byte, RedeemCodeLen)
	for len(out) < RedeemCodeLen {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("store: generate redeem code: %w", err)
		}
		for _, v := range buf {
			if int(v) >= 252 {
				continue // 拒绝,保持均匀分布
			}
			out = append(out, redeemCodeCharset[int(v)%len(redeemCodeCharset)])
			if len(out) == RedeemCodeLen {
				break
			}
		}
	}
	return string(out), nil
}

// NormalizeRedeemCode 归一化用户输入的充值码:去空白 + 转大写(决策 #21:大小写不敏感)。
func NormalizeRedeemCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

// RedemptionCreateBatch 批量生成 count 个面额为 quota 的充值码,返回明文码列表。
//
// 决策 X20:单条命中 UNIQUE 冲突时就**该条**重试(上限 redeemGenRetry 次);
// 超过上限则整体回滚并报错 —— 不允许静默跳过,否则会出现"生成的码比要求少"。
// 整个批量在同一事务内,失败不留半批。
func (s *Store) RedemptionCreateBatch(quota int64, count int, remark string) ([]string, error) {
	if quota <= 0 {
		return nil, fmt.Errorf("%w: quota must be >= 1", ErrInvalid)
	}
	if count <= 0 {
		return nil, fmt.Errorf("%w: count must be >= 1", ErrInvalid)
	}
	if count > RedeemMaxBatch {
		return nil, fmt.Errorf("%w: count must be <= %d", ErrInvalid, RedeemMaxBatch)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: redemption batch begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	codes := make([]string, 0, count)
	for i := 0; i < count; i++ {
		var inserted bool
		for attempt := 0; attempt < redeemGenRetry && !inserted; attempt++ {
			code, err := generateRedeemCode()
			if err != nil {
				return nil, err
			}
			_, err = tx.Exec(`
				INSERT INTO redemptions (code, quota, remark, created_at)
				VALUES (?, ?, ?, datetime('now'))`, code, quota, remark)
			if err != nil {
				if isUniqueViolation(err) {
					continue // 撞码:换一个再来(X20)
				}
				return nil, fmt.Errorf("store: redemption insert: %w", err)
			}
			codes = append(codes, code)
			inserted = true
		}
		if !inserted {
			return nil, fmt.Errorf("store: redemption code generation exhausted %d retries", redeemGenRetry)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: redemption batch commit: %w", err)
	}
	return codes, nil
}

// RedemptionRedeemAtomic 原子兑换:占用充值码 + 给用户加余额,**同一事务**(X18 + D20)。
//
// 并发语义:UPDATE ... WHERE code=? AND used_by IS NULL + RowsAffected 判定,
// 同一码并发兑换只有一个能成功,其余得 ErrRedeemCodeUsed(→ 409)。
//
// 返回 (面额, 兑换后余额)。
func (s *Store) RedemptionRedeemAtomic(code string, userID int64) (int64, int64, error) {
	c := NormalizeRedeemCode(code)
	if len(c) != RedeemCodeLen {
		// 长度不符直接判不存在:不泄露"格式对不对"这类可用于枚举的信号
		return 0, 0, ErrNotFound
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("store: redeem begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1) 取未使用的码
	var quota int64
	err = tx.QueryRow(`SELECT quota FROM redemptions WHERE code = ? AND used_by IS NULL`, c).Scan(&quota)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 区分"码不存在"(404)与"码已用过"(409),便于前端给不同文案
		var used int
		if e2 := tx.QueryRow(`SELECT COUNT(*) FROM redemptions WHERE code = ?`, c).Scan(&used); e2 != nil {
			return 0, 0, fmt.Errorf("store: redeem lookup: %w", e2)
		}
		if used > 0 {
			return 0, 0, ErrRedeemCodeUsed
		}
		return 0, 0, ErrNotFound
	case err != nil:
		return 0, 0, fmt.Errorf("store: redeem select: %w", err)
	}

	// 2) 原子占用(条件更新 + RowsAffected 判定)
	res, err := tx.Exec(`
		UPDATE redemptions SET used_by = ?, used_at = datetime('now')
		WHERE code = ? AND used_by IS NULL`, userID, c)
	if err != nil {
		return 0, 0, fmt.Errorf("store: redeem claim: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, 0, ErrRedeemCodeUsed // 并发抢跑:别人先占了
	}

	// 3) 加余额 + 写 charge_logs(同事务)
	bal, err := s.UserAddBalanceTx(tx, userID, quota, ChargeTypeRedeem, ChargeSourceRedeem, "充值码兑换")
	if err != nil {
		return 0, 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("store: redeem commit: %w", err)
	}
	return quota, bal, nil
}

// RedemptionList 分页列出充值码(管理端用)。
// used 三态:"" 全部 / "0" 未使用 / "1" 已使用。
func (s *Store) RedemptionList(used string, limit, offset int) ([]Redemption, int, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	where := ""
	switch used {
	case "0":
		where = " WHERE used_by IS NULL"
	case "1":
		where = " WHERE used_by IS NOT NULL"
	}

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM redemptions` + where).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: redemption count: %w", err)
	}

	q := `SELECT id, code, quota, used_by, used_at, remark, created_at
	      FROM redemptions` + where + ` ORDER BY id DESC LIMIT ? OFFSET ?`
	rows, err := s.db.Query(q, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("store: redemption list: %w", err)
	}
	defer rows.Close()

	out := make([]Redemption, 0, limit)
	for rows.Next() {
		var r Redemption
		if err := rows.Scan(&r.ID, &r.Code, &r.Quota, &r.UsedBy, &r.UsedAt, &r.Remark, &r.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("store: redemption scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: redemption rows: %w", err)
	}
	return out, total, nil
}
