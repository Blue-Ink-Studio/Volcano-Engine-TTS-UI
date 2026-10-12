// Package billing 实现配额三段式计费:预扣 → 结算 → 退款。
//
// 【STAGE4 归属变更 —— 决策 C(new-api 式双扣)】
// 额度归属已从 **token 层**迁到 **user 钱包**:
//   - 扣费主源是 users.balance(充值/赠送/消费都在这一个钱包里)
//   - tokens 退化为「认证凭证 + 可选准入限额(limit_quota)」,不再扣 remain_quota
//   - tokens.used_quota 语义变为**限额预占计数器**(预扣 +Pre、结算 +(Actual-Pre)、退款 -Pre)
//   - 余额**允许被结算补扣成负**;负余额由"欠费停服"处理(决策 X24)
//
// 【倍率默认值与 STAGE3-QUOTA-BILLING.md v2 对齐】
// 默认语义:1 个字符 = 1 额度;1 秒音频 = 1 额度;无分组倍率(group=1.0);
// 预扣乘数 multiplier=1.2(预扣比实际多 20%,防跑超,结算多退少补)。
// 若需调整倍率,只需改 DefaultRatios()/常量一处,调用方零改动。
//
// 计费公式(§3,ceil 向上取整):
//
//	pre    = ceil(chars × CharRatio × Group × Multiplier)
//	actual = ceil(chars × CharRatio × Group + audio_ms/1000 × SecRatio × Group)
//
// ⚠️ multiplier=1.2 **并不保证** pre >= actual:actual 含"音频秒数"项,
// 中文语速约 4–5 字/秒时 actual 常略大于 pre ⇒ **补扣是常态**(分界点约 5 字/秒)。
// 这正是"允许余额为负 + 欠费停服"(X24)与"limit_quota 可小幅穿透"(X25)的由来。
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

// 计费失败原因 —— controller 据此映射不同的 402 code(见文档 §17 错误码总表)。
var (
	// ErrQuotaExhausted:余额不足本笔预扣 → 402 insufficient_balance
	ErrQuotaExhausted = errors.New("billing: quota exhausted")
	// ErrBalanceNegative:余额为负 = 已欠费停服 → 402 balance_negative(决策 X24)
	ErrBalanceNegative = errors.New("billing: balance is negative")
	// ErrTokenLimitExceeded:该 token 的 limit_quota 已用尽 → 402 token_limit_exceeded
	ErrTokenLimitExceeded = errors.New("billing: token limit exceeded")
	// ErrDuplicateRequest:同一 request_id 已有预扣在途 → 402(防重放,决策 X16)
	ErrDuplicateRequest = errors.New("billing: duplicate in-flight request")
)

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
//	sess, err := billing.PreConsume(...)   // 鉴权后、Synthesize 前;额度不足 → 402
//	... Synthesize ...
//	sess.Settle(usage)   // 成功
//	sess.Refund(usage)   // 失败(幂等)
//
// reqID 是**预扣时**使用的本地请求号,同时是 usage 里 pending 行的定位键(决策 X26);
// 结算时落库的 request_id 可能换成上游 ReqID,但定位始终用 reqID。
type Session struct {
	store     *store.Store
	TokenID   int64
	UserID    int64
	reqID     string
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
// 【STAGE4 落点变更】不再调 store.TokenDecreaseQuota,改为 store.UserPreConsume,
// 一个事务内完成「余额校验 + 限额预占 + 扣余额 + 落 status=0 流水」(决策 D27 / X26)。
//
// 语义:
//   - unlimited token(决策 X24 连带决定①):**仍受欠费停服约束** ——
//     store 层会做"余额 >= 0"探针,负余额同样拒绝;但不扣余额、不预占限额,Pre 记 0。
//   - 普通 token:Pre = PreQuota(chars);不足 / 超限 / 重放 → 各自的 402 语义错误。
//   - pre == 0(如 chars=0)仍走完整路径,避免给不存在的 token/user 放行。
func PreConsume(s *store.Store, userID, tokenID int64, unlimited bool, chars int, reqID string, r Ratios) (*Session, error) {
	if s == nil {
		return nil, errors.New("billing: nil store")
	}
	if userID == 0 || tokenID == 0 {
		return nil, errors.New("billing: userID and tokenID are required")
	}
	if reqID == "" {
		return nil, errors.New("billing: requestID is required")
	}
	sess := &Session{
		store:     s,
		TokenID:   tokenID,
		UserID:    userID,
		reqID:     reqID,
		ratios:    r.normalized(),
		unlimited: unlimited,
	}
	pre := int64(0)
	if !unlimited {
		pre = PreQuota(chars, sess.ratios)
	}
	sess.pre = pre

	err := s.UserPreConsume(store.PreConsumeInput{
		UserID:     userID,
		TokenID:    tokenID,
		Amount:     pre,
		RequestID:  reqID,
		Characters: chars,
		Unlimited:  unlimited,
	})
	if err != nil {
		return nil, mapPreConsumeError(err)
	}
	return sess, nil
}

// mapPreConsumeError 把 store 层错误翻译成计费语义错误(供 controller 映射 402 code)。
func mapPreConsumeError(err error) error {
	switch {
	case errors.Is(err, store.ErrBalanceNegative):
		return fmt.Errorf("%w: %v", ErrBalanceNegative, err)
	case errors.Is(err, store.ErrInsufficientBalance):
		return fmt.Errorf("%w: %v", ErrQuotaExhausted, err)
	case errors.Is(err, store.ErrTokenLimitExceeded):
		return fmt.Errorf("%w: %v", ErrTokenLimitExceeded, err)
	case errors.Is(err, store.ErrDuplicate):
		return fmt.Errorf("%w: %v", ErrDuplicateRequest, err)
	default:
		return fmt.Errorf("billing: pre-consume: %w", err)
	}
}

// Settle 结算(合成成功后调用):按实际用量多退少补,并把 pending 流水推进为 status=1。
// 返回实际扣减额度(unlimited 恒 0)。
//
// u 需带 RequestID/ChannelID/Model/Characters/AudioMS/AudioBytes;
// UserID/TokenID/PreConsumed/ActualQuota/Status 由本函数/store 填充。
// 定位 pending 行用**预扣时的 reqID**;u.RequestID 作为最终对外的 request_id 落库
// (成功时通常为上游 ReqID,便于对账追溯)。
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
		UserID:       s.UserID,
		TokenID:      s.TokenID,
		Pre:          s.pre,
		Actual:       actual,
		ReserveReqID: s.reqID,
		Usage:        u,
	}); err != nil {
		return 0, err
	}
	s.closed = true
	return actual, nil
}

// Refund 退款(合成失败后调用):全额退回预扣,并把 pending 流水推进为 status=2。
//
// 幂等且**单向前进**(决策 X26):store.UsageRefund 只推进 status=0 的行;
// 重复调用(已 status=2)返回 nil 不再退额度;若已 status=1(已结算)则拒绝,
// 避免"先结算、后重试退款"造成重复退钱(3b 的真实缺陷)。
func (s *Session) Refund(u store.Usage) error {
	if s == nil || s.closed {
		return nil
	}
	if u.RequestID == "" {
		return fmt.Errorf("%w: request_id is required", store.ErrInvalid)
	}
	if err := s.store.UsageRefund(s.UserID, s.TokenID, s.pre, s.reqID, u); err != nil {
		return err
	}
	s.closed = true
	return nil
}
