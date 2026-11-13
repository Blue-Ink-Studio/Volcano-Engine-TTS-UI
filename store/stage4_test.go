package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// openTestStore 开一个临时库(schema 已迁移到最新版)。
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newUserWithBalance 建用户并把余额设成指定值(可传负数,模拟欠费)。
func newUserWithBalance(t *testing.T, s *Store, name string, balance int64) int64 {
	t.Helper()
	id, err := s.UserInsert(User{Name: name, Status: 1})
	if err != nil {
		t.Fatalf("insert user %s: %v", name, err)
	}
	if _, err := s.DB().Exec(`UPDATE users SET balance = ? WHERE id = ?`, balance, id); err != nil {
		t.Fatalf("set balance: %v", err)
	}
	return id
}

func newTokenFor(t *testing.T, s *Store, userID int64, unlimited bool, limitQuota *int64) int64 {
	t.Helper()
	id, _, err := s.TokenCreateWithKey(userID, "t", unlimited, limitQuota)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	return id
}

func mustBalance(t *testing.T, s *Store, userID int64) int64 {
	t.Helper()
	u, err := s.UserGet(userID)
	if err != nil {
		t.Fatalf("user get: %v", err)
	}
	return u.Balance
}

func mustToken(t *testing.T, s *Store, tokenID int64) Token {
	t.Helper()
	tk, err := s.TokenGet(tokenID)
	if err != nil {
		t.Fatalf("token get: %v", err)
	}
	return *tk
}

// ---- 预扣的四种拒绝语义 ----

func TestPreConsume_RejectsWhenBalanceNegative(t *testing.T) {
	s := openTestStore(t)
	uid := newUserWithBalance(t, s, "debtor", -5)
	tid := newTokenFor(t, s, uid, false, nil)

	err := s.UserPreConsume(PreConsumeInput{
		UserID: uid, TokenID: tid, Amount: 0, RequestID: "r1", Characters: 0,
	})
	if !errors.Is(err, ErrBalanceNegative) {
		t.Fatalf("期望 ErrBalanceNegative,得到 %v", err)
	}
	// 关键:即便 Amount=0,负余额也必须被挡住(SQL 里 -5 >= 0 为假)
	if got := mustBalance(t, s, uid); got != -5 {
		t.Fatalf("余额不应变动,得到 %d", got)
	}
}

func TestPreConsume_UnlimitedStillBlockedWhenDebtor(t *testing.T) {
	s := openTestStore(t)
	uid := newUserWithBalance(t, s, "debtor2", -1)
	tid := newTokenFor(t, s, uid, true, nil) // unlimited

	err := s.UserPreConsume(PreConsumeInput{
		UserID: uid, TokenID: tid, Amount: 0, RequestID: "r2", Unlimited: true,
	})
	if !errors.Is(err, ErrBalanceNegative) {
		t.Fatalf("unlimited token 也应受欠费停服约束(X24),得到 %v", err)
	}
}

func TestPreConsume_RejectsInsufficientBalance(t *testing.T) {
	s := openTestStore(t)
	uid := newUserWithBalance(t, s, "poor", 10)
	tid := newTokenFor(t, s, uid, false, nil)

	err := s.UserPreConsume(PreConsumeInput{
		UserID: uid, TokenID: tid, Amount: 20, RequestID: "r3", Characters: 16,
	})
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("期望 ErrInsufficientBalance,得到 %v", err)
	}
	if got := mustBalance(t, s, uid); got != 10 {
		t.Fatalf("失败必须整体回滚,余额应为 10,得到 %d", got)
	}
	// 失败时不应留下 pending 行
	n, err := s.PendingUsageCount()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("失败不应留下 pending 行,得到 %d", n)
	}
}

func TestPreConsume_RejectsOverTokenLimit(t *testing.T) {
	s := openTestStore(t)
	uid := newUserWithBalance(t, s, "limited", 1000)
	lim := int64(10)
	tid := newTokenFor(t, s, uid, false, &lim)

	err := s.UserPreConsume(PreConsumeInput{
		UserID: uid, TokenID: tid, Amount: 11, RequestID: "r4", Characters: 9,
	})
	if !errors.Is(err, ErrTokenLimitExceeded) {
		t.Fatalf("期望 ErrTokenLimitExceeded,得到 %v", err)
	}
	// D27:限额拒绝时余额也不得变动(同事务)
	if got := mustBalance(t, s, uid); got != 1000 {
		t.Fatalf("限额拒绝时余额不应变动,得到 %d", got)
	}
	if got := mustToken(t, s, tid).UsedQuota; got != 0 {
		t.Fatalf("限额拒绝时 used_quota 不应变动,得到 %d", got)
	}
}

