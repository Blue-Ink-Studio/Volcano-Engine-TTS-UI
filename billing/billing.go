// Package billing 实现阶段 3b 的配额三段式计费:预扣 → 结算 → 退款。
//
// 【重要:倍率默认值与 STAGE3-QUOTA-BILLING.md v2 对齐】
// 默认语义:1 个字符 = 1 额度;1 秒音频 = 1 额度;无分组倍率(group=1.0);
// 预扣乘数 multiplier=1.2(预扣比实际多 20%,防跑超,结算多退少补)。
// 若需调整倍率,只需改 DefaultRatios()/常量一处,调用方零改动。
//
// 计费公式(§3,ceil 向上取整):
//
//	pre    = ceil(chars × CharRatio × Group × Multiplier)
//	actual = ceil(chars × CharRatio × Group + audio_ms/1000 × SecRatio × Group)
//
// 额度归属:**token 层**(tokens.remain_quota 是唯一校验源);users.quota 作废不校验,
// users.used_quota 只做聚合对账(累加 actual)。
package billing

import (
	"errors"
	"fmt"
	"math"

	"github.com/volcano-tts/tts-api/store"
)

// 倍率默认值。
// char/sec/group 与 STAGE3-QUOTA-BILLING.md v2 一致;multiplier 按 v2 取 1.2(预扣防跑超)。
const (
	DefaultCharRatio  = 1.0
	DefaultSecRatio   = 1.0
	DefaultGroupRatio = 1.0
	DefaultMultiplier = 1.2
)

// ErrQuotaExhausted 表示额度不足(或并发预扣失败)→ controller 翻译为 402 quota_exhausted。
var ErrQuotaExhausted = errors.New("billing: quota exhausted")

// Ratios 是一次计费用到的全部倍率。
type Ratios struct {
	Char       float64 // 每字符额度
	Sec        float64 // 每秒音频额度
	Group      float64 // 分组倍率(3b 恒 1.0;分组配置后续接入)
	Multiplier float64 // 预扣乘数(1.2:预扣比实际多 20%,防跑超;分组配置后续接入)
}

// DefaultRatios 返回当前默认倍率。
func DefaultRatios() Ratios {
	return Ratios{
		Char:       DefaultCharRatio,
		Sec:        DefaultSecRatio,
		Group:      DefaultGroupRatio,
		Multiplier: DefaultMultiplier,
	}
}

// normalized 把非法(<=0 或 NaN)倍率兜回 1.0,避免配置错误导致 0 计费或 NaN。
func (r Ratios) normalized() Ratios {
	fix := func(v float64) float64 {
		if math.IsNaN(v) || v < 0 {
			return 1.0
		}
		return v
	}
	r.Char, r.Sec, r.Group, r.Multiplier = fix(r.Char), fix(r.Sec), fix(r.Group), fix(r.Multiplier)
	return r
}

// PreQuota 计算预扣额度:ceil(chars × Char × Group × Multiplier)。
func PreQuota(chars int, r Ratios) int64 {
	r = r.normalized()
	if chars <= 0 {
		return 0
	}
	v := float64(chars) * r.Char * r.Group * r.Multiplier
	return int64(math.Ceil(v))
}

// ActualQuota 计算实际额度:ceil(chars × Char × Group + 秒数 × Sec × Group)。
// audioMS 为合成音频时长(毫秒)。
func ActualQuota(chars int, audioMS int64, r Ratios) int64 {
	r = r.normalized()
	charsPart := float64(chars) * r.Char * r.Group
	secPart := 0.0
	if audioMS > 0 {
		secPart = float64(audioMS) / 1000.0 * r.Sec * r.Group
	}
	v := charsPart + secPart
	if v <= 0 {
		return 0
	}
	return int64(math.Ceil(v))
}

