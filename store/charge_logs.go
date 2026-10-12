package store

import "fmt"

// ChargeLog 是 charge_logs 表的一行(资金流水 / 审计)。
//
// 只记录**余额来源**:充值(charge) / 兑换(redeem) / 手动调整(manual_adjust) / 赠送(grant)。
// 调用消费**不在**本表 —— 由 usage.actual_quota 覆盖,退款在 usage.status,不双写
// (决策 D25 / D33)。这样才能保证"余额可追溯"且不产生互相矛盾的两套消费账。
type ChargeLog struct {
	ID           int64  `json:"id"`
	UserID       int64  `json:"user_id"`
	Type         string `json:"type"`          // ChargeType*
	Delta        int64  `json:"delta"`         // 正负均可
	BalanceAfter int64  `json:"balance_after"` // 变更后余额
	Source       string `json:"source"`        // ChargeSource*
	Remark       string `json:"remark"`
	CreatedAt    string `json:"created_at"`
}

// ChargeLogListByUser 列某 user 的余额流水(ORDER BY id DESC + 分页)。
//
// typ 非空时按 type 过滤(决策 X22);limit 默认 20 / 上限 100,与
// controller 的 parsePagination 口径一致。
func (s *Store) ChargeLogListByUser(userID int64, typ string, limit, offset int) ([]ChargeLog, int, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	where := " WHERE user_id = ?"
	args := []any{userID}
	if typ != "" {
		where += " AND type = ?"
		args = append(args, typ)
	}

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM charge_logs`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: charge log count user=%d: %w", userID, err)
	}

	q := `SELECT id, user_id, type, delta, balance_after, source, remark, created_at
	      FROM charge_logs` + where + ` ORDER BY id DESC LIMIT ? OFFSET ?`
	qArgs := append(append([]any{}, args...), limit, offset)
	rows, err := s.db.Query(q, qArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: charge log list user=%d: %w", userID, err)
	}
	defer rows.Close()

	out := make([]ChargeLog, 0, limit)
	for rows.Next() {
		var c ChargeLog
		if err := rows.Scan(&c.ID, &c.UserID, &c.Type, &c.Delta, &c.BalanceAfter,
			&c.Source, &c.Remark, &c.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("store: charge log scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: charge log rows: %w", err)
	}
	return out, total, nil
}

// PendingUsageCount 统计遗留的 status=0(预扣中)流水行数。
//
// 用途:崩溃 / 被 kill 后对账 —— 这些行的钱已从余额扣走但从未结算或退款。
// 本期先靠运维人工扫描处理,自动补偿记待办(§13)。
func (s *Store) PendingUsageCount() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM usage WHERE status = ?`,
		UsageStatusPending).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: pending usage count: %w", err)
	}
	return n, nil
}
