package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// usage.status 取值(阶段 3b 计费两态)。
const (
	UsageStatusSettled  = 1 // 合成成功并结算
	UsageStatusRefunded = 2 // 合成失败,预扣已全额退回
)

// Usage 是 usage 表的一行(计量流水,只增不改)。
type Usage struct {
	ID         int64  `json:"id"`
	UserID     int64  `json:"user_id"`
	TokenID    int64  `json:"token_id"`
	ChannelID  *int64 `json:"channel_id,omitempty"` // 兜底场景为 nil
	RequestID  string `json:"request_id"`
	Model      string `json:"model"`
	Characters int    `json:"characters"`
	AudioMS    int64  `json:"audio_ms"`
	AudioBytes int    `json:"audio_bytes"`
	// 阶段 3b 计费列
	PreConsumed int64 `json:"pre_consumed"` // 预扣额度(退款按此全额回退)
	ActualQuota int64 `json:"actual_quota"` // 结算后的实际额度(退款行为 0)
	Status      int   `json:"status"`       // 1=settled 2=refunded
	CreatedAt   string `json:"created_at"`
}

// UsageInsert 写一条流水;channel_id 为 nil 时落 NULL(走兜底)。
// 写失败不阻断主请求(由 controller 决定 log warn,见 3a 决策 #7)。
//
// 阶段 3b:带计费列。Status 为 0 时按 1(settled)处理,保持 3a 老调用点语义。
// 计费路径请优先用 UsageSettle / UsageRefund(它们把额度变动与流水放在同一事务)。
func (s *Store) UsageInsert(u Usage) (int64, error) {
	if u.UserID == 0 || u.TokenID == 0 {
		return 0, fmt.Errorf("%w: user_id and token_id are required", ErrInvalid)
	}
	if u.RequestID == "" {
		return 0, fmt.Errorf("%w: request_id is required", ErrInvalid)
	}
	if u.Status == 0 {
		u.Status = UsageStatusSettled
	}
	var channelVal any
	if u.ChannelID != nil {
		channelVal = *u.ChannelID
	} else {
		channelVal = nil
	}
	res, err := s.db.Exec(`
		INSERT INTO usage (user_id, token_id, channel_id, request_id, model, characters, audio_ms, audio_bytes, pre_consumed, actual_quota, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))`,
		u.UserID, u.TokenID, channelVal, u.RequestID, u.Model, u.Characters, u.AudioMS, u.AudioBytes,
		u.PreConsumed, u.ActualQuota, u.Status)
	if err != nil {
		return 0, fmt.Errorf("store: usage insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: usage insert lastid: %w", err)
	}
	return id, nil
}

// usageColumns 是 usage 表的统一 SELECT 列顺序;UsageSettle/Refund/List 共用,
// 避免三处 scan 顺序漂移。
const usageColumns = `id, user_id, token_id, channel_id, request_id, model, characters, audio_ms, audio_bytes, pre_consumed, actual_quota, status, created_at`

// scanUsage 把一行扫成 Usage(列顺序见 usageColumns)。
func scanUsage(r scanner) (Usage, error) {
	var u Usage
	var channelID *int64
	if err := r.Scan(&u.ID, &u.UserID, &u.TokenID, &channelID, &u.RequestID, &u.Model,
		&u.Characters, &u.AudioMS, &u.AudioBytes, &u.PreConsumed, &u.ActualQuota, &u.Status, &u.CreatedAt); err != nil {
		return u, err
	}
	u.ChannelID = channelID
	return u, nil
}

