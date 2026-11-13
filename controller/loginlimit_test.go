package controller

import (
	"testing"
	"time"
)

// 本文件覆盖 STAGE6 安全加固的登录限流部分(审计 P1-4)。
//
// 背景:原实现只有**账号维度**一把锁,且 map 只增不减。
// 两个可被未认证攻击者直接利用的问题:
//  1. 账号 DoS:5 次错误密码锁住任意账号 15 分钟;
//  2. 内存 OOM:海量随机用户名把 map 撑爆 → 进程被 OOM killer 干掉。
//
// 现在是三层:账号维度锁 + IP 维度锁 + 容量/过期上限。

// resetLoginAttempts 清空两张表(测试间隔离)。
func resetLoginAttempts(t *testing.T) {
	t.Helper()
	loginMu.Lock()
	defer loginMu.Unlock()
	loginAttempts = map[string]*loginAttempt{}
	loginIPAttempts = map[string]*loginAttempt{}
}

// TestLoginLockByAccount 锁住 P1-4 的"账号维度"语义:
// 同一账号连续失败 loginMaxFail 次 → 该账号被锁。
//
// 同时验证**不同 IP 不影响账号锁**:这是刻意的 ——
// 审计建议的"改成 (用户名,IP) 组合键"会让攻击者换 IP 就绕过锁定,
// 组合键单独用是不够的,所以我们保留纯账号键。
func TestLoginLockByAccount(t *testing.T) {
	resetLoginAttempts(t)

	for i := 0; i < loginMaxFail; i++ {
		loginRecordFailure("victim", "1.2.3.4")
	}
	locked, remain := loginLocked("victim", "9.9.9.9")
	if !locked {
		t.Fatalf("账号 %q 连续失败 %d 次后应被锁", "victim", loginMaxFail)
	}
	if remain <= 0 || remain > loginLockWindow {
		t.Fatalf("锁定剩余时长 %v 不在 (0, %v] 区间", remain, loginLockWindow)
	}

	// 其它账号不应受影响
	if locked, _ := loginLocked("other", "9.9.9.9"); locked {
		t.Fatal("其它账号不应被锁")
	}
}

// TestLoginLockByIP 锁住 P1-4 新增的"IP 维度"语义:
// 同一 IP 连续打**不同账号**失败 loginMaxIPFail 次 → 该 IP 被锁。
//
// 这是防"批量账号枚举/分布式爆破"的主力:账号锁拦不住"一个 IP 打一千个账号"。
func TestLoginLockByIP(t *testing.T) {
	resetLoginAttempts(t)

	const ip = "5.6.7.8"
	for i := 0; i < loginMaxIPFail; i++ {
		loginRecordFailure("someuser", ip)
	}
	locked, remain := loginLocked("never-seen-user", ip)
	if !locked {
		t.Fatalf("IP %s 累计失败 %d 次后应被锁", ip, loginMaxIPFail)
	}
	if remain <= 0 || remain > loginLockWindow {
		t.Fatalf("锁定剩余时长 %v 不在 (0, %v] 区间", remain, loginLockWindow)
	}
}

// TestLoginIPLockNotBypassedByAccountRotation 验证关键性质:
// 攻击者**轮换账号名**打同一个 IP,仍会被 IP 维度锁住。
//
// 若只实现账号维度锁,这个攻击可以无限枚举账号(每次换名,计数永不累积)。
func TestLoginIPLockNotBypassedByAccountRotation(t *testing.T) {
	resetLoginAttempts(t)

	const ip = "10.20.30.40"
	for i := 0; i < loginMaxIPFail; i++ {
		loginRecordFailure("user-"+string(rune('a'+i%26))+string(rune('0'+i/26)), ip)
	}
	if locked, _ := loginLocked("brand-new-name", ip); !locked {
		t.Fatal("轮换账号名不应绕过 IP 维度锁")
	}
}

// TestLoginLockEntriesBounded 锁住"内存 OOM"防护:
// 即便有海量随机用户名,两张表的条目数也**不得超过上限**。
func TestLoginLockEntriesBounded(t *testing.T) {
	resetLoginAttempts(t)

	// 远超上限的用户名
	for i := 0; i < loginMaxEntries+500; i++ {
		loginRecordFailure("flood-"+time.Duration(i).String(), "192.0.2.1")
	}

	loginMu.Lock()
	defer loginMu.Unlock()
	if len(loginAttempts) > loginMaxEntries {
		t.Fatalf("账号表条目数 %d 超过上限 %d —— 内存 OOM 风险未消除",
			len(loginAttempts), loginMaxEntries)
	}
	if len(loginIPAttempts) > loginMaxEntries {
		t.Fatalf("IP 表条目数 %d 超过上限 %d —— 内存 OOM 风险未消除",
			len(loginIPAttempts), loginMaxEntries)
	}
}

// TestLoginCleanupRemovesExpired 验证过期清理确实会回收条目。
func TestLoginCleanupRemained(t *testing.T) {
	resetLoginAttempts(t)

	loginRecordFailure("expired-user", "203.0.113.5")
	loginMu.Lock()
	before := len(loginAttempts)
	// 把 lastSeen 拨到很久以前,模拟"早已过期且很久没被访问"
	for _, a := range loginAttempts {
		a.lastSeen = time.Now().Add(-2 * loginLockWindow)
	}
	loginCleanupLocked(time.Now())
	after := len(loginAttempts)
	loginMu.Unlock()

	if after >= before {
		t.Fatalf("过期条目未被清理:before=%d after=%d", before, after)
	}
}

// TestLoginResetClearsBothDimensions 验证登录成功会把两个维度都清零。
func TestLoginResetClearsBothDimensions(t *testing.T) {
	resetLoginAttempts(t)

	loginRecordFailure("good-user", "198.51.100.7")
	loginReset("good-user", "198.51.100.7")

	loginMu.Lock()
	defer loginMu.Unlock()
	if _, ok := loginAttempts[loginKey("good-user")]; ok {
		t.Fatal("登录成功后账号维度计数应被清零")
	}
	if _, ok := loginIPAttempts["198.51.100.7"]; ok {
		t.Fatal("登录成功后 IP 维度计数应被清零")
	}
}
