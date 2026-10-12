package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// usage.status 取值。
//
// STAGE4 引入 0=pending(预扣中):预扣时插入的行处于该状态,
// 结算推进为 1、退款推进为 2 —— 只能单向前进(决策 X26)。
const (
	UsageStatusPending  = 0 // STAGE4:预扣中(尚未结算/退款)
	UsageStatusSettled  = 1 // 合成成功并结算
	UsageStatusRefunded = 2 // 合成失败,预扣已全额退回
)

// STAGE4 计费错误(由 billing 翻译成不同的 402 code / controller 决定 HTTP 状态)。
var (
	// ErrBalanceNegative:余额为负 = 已欠费停服(决策 X24)。→ 402 balance_negative
	ErrBalanceNegative = errors.New("store: balance is negative")
	// ErrTokenLimitExceeded:token 的 limit_quota 已用尽。→ 402 token_limit_exceeded
	ErrTokenLimitExceeded = errors.New("store: token limit exceeded")
	// ErrUsageStateConflict:usage 行状态与本次操作不兼容
	// (如"已结算再退款"),属数据不一致,不应静默重试。
	ErrUsageStateConflict = errors.New("store: usage state conflict")
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
	// UpstreamReqID 是上游(火山)返回的 ReqID,仅用于对账追溯。
	// 注意:定位键始终是 RequestID(本地请求号),整条生命周期内不变 ——
	// 否则结算后再按原键查行会查不到,状态机无法区分"已结算"与"不存在"。
	UpstreamReqID string `json:"upstream_req_id,omitempty"`
	Status        int    `json:"status"` // 0=pending 1=settled 2=refunded
	CreatedAt     string `json:"created_at"`
}