func TestPreConsume_ReplayBlockedByPartialIndex(t *testing.T) {
	s := openTestStore(t)
	uid := newUserWithBalance(t, s, "replayer", 1000)
	tid := newTokenFor(t, s, uid, false, nil)

	in := PreConsumeInput{UserID: uid, TokenID: tid, Amount: 12, RequestID: "same-rid", Characters: 10}
	if err := s.UserPreConsume(in); err != nil {
		t.Fatalf("首次预扣应成功: %v", err)
	}
	err := s.UserPreConsume(in)
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("同 request_id 重放应被 idx_usage_request_pending 挡住(X16),得到 %v", err)
	}
	if got := mustBalance(t, s, uid); got != 988 {
		t.Fatalf("重放不得二次扣款,余额应为 988,得到 %d", got)
	}
}

// ---- 三段式:预扣 → 结算 / 退款 ----

func TestPreConsumeSettle_MovesMoneyAndUsageState(t *testing.T) {
	s := openTestStore(t)
	uid := newUserWithBalance(t, s, "ok", 100)
	tid := newTokenFor(t, s, uid, false, nil)

	if err := s.UserPreConsume(PreConsumeInput{
		UserID: uid, TokenID: tid, Amount: 12, RequestID: "local-1", Characters: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if got := mustBalance(t, s, uid); got != 88 {
		t.Fatalf("预扣后余额应为 88,得到 %d", got)
	}

	// 结算:actual(10) < pre(12) → 退回 2;request_id 换成上游 id
	if err := s.UsageSettle(SettleInput{
		UserID: uid, TokenID: tid, Pre: 12, Actual: 10, ReserveReqID: "local-1",
		Usage: Usage{RequestID: "upstream-abc", Model: "m", Characters: 10, AudioMS: 0},
	}); err != nil {
		t.Fatal(err)
	}
	if got := mustBalance(t, s, uid); got != 90 {
		t.Fatalf("结算后余额应为 90(多退少补),得到 %d", got)
	}
	// token.used_quota 净效果 = actual
	if got := mustToken(t, s, tid).UsedQuota; got != 10 {
		t.Fatalf("used_quota 净效果应为 actual=10,得到 %d", got)
	}
	// users.used_quota 镜像
	if u, _ := s.UserGet(uid); u.UsedQuota != 10 {
		t.Fatalf("users.used_quota 应为 10,得到 %d", u.UsedQuota)
	}
	// remain_quota 必须冻结不动
	if got := mustToken(t, s, tid).RemainQuota; got != 0 {
		t.Fatalf("remain_quota 必须冻结为 0,得到 %d", got)
	}
	// 流水:同一 request_id 只有一行,status=1,且 request_id 已更新为上游 id
	items, total, err := s.UsageListFiltered(UsageFilter{UserID: uid})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("同一请求应只有 1 行流水,得到 %d", total)
	}
	if items[0].Status != UsageStatusSettled || items[0].ActualQuota != 10 || items[0].PreConsumed != 12 {
		t.Fatalf("流水字段不对: %+v", items[0])
	}
	// request_id 必须**保持稳定**(它是三段式的定位键);上游 id 另存一列
	if items[0].RequestID != "local-1" {
		t.Fatalf("request_id 必须保持为本地定位键 local-1,得到 %q", items[0].RequestID)
	}
	if items[0].UpstreamReqID != "upstream-abc" {
		t.Fatalf("upstream_req_id 应为上游 id,得到 %q", items[0].UpstreamReqID)
	}
}

func TestSettle_AllowsBalanceToGoNegative(t *testing.T) {
	s := openTestStore(t)
	uid := newUserWithBalance(t, s, "tight", 12) // 刚好够预扣
	tid := newTokenFor(t, s, uid, false, nil)

	if err := s.UserPreConsume(PreConsumeInput{
		UserID: uid, TokenID: tid, Amount: 12, RequestID: "local-2", Characters: 10,
	}); err != nil {
		t.Fatal(err)
	}
	// actual(20) > pre(12) → 补扣 8,而余额只剩 0 → 允许为负(X24)
	if err := s.UsageSettle(SettleInput{
		UserID: uid, TokenID: tid, Pre: 12, Actual: 20, ReserveReqID: "local-2",
		Usage: Usage{RequestID: "up", Characters: 10, AudioMS: 8000},
	}); err != nil {
		t.Fatalf("结算补扣不应被拒绝(X24): %v", err)
	}
	if got := mustBalance(t, s, uid); got != -8 {
		t.Fatalf("余额应被补扣成 -8,得到 %d", got)
	}
}

func TestRefund_IdempotentAndBlocksAfterSettle(t *testing.T) {
	s := openTestStore(t)
	uid := newUserWithBalance(t, s, "refunder", 100)
	tid := newTokenFor(t, s, uid, false, nil)

	if err := s.UserPreConsume(PreConsumeInput{
		UserID: uid, TokenID: tid, Amount: 12, RequestID: "local-3", Characters: 10,
	}); err != nil {
		t.Fatal(err)
	}
	u := Usage{RequestID: "local-3", Characters: 10}

	if err := s.UsageRefund(uid, tid, 12, "local-3", u); err != nil {
		t.Fatal(err)
	}
	if got := mustBalance(t, s, uid); got != 100 {
		t.Fatalf("退款后余额应回到 100,得到 %d", got)
	}
	if got := mustToken(t, s, tid).UsedQuota; got != 0 {
		t.Fatalf("退款后 used_quota 应回到 0,得到 %d", got)
	}
	// 幂等:再退一次不得重复加钱
	if err := s.UsageRefund(uid, tid, 12, "local-3", u); err != nil {
		t.Fatalf("重复退款应幂等返回 nil,得到 %v", err)
	}
	if got := mustBalance(t, s, uid); got != 100 {
		t.Fatalf("重复退款不得加钱,得到 %d", got)
	}
	// 单向前进:已退款的行不得再被结算
	err := s.UsageSettle(SettleInput{
		UserID: uid, TokenID: tid, Pre: 12, Actual: 10, ReserveReqID: "local-3",
		Usage: Usage{RequestID: "up", Characters: 10},
	})
	if !errors.Is(err, ErrUsageStateConflict) {
		t.Fatalf("已退款的行再结算应报冲突,得到 %v", err)
	}
}

// 这条覆盖 3b 的真实缺陷:其退款幂等只看 status=2,看不到 status=1,
// 于是"先结算、后重试退款"会把已结算的预扣再退一次(重复退钱)。
func TestRefund_AfterSettleMustBeRejected(t *testing.T) {
	s := openTestStore(t)
	uid := newUserWithBalance(t, s, "double", 100)
	tid := newTokenFor(t, s, uid, false, nil)

	if err := s.UserPreConsume(PreConsumeInput{
		UserID: uid, TokenID: tid, Amount: 12, RequestID: "local-4", Characters: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UsageSettle(SettleInput{
		UserID: uid, TokenID: tid, Pre: 12, Actual: 10, ReserveReqID: "local-4",
		Usage: Usage{RequestID: "up4", Characters: 10},
	}); err != nil {
		t.Fatal(err)
	}
	balAfterSettle := mustBalance(t, s, uid) // 90

	err := s.UsageRefund(uid, tid, 12, "local-4", Usage{RequestID: "local-4"})
	if !errors.Is(err, ErrUsageStateConflict) {
		t.Fatalf("已结算的行退款必须被拒绝,得到 %v", err)
	}
	if got := mustBalance(t, s, uid); got != balAfterSettle {
		t.Fatalf("被拒绝的退款不得加钱:期望 %d,得到 %d", balAfterSettle, got)
	}
}

// ---- 钱包:非负约束 + 流水 ----

func TestUserAddBalance_GuardAndChargeLog(t *testing.T) {
	s := openTestStore(t)
	uid := newUserWithBalance(t, s, "wallet", 10)

	if _, err := s.UserAddBalance(uid, 90, ChargeTypeRedeem, ChargeSourceRedeem, "充值码兑换"); err != nil {
		t.Fatal(err)
	}
	if got := mustBalance(t, s, uid); got != 100 {
		t.Fatalf("余额应为 100,得到 %d", got)
	}
	// 手动扣成负必须被拒(D7:只约束手动调整路径)
	if _, err := s.UserAddBalance(uid, -101, ChargeTypeManualAdjust, ChargeSourceManual, "扣回"); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("手动扣成负应报 ErrInsufficientBalance,得到 %v", err)
	}
	if _, err := s.UserAddBalance(uid, 0, ChargeTypeCharge, ChargeSourceManual, "零"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("delta=0 应报 ErrInvalid,得到 %v", err)
	}

	logs, total, err := s.ChargeLogListByUser(uid, "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(logs) != 1 {
		t.Fatalf("应恰好 1 条资金流水,得到 %d", total)
	}
	if logs[0].Type != ChargeTypeRedeem || logs[0].Delta != 90 || logs[0].BalanceAfter != 100 {
		t.Fatalf("流水字段不对: %+v", logs[0])
	}
	// 按 type 过滤
	_, total2, err := s.ChargeLogListByUser(uid, ChargeTypeCharge, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total2 != 0 {
		t.Fatalf("type=charge 应为 0 条,得到 %d", total2)
	}
}

// ---- 充值码 ----

func TestRedemption_AtomicAndSingleUse(t *testing.T) {
	s := openTestStore(t)
	uid := newUserWithBalance(t, s, "redeemer", 0)

	codes, err := s.RedemptionCreateBatch(100, 2, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 2 {
		t.Fatalf("应生成 2 个码,得到 %d", len(codes))
	}
	for _, c := range codes {
		if len(c) != RedeemCodeLen {
			t.Fatalf("码长度应为 %d,得到 %d (%q)", RedeemCodeLen, len(c), c)
		}
	}

	quota, bal, err := s.RedemptionRedeemAtomic(codes[0], uid)
	if err != nil {
		t.Fatal(err)
	}
	if quota != 100 || bal != 100 {
		t.Fatalf("兑换应得 100,余额 100;得到 quota=%d bal=%d", quota, bal)
	}
	// 二次兑换 → 已使用
	if _, _, err := s.RedemptionRedeemAtomic(codes[0], uid); !errors.Is(err, ErrRedeemCodeUsed) {
		t.Fatalf("同码二次兑换应报 ErrRedeemCodeUsed,得到 %v", err)
	}
	if got := mustBalance(t, s, uid); got != 100 {
		t.Fatalf("二次兑换不得加钱,得到 %d", got)
	}
	// 大小写 + 空白归一化(决策 #21):第二个码用小写 + 前后空白也应兑换成功
	quota2, bal2, err := s.RedemptionRedeemAtomic("  "+strings.ToLower(codes[1])+"  ", uid)
	if err != nil {
		t.Fatalf("小写/带空白的码应能兑换: %v", err)
	}
	if quota2 != 100 || bal2 != 200 {
		t.Fatalf("第二个码应得 100、余额 200;得到 quota=%d bal=%d", quota2, bal2)
	}
	// 不存在的码
	if _, _, err := s.RedemptionRedeemAtomic("ZZZZZZZZZZZZZZZZ", uid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的码应报 ErrNotFound,得到 %v", err)
	}
	// 长度不符也按不存在处理
	if _, _, err := s.RedemptionRedeemAtomic("SHORT", uid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("长度不符应按不存在处理,得到 %v", err)
	}
}

// ---- 初始安全策略(D24/D31c + BUG-2 回归) ----

func TestEnforceInitialSecurityPolicy_FreshInstall(t *testing.T) {
	s := openTestStore(t)
	// 模拟 seed 出来的 default:无密码(seed 的 INSERT 不带 is_admin/password_hash)
	if _, err := s.UserInsert(User{Name: "default", Status: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnforceInitialSecurityPolicy(); err != nil {
		t.Fatal(err)
	}
	u, err := s.UserGetByName("default")
	if err != nil {
		t.Fatal(err)
	}
	// BUG-2 回归:migrate() 跑在 seed 之前,必须靠这里把 default 提升为管理员
	if !u.IsAdmin {
		t.Fatal("default 必须被提升为管理员(否则全新安装没有管理角色)")
	}
	v, ok, err := s.SettingsGet("allow_register")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || v != "0" {
		t.Fatalf("无密码的 default 应强制 allow_register=0,得到 ok=%v v=%q", ok, v)
	}
	// 幂等:再跑一次不报错
	if err := s.EnforceInitialSecurityPolicy(); err != nil {
		t.Fatal(err)
	}
}

func TestEnforceInitialSecurityPolicy_DoesNotOverrideAdminChoice(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.UserInsert(User{Name: "default", Status: 1, PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	// 管理员显式开过注册
	if err := s.SettingsSet("allow_register", "1"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnforceInitialSecurityPolicy(); err != nil {
		t.Fatal(err)
	}
	v, _, _ := s.SettingsGet("allow_register")
	if v != "1" {
		t.Fatalf("D31(c):不得覆盖管理员的显式设置,得到 %q", v)
	}
}