// Session 是一次请求的计费上下文:由 PreConsume 创建,随后 Settle 或 Refund。
//
// 生命周期(controller/tts.go 严格按此):
//
//	sess, err := billing.PreConsume(...)   // 鉴权后、Synthesize 前;额度不足 → ErrQuotaExhausted
//	... Synthesize ...
//	sess.Settle(usage)   // 成功
//	sess.Refund(usage)   // 失败(幂等)
type Session struct {
	store     *store.Store
	TokenID   int64
	UserID    int64
	ratios    Ratios
	pre       int64
	unlimited bool
	closed    bool // 防重复 Settle/Refund(第二次直接 no-op)
}

// Pre 返回本会话的预扣额度(unlimited 恒 0)。
func (s *Session) Pre() int64 { return s.pre }

// Unlimited 报告本会话是否为不限额 token。
func (s *Session) Unlimited() bool { return s.unlimited }

// PreConsume 计算并原子预扣额度。
//
// userID 用于结算时累加 users.used_quota(决策 #3 聚合对账);tokenID 是额度唯一归属。
//
//   - unlimited token:**短路**,不发任何扣减 SQL,Pre 记 0
//     (这样 usage 的 pre_consumed/actual_quota 都是 0,SUM(actual_quota) 才是
//     真实"额度消耗",不会被不限额流量虚增 —— 决策 #3 对账口径)
//   - 普通 token:Pre = PreQuota(chars);原子扣减失败(额度不足或并发抢跑)
//     → ErrQuotaExhausted,调用方必须 402 且**不得**继续合成
//
// pre == 0(如 chars=0)时仍走一次存在性确认,避免给不存在的 token 放行。
func PreConsume(s *store.Store, userID, tokenID int64, unlimited bool, chars int, r Ratios) (*Session, error) {
	if s == nil {
		return nil, errors.New("billing: nil store")
	}
	if userID == 0 || tokenID == 0 {
		return nil, errors.New("billing: userID and tokenID are required")
	}
	sess := &Session{
		store:     s,
		TokenID:   tokenID,
		UserID:    userID,
		ratios:    r.normalized(),
		unlimited: unlimited,
	}
	if unlimited {
		return sess, nil
	}
	pre := PreQuota(chars, sess.ratios)
	sess.pre = pre
	ok, err := s.TokenDecreaseQuota(tokenID, pre)
	if err != nil {
		return nil, fmt.Errorf("billing: pre-consume token=%d: %w", tokenID, err)
	}
	if !ok {
		return nil, ErrQuotaExhausted
	}
	return sess, nil
}

// Settle 结算(合成成功后调用):按实际用量多退少补,并记 status=1 的流水。
// 返回实际扣减额度(unlimited 恒 0)。
//
// u 需带 RequestID/ChannelID/Model/Characters/AudioMS/AudioBytes;
// UserID/TokenID/PreConsumed/ActualQuota/Status 由本函数填充。
func (s *Session) Settle(u store.Usage) (int64, error) {
	if s == nil || s.closed {
		return 0, nil
	}
	if u.RequestID == "" {
		return 0, fmt.Errorf("%w: request_id is required", store.ErrInvalid)
	}
	var actual int64
	if !s.unlimited {
		actual = ActualQuota(u.Characters, u.AudioMS, s.ratios)
	}
	if err := s.store.UsageSettle(store.SettleInput{
		UserID:  s.UserID,
		TokenID: s.TokenID,
		Pre:     s.pre,
		Actual:  actual,
		Usage:   u,
	}); err != nil {
		return 0, err
	}
	s.closed = true
	return actual, nil
}

// Refund 退款(合成失败后调用):全额退回预扣,并记 status=2 的流水。
// 幂等:store.UsageRefund 在事务内按 request_id+status=2 判重,重复调用不再退额度。
func (s *Session) Refund(u store.Usage) error {
	if s == nil || s.closed {
		return nil
	}
	if u.RequestID == "" {
		return fmt.Errorf("%w: request_id is required", store.ErrInvalid)
	}
	if err := s.store.UsageRefund(s.UserID, s.TokenID, s.pre, u); err != nil {
		return err
	}
	s.closed = true
	return nil
}