// UsageInsert 写一条流水;channel_id 为 nil 时落 NULL(走兜底)。
// 写失败不阻断主请求(由 controller 决定 log warn,见 3a 决策 #7)。
//
// ⚠️ STAGE4 行为变更:status=0 现在是**合法值**(预扣中),因此
// 3b 的「Status 为 0 时按 1(settled)处理」兜底已**移除**(决策 X26 连带项 1)。
// 未显式设置 Status 即视为 pending —— 这正确反映了"刚插入、尚未结算"的事实。
// 计费路径请优先用 UserPreConsume / UsageSettle / UsageRefund。
func (s *Store) UsageInsert(u Usage) (int64, error) {
	if u.UserID == 0 || u.TokenID == 0 {
		return 0, fmt.Errorf("%w: user_id and token_id are required", ErrInvalid)
	}
	if u.RequestID == "" {
		return 0, fmt.Errorf("%w: request_id is required", ErrInvalid)
	}
	if u.Status != UsageStatusPending && u.Status != UsageStatusSettled && u.Status != UsageStatusRefunded {
		return 0, fmt.Errorf("%w: status must be 0 (pending), 1 (settled) or 2 (refunded)", ErrInvalid)
	}
	var channelVal any
	if u.ChannelID != nil {
		channelVal = *u.ChannelID
	} else {
		channelVal = nil
	}
	res, err := s.db.Exec(`
		INSERT INTO usage (user_id, token_id, channel_id, request_id, upstream_req_id, model, characters, audio_ms, audio_bytes, pre_consumed, actual_quota, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))`,
		u.UserID, u.TokenID, channelVal, u.RequestID, u.UpstreamReqID, u.Model, u.Characters, u.AudioMS, u.AudioBytes,
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
const usageColumns = `id, user_id, token_id, channel_id, request_id, upstream_req_id, model, characters, audio_ms, audio_bytes, pre_consumed, actual_quota, status, created_at`

// scanUsage 把一行扫成 Usage(列顺序见 usageColumns)。
func scanUsage(r scanner) (Usage, error) {
	var u Usage
	var channelID *int64
	if err := r.Scan(&u.ID, &u.UserID, &u.TokenID, &channelID, &u.RequestID, &u.UpstreamReqID, &u.Model,
		&u.Characters, &u.AudioMS, &u.AudioBytes, &u.PreConsumed, &u.ActualQuota, &u.Status, &u.CreatedAt); err != nil {
		return u, err
	}
	u.ChannelID = channelID
	return u, nil
}

// statsTZOffset 是统计与筛选使用的时区偏移(决策 X17)。
//
// created_at 由 datetime('now') 产生,是 **UTC**;按"北京时间日界"切分必须显式加偏移,
// 否则北京时间 07:00 的调用会被算进前一天,趋势柱状图整体错位 8 小时。
const statsTZOffset = "+8 hours"

// UsageFilter 是自助接口的流水筛选条件(决策 X22)。
//
// ⚠️ UserID 必须由调用方从 JWT 取,**禁止**由查询参数指定(越权面)。
type UsageFilter struct {
	UserID  int64
	TokenID *int64 // 可选:限定某个令牌
	Status  *int   // 可选:0=pending 1=settled 2=refunded
	From    string // 可选:'YYYY-MM-DD'(北京时间日界)
	To      string // 可选:'YYYY-MM-DD'(含当日)
	Limit   int
	Offset  int
}

// usageWhere 依据筛选条件拼 WHERE 片段与参数(始终以 user_id 起头)。
func usageWhere(f UsageFilter) (string, []any) {
	where := " WHERE user_id = ?"
	args := []any{f.UserID}
	if f.TokenID != nil {
		where += " AND token_id = ?"
		args = append(args, *f.TokenID)
	}
	if f.Status != nil {
		where += " AND status = ?"
		args = append(args, *f.Status)
	}
	if f.From != "" {
		where += " AND date(created_at, ?) >= ?"
		args = append(args, statsTZOffset, f.From)
	}
	if f.To != "" {
		where += " AND date(created_at, ?) <= ?"
		args = append(args, statsTZOffset, f.To)
	}
	return where, args
}

// UsageListFiltered 按筛选条件列流水(ORDER BY id DESC),并返回满足条件的总数。
// limit 默认 20 / 上限 100,与 controller 的 parsePagination 口径一致。
func (s *Store) UsageListFiltered(f UsageFilter) ([]Usage, int, error) {
	limit := f.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	where, args := usageWhere(f)

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM usage`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: usage count filtered: %w", err)
	}

	q := `SELECT ` + usageColumns + ` FROM usage` + where + ` ORDER BY id DESC LIMIT ? OFFSET ?`
	qArgs := append(append([]any{}, args...), limit, offset)
	rows, err := s.db.Query(q, qArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: usage list filtered: %w", err)
	}
	defer rows.Close()
	out := make([]Usage, 0, limit)
	for rows.Next() {
		u, err := scanUsage(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("store: usage list filtered scan: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: usage list filtered rows: %w", err)
	}
	return out, total, nil
}

// UsageListByUser 按 user_id 列流水(不分页筛选的便捷版本,保留给既有调用方)。
func (s *Store) UsageListByUser(userID int64, limit, offset int) ([]Usage, error) {
	items, _, err := s.UsageListFiltered(UsageFilter{UserID: userID, Limit: limit, Offset: offset})
	return items, err
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
	Requests    int64 `json:"requests"`     // STAGE4:已结算的调用次数(不含 pending/refunded)
}

// UsageTotalsByUser 聚合该 user 的 characters / audio_ms / audio_bytes / actual_quota 总和。
// 管理端仪表盘与门户统计卡展示用。
func (s *Store) UsageTotalsByUser(userID int64) (UsageTotals, error) {
	var t UsageTotals
	err := s.db.QueryRow(`SELECT COALESCE(SUM(characters),0), COALESCE(SUM(audio_ms),0),
	                             COALESCE(SUM(audio_bytes),0), COALESCE(SUM(actual_quota),0),
	                             COUNT(CASE WHEN status = ? THEN 1 END)
	                      FROM usage WHERE user_id = ?`, UsageStatusSettled, userID).
		Scan(&t.Characters, &t.AudioMS, &t.AudioBytes, &t.ActualQuota, &t.Requests)
	if err != nil {
		return t, fmt.Errorf("store: usage totals by user id=%d: %w", userID, err)
	}
	return t, nil
}

// UsageTrendPoint 是趋势图的一个按天数据点。
type UsageTrendPoint struct {
	Date        string `json:"date"` // 'YYYY-MM-DD'(北京时间)
	ActualQuota int64  `json:"actual_quota"`
	Characters  int64  `json:"characters"`
	Requests    int64  `json:"requests"`
}

// UsageTrendByUser 返回最近 days 天(含今天)的按天聚合,**只统计 status=1**(决策 X5)。
//
// 两个关键口径:
//   - 按**北京时间**日界分组(X17),否则整体错位 8 小时;
//   - **补齐连续日期轴**:没有数据的日期补零行,前端柱状图需要连续轴(§8.2)。
func (s *Store) UsageTrendByUser(userID int64, days int) ([]UsageTrendPoint, error) {
	if days <= 0 || days > 90 {
		days = 14
	}
	loc := time.FixedZone("CST", 8*3600)
	today := time.Now().In(loc)

	start := today.AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	end := today.Format("2006-01-02")

	rows, err := s.db.Query(`
		SELECT date(created_at, ?) AS d,
		       COALESCE(SUM(actual_quota),0), COALESCE(SUM(characters),0), COUNT(*)
		FROM usage
		WHERE user_id = ? AND status = ?
		  AND date(created_at, ?) >= ? AND date(created_at, ?) <= ?
		GROUP BY d ORDER BY d ASC`,
		statsTZOffset, userID, UsageStatusSettled, statsTZOffset, start, statsTZOffset, end)
	if err != nil {
		return nil, fmt.Errorf("store: usage trend id=%d: %w", userID, err)
	}
	defer rows.Close()

	got := make(map[string]UsageTrendPoint, days)
	for rows.Next() {
		var p UsageTrendPoint
		if err := rows.Scan(&p.Date, &p.ActualQuota, &p.Characters, &p.Requests); err != nil {
			return nil, fmt.Errorf("store: usage trend scan: %w", err)
		}
		got[p.Date] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: usage trend rows: %w", err)
	}

	out := make([]UsageTrendPoint, 0, days)
	for i := 0; i < days; i++ {
		d := today.AddDate(0, 0, -(days - 1 - i)).Format("2006-01-02")
		if p, ok := got[d]; ok {
			out = append(out, p)
			continue
		}
		out = append(out, UsageTrendPoint{Date: d}) // 缺日补 0
	}
	return out, nil
}

// ---- STAGE4:预扣 / 结算 / 退款(额度变动 + 流水同一事务) ----------------
//
// 决策 C:扣费主源是 users.balance;tokens 只保留可选准入限额。三段式落点(§7.2):
//  ① 预扣 UserPreConsume —— 扣 users.balance + 预占 tokens.used_quota + INSERT usage(status=0)
//  ② 结算 UsageSettle     —— UPDATE usage→status=1 + balance 多退少补 + used_quota 回退
//  ③ 退款 UsageRefund     —— UPDATE usage→status=2 + balance 全额退回 + used_quota 回退
//
// ⚠️ 单连接自锁约束(store/db.go:294):本文件所有 Tx 方法内部**只能**用 tx,
// 绝不能调用走 s.db 的方法,否则在 SetMaxOpenConns(1) 下自锁。

// PreConsumeInput 是一次预扣的完整输入。
type PreConsumeInput struct {
	UserID     int64
	TokenID    int64
	Amount     int64  // 预扣额(billing.PreQuota 算出)
	RequestID  string // 本地请求号 —— 同时是 pending 行的定位键(如 local-xxx)
	Model      string
	Characters int
	// Unlimited=true 表示该 token 不计费:不扣余额、不预占限额、Pre 记 0,
	// 但**仍然**要做欠费探针并落 pending 流水(决策 X24 连带决定① + X26)。
	Unlimited bool
}

// UserPreConsume 预扣:单事务内完成「余额校验 + 限额预占 + 扣余额 + 落 pending 流水」
// (决策 D11 / D27 / X16 / X24 / X26)。
//
// 校验顺序(决策 D11 的"预扣前预检"):
//  1. 余额为负 → ErrBalanceNegative(**欠费停服**,X24)
//  2. 0 <= balance < Amount → ErrInsufficientBalance
//  3. limit_quota 非空且 used_quota + Amount 超限 → ErrTokenLimitExceeded(原子预占,D17)
//  4. 才真正扣减
//
// 重放保护:部分唯一索引 idx_usage_request_pending 保证同一 request_id 至多一条
// status=0 行,重放会命中 UNIQUE 冲突 → 返回 ErrDuplicate(调用方按 402 处理,不重复扣款)。
func (s *Store) UserPreConsume(in PreConsumeInput) error {
	if in.UserID == 0 || in.TokenID == 0 {
		return fmt.Errorf("%w: user_id and token_id are required", ErrInvalid)
	}
	if in.RequestID == "" {
		return fmt.Errorf("%w: request_id is required", ErrInvalid)
	}
	if in.Amount < 0 {
		return fmt.Errorf("%w: amount must be >= 0", ErrInvalid)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: pre-consume begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1) 余额探针:unlimited 也必须做 —— 负余额即停服
	var balance int64
	err = tx.QueryRow(`SELECT balance FROM users WHERE id = ? AND status <> ?`,
		in.UserID, UserDeleteStatus).Scan(&balance)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: pre-consume read balance: %w", err)
	}
	if balance < 0 {
		return ErrBalanceNegative
	}
	if in.Unlimited {
		// 不扣费,但仍落 pending 流水(计量保留,且让结算能 UPDATE 到它)
		if err := insertPendingUsage(tx, in); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: pre-consume commit: %w", err)
		}
		return nil
	}
	if balance < in.Amount {
		return ErrInsufficientBalance
	}

	// 2) 原子预占限额:limit_quota 为 NULL 时条件恒真(仅累加 used_quota,不拦截)
	res, err := tx.Exec(`
		UPDATE tokens SET used_quota = used_quota + ?, updated_at = datetime('now')
		WHERE id = ? AND (limit_quota IS NULL OR used_quota + ? <= limit_quota)`,
		in.Amount, in.TokenID, in.Amount)
	if err != nil {
		return fmt.Errorf("store: pre-consume reserve limit: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTokenLimitExceeded
	}
	// 3) 扣余额
	if _, err := tx.Exec(`UPDATE users SET balance = balance - ?, updated_at = datetime('now') WHERE id = ?`,
		in.Amount, in.UserID); err != nil {
		return fmt.Errorf("store: pre-consume deduct balance: %w", err)
	}

	// 4) 落 pending 流水(预扣是 usage 的唯一 INSERT 点)
	if err := insertPendingUsage(tx, in); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: pre-consume commit: %w", err)
	}
	return nil
}

// insertPendingUsage 插入 status=0 的预扣行。
func insertPendingUsage(tx *sql.Tx, in PreConsumeInput) error {
	_, err := tx.Exec(`
		INSERT INTO usage (user_id, token_id, channel_id, request_id, upstream_req_id, model, characters, audio_ms, audio_bytes, pre_consumed, actual_quota, status, created_at)
		VALUES (?, ?, NULL, ?, '', ?, ?, 0, 0, ?, 0, ?, datetime('now'))`,
		in.UserID, in.TokenID, in.RequestID, in.Model, in.Characters, in.Amount, UsageStatusPending)
	if err != nil {
		if isUniqueViolation(err) {
			// 同一 request_id 已有 pending 行 → 重放(X16)
			return ErrDuplicate
		}
		return fmt.Errorf("store: pre-consume insert usage: %w", err)
	}
	return nil
}

// SettleInput 是一次结算的完整输入。
type SettleInput struct {
	UserID  int64
	TokenID int64
	Pre     int64 // 已预扣额
	Actual  int64 // 实际应扣额
	// ReserveReqID 是**预扣时**使用的定位键(本地请求号),用于找到 pending 行。
	ReserveReqID string
	// Usage 携带其余流水字段;其中 RequestID 是最终对外的 request_id
	// (成功时优先用上游 ReqID,失败为空则回退本地号)——可能与 ReserveReqID 不同。
	Usage Usage
}

// UsageSettle 结算:多退少补 + 把 pending 行推进为 status=1,单事务。
//
// 动作(全在一个 tx 内):
//  1. users.balance -= (Actual - Pre)      // ⚠️ **允许扣成负**(X24:负余额由欠费停服处理)
//  2. tokens.used_quota += (Actual - Pre)  // 释放/补占限额;净效果 = 累计 actual
//  3. users.used_quota += Actual           // 聚合对账镜像(语义不变)
//  4. UPDATE usage SET status=1 ... WHERE request_id=ReserveReqID AND status=0
//
// 幂等与单向前进(X26):只有 status=0 的行能被推进;定位不到或状态不符时区分返回:
//   - 已有 status=1 行 → 幂等返回 nil(重复结算不再动额度)
//   - 已有 status=2 行 → ErrUsageStateConflict(已退款又结算,数据不一致)
//   - 完全没有该 request_id 的行 → ErrNotFound(预扣行丢失)
func (s *Store) UsageSettle(in SettleInput) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: usage settle begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 0) 定位 pending 行 / 幂等判定
	var st int
	err = tx.QueryRow(`SELECT status FROM usage WHERE request_id = ? ORDER BY id DESC LIMIT 1`,
		in.ReserveReqID).Scan(&st)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("store: settle locate pending: %w", err)
	case st == UsageStatusSettled:
		return nil // 幂等:已结算
	case st == UsageStatusRefunded:
		return ErrUsageStateConflict
	}

	// 1) 余额多退少补(允许为负)
	if _, err := tx.Exec(`UPDATE users SET balance = balance - ?, updated_at = datetime('now') WHERE id = ?`,
		in.Actual-in.Pre, in.UserID); err != nil {
		return fmt.Errorf("store: settle adjust balance: %w", err)
	}
	// 2) 限额预占回退(净效果 = 累计 actual)
	if _, err := tx.Exec(`UPDATE tokens SET used_quota = used_quota + ?, updated_at = datetime('now') WHERE id = ?`,
		in.Actual-in.Pre, in.TokenID); err != nil {
		return fmt.Errorf("store: settle adjust token used_quota: %w", err)
	}
	// 3) user 维度聚合对账镜像
	if _, err := tx.Exec(`UPDATE users SET used_quota = used_quota + ?, updated_at = datetime('now') WHERE id = ?`,
		in.Actual, in.UserID); err != nil {
		return fmt.Errorf("store: settle user used_quota: %w", err)
	}
	// 4) 流水推进 0 → 1(单向前进)
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
	res, err := tx.Exec(`
		UPDATE usage SET status = ?, pre_consumed = ?, actual_quota = ?,
		                 upstream_req_id = ?, channel_id = ?, model = ?, characters = ?,
		                 audio_ms = ?, audio_bytes = ?
		WHERE request_id = ? AND status = ?`,
		u.Status, u.PreConsumed, u.ActualQuota, u.RequestID, channelVal, u.Model,
		u.Characters, u.AudioMS, u.AudioBytes, in.ReserveReqID, UsageStatusPending)
	if err != nil {
		return fmt.Errorf("store: settle update usage: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUsageStateConflict
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: usage settle commit: %w", err)
	}
	return nil
}

// UsageRefund 退款:把预扣全额退回余额 + 把 pending 行推进为 status=2,单事务且**幂等**。
//
// 单向前进(X26):只有 status=0 的行能被退款。
//   - 已 status=2 → 幂等返回 nil(重复退款不再加额度)
//   - 已 status=1 → ErrUsageStateConflict(**拒绝**)
//
// 这里修掉了 3b 的一个真实缺陷:3b 的幂等只查 status=2,看不到 status=1,
// 因此"先结算、后跨进程重试退款"会把已结算的预扣**再退一次**(重复退钱)。
//
// 动作:
//  1. 定位行并判定状态(见上)
//  2. users.balance += Pre
//  3. tokens.used_quota -= Pre(释放预占)
//  4. UPDATE usage SET status=2, actual_quota=0 ... WHERE request_id=? AND status=0
func (s *Store) UsageRefund(userID, tokenID, pre int64, reserveReqID string, u Usage) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: usage refund begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var st int
	err = tx.QueryRow(`SELECT status FROM usage WHERE request_id = ? ORDER BY id DESC LIMIT 1`,
		reserveReqID).Scan(&st)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("store: refund locate pending: %w", err)
	case st == UsageStatusRefunded:
		return nil // 幂等:已退款
	case st == UsageStatusSettled:
		return ErrUsageStateConflict // 已结算不得再退预扣
	}

	if _, err := tx.Exec(`UPDATE users SET balance = balance + ?, updated_at = datetime('now') WHERE id = ?`,
		pre, userID); err != nil {
		return fmt.Errorf("store: refund balance: %w", err)
	}
	if _, err := tx.Exec(`UPDATE tokens SET used_quota = used_quota - ?, updated_at = datetime('now') WHERE id = ?`,
		pre, tokenID); err != nil {
		return fmt.Errorf("store: refund token used_quota: %w", err)
	}

	var channelVal any
	if u.ChannelID != nil {
		channelVal = *u.ChannelID
	}
	res, err := tx.Exec(`
		UPDATE usage SET status = ?, pre_consumed = ?, actual_quota = 0,
		                 channel_id = ?, model = ?, characters = ?, audio_ms = ?, audio_bytes = ?
		WHERE request_id = ? AND status = ?`,
		UsageStatusRefunded, pre, channelVal, u.Model, u.Characters, u.AudioMS, u.AudioBytes,
		reserveReqID, UsageStatusPending)
	if err != nil {
		return fmt.Errorf("store: refund update usage: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUsageStateConflict
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: usage refund commit: %w", err)
	}
	return nil
}