// UsageListByUser 按 user_id 列流水;ORDER BY id DESC(最新在前);LIMIT/OFFSET 分页。
// 决策 #9:limit/offset 形式,跟项目其它 CRUD 一致。
func (s *Store) UsageListByUser(userID int64, limit, offset int) ([]Usage, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.Query(`SELECT `+usageColumns+`
	                        FROM usage WHERE user_id = ? ORDER BY id DESC LIMIT ? OFFSET ?`,
		userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: usage list by user id=%d: %w", userID, err)
	}
	defer rows.Close()
	out := make([]Usage, 0, 16)
	for rows.Next() {
		u, err := scanUsage(rows)
		if err != nil {
			return nil, fmt.Errorf("store: usage list scan: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: usage list rows: %w", err)
	}
	return out, nil
}

// UsageCountByUser 统计该 user 的总流水行数(分页辅助)。
func (s *Store) UsageCountByUser(userID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM usage WHERE user_id = ?`, userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: usage count by user id=%d: %w", userID, err)
	}
	return n, nil
}

// UsageTotals 是聚合视图(含阶段 3b 计费列)。
type UsageTotals struct {
	Characters  int64 `json:"characters"`
	AudioMS     int64 `json:"audio_ms"`
	AudioBytes  int64 `json:"audio_bytes"`
	ActualQuota int64 `json:"actual_quota"` // 只累加 settled 行(退款行 actual=0,不重复计)
}

// UsageTotalsByUser 聚合该 user 的 characters / audio_ms / audio_bytes / actual_quota 总和。
// 管理端仪表盘展示用。
func (s *Store) UsageTotalsByUser(userID int64) (UsageTotals, error) {
	var t UsageTotals
	err := s.db.QueryRow(`SELECT COALESCE(SUM(characters),0), COALESCE(SUM(audio_ms),0),
	                             COALESCE(SUM(audio_bytes),0), COALESCE(SUM(actual_quota),0)
	                      FROM usage WHERE user_id = ?`, userID).
		Scan(&t.Characters, &t.AudioMS, &t.AudioBytes, &t.ActualQuota)
	if err != nil {
		return t, fmt.Errorf("store: usage totals by user id=%d: %w", userID, err)
	}
	return t, nil
}

// ---- 阶段 3b:结算 / 退款(额度变动 + 流水同一事务) --------------------

// SettleInput 是一次结算的完整输入。
type SettleInput struct {
	UserID  int64
	TokenID int64
	Pre     int64 // 已预扣额
	Actual  int64 // 实际应扣额
	Usage   Usage // 除 PreConsumed/ActualQuota/Status 外的流水字段
}

// UsageSettle 结算:多退少补 + 记流水(status=1),单事务。
//
// 动作(全在一个 tx 内):
//  1. tokens.remain_quota += (Pre - Actual)   // Actual>Pre 时为负数=补扣
//  2. tokens.used_quota   += Actual
//  3. users.used_quota    += Actual           // 聚合对账(决策 #3)
//  4. INSERT usage(pre_consumed=Pre, actual_quota=Actual, status=1)
//
// unlimited token:调用方(billing)已短路,不会走到这里改额度;
// 但若传了 Pre=Actual=0,本函数仍会插入一条 status=1 的 usage(计量保留)。
func (s *Store) UsageSettle(in SettleInput) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: usage settle begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1) 多退少补(Pre - Actual 可为负 = 补扣)
	if _, err := tx.Exec(`UPDATE tokens SET remain_quota = remain_quota + (? - ?), updated_at = datetime('now') WHERE id = ?`,
		in.Pre, in.Actual, in.TokenID); err != nil {
		return fmt.Errorf("store: settle adjust token quota: %w", err)
	}
	// 2) token 维度用量
	if _, err := tx.Exec(`UPDATE tokens SET used_quota = used_quota + ?, updated_at = datetime('now') WHERE id = ?`,
		in.Actual, in.TokenID); err != nil {
		return fmt.Errorf("store: settle token used_quota: %w", err)
	}
	// 3) user 维度聚合对账(users.quota 不参与,见 3b 决策 #3)
	if _, err := tx.Exec(`UPDATE users SET used_quota = used_quota + ?, updated_at = datetime('now') WHERE id = ?`,
		in.Actual, in.UserID); err != nil {
		return fmt.Errorf("store: settle user used_quota: %w", err)
	}
	// 4) 流水
	u := in.Usage
	u.UserID = in.UserID
	u.TokenID = in.TokenID
	u.PreConsumed = in.Pre
	u.ActualQuota = in.Actual
	u.Status = UsageStatusSettled
	var channelVal any
	if u.ChannelID != nil {
		channelVal = *u.ChannelID
	}
	if _, err := tx.Exec(`
		INSERT INTO usage (user_id, token_id, channel_id, request_id, model, characters, audio_ms, audio_bytes, pre_consumed, actual_quota, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))`,
		u.UserID, u.TokenID, channelVal, u.RequestID, u.Model, u.Characters, u.AudioMS, u.AudioBytes,
		u.PreConsumed, u.ActualQuota, u.Status); err != nil {
		return fmt.Errorf("store: settle insert usage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: usage settle commit: %w", err)
	}
	return nil
}

// UsageRefund 退款:把预扣全额退回 + 记流水(status=2),单事务且**幂等**。
//
// 幂等实现:先在 tx 内查同 request_id 是否已有 status=2 的退款行;
// 有则直接返回 nil(重复退款不再加额度、不重复插行)。
// 依赖 SQLite 单写串行 + SetMaxOpenConns(1),tx 内检查-写入不会被并发穿插。
//
// 动作:
//  1. 若已退款 → return nil
//  2. tokens.remain_quota += Pre
//  3. INSERT usage(pre_consumed=Pre, actual_quota=0, status=2)
func (s *Store) UsageRefund(userID, tokenID, pre int64, u Usage) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: usage refund begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var exists int
	err = tx.QueryRow(`SELECT 1 FROM usage WHERE request_id = ? AND status = ? LIMIT 1`,
		u.RequestID, UsageStatusRefunded).Scan(&exists)
	switch {
	case err == nil:
		// 已退款过 → 幂等返回
		return nil
	case errors.Is(err, sql.ErrNoRows):
		// 继续退款
	default:
		return fmt.Errorf("store: refund idempotency check: %w", err)
	}

	if _, err := tx.Exec(`UPDATE tokens SET remain_quota = remain_quota + ?, updated_at = datetime('now') WHERE id = ?`,
		pre, tokenID); err != nil {
		return fmt.Errorf("store: refund token quota: %w", err)
	}

	u.UserID = userID
	u.TokenID = tokenID
	u.PreConsumed = pre
	u.ActualQuota = 0
	u.Status = UsageStatusRefunded
	var channelVal any
	if u.ChannelID != nil {
		channelVal = *u.ChannelID
	}
	if _, err := tx.Exec(`
		INSERT INTO usage (user_id, token_id, channel_id, request_id, model, characters, audio_ms, audio_bytes, pre_consumed, actual_quota, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))`,
		u.UserID, u.TokenID, channelVal, u.RequestID, u.Model, u.Characters, u.AudioMS, u.AudioBytes,
		u.PreConsumed, u.ActualQuota, u.Status); err != nil {
		return fmt.Errorf("store: refund insert usage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: usage refund commit: %w", err)
	}
	return nil
}
