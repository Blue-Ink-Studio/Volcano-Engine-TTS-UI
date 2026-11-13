package store

import (
	"errors"
	"path/filepath"
	"testing"
)

// 本文件覆盖 STAGE5 的用户体系埋点(契约 docs/STAGE5_SETUP_ROLES_PLAN.md)。
//
// 注意:*_test.go 被 .gitignore 忽略(项目约定,测试不提交),
// 所以这些断言的价值在于**本地验收**,而不是 CI 门禁。

// ---------------------------------------------------------------------------
// §4.2 的核心回归:is_admin → role 的回填必须被 schema_version 门控
// ---------------------------------------------------------------------------

// TestStage5BackfillIsVersionGated 是本阶段最重要的一条测试。
//
// 它复现并锁死一个会**静默提权**的缺陷:
//
//	is_admin 列在 STAGE5 之后被冻结(不再维护)。若 "is_admin=1 → role=admin"
//	这条回填在每次启动都跑,那么一个"曾被提升、后来被管理员降级为 user"的账号
//	会因为 is_admin 仍是 1 而**每次重启都被重新提权**。
//
// 测试做法:把库退回 "v5 + 只有 is_admin 没有 role" 的老库形态 → 重开触发回填
// → 再把 owner 降级 → 再次重开 → 断言**没有被重新提权**。
func TestStage5BackfillIsVersionGated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stage5.db")

	// 1) 先正常开一次(此刻 schema 已到 6)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// 2) 把它改造成"STAGE4 老库":删掉 version>=6 的记账行 + 恢复 is_admin 语义
	//
	// ⚠️ 必须删 ">= 6" 而不是 "= 6":schema_version 是**累积行表**,MAX(version) 才是
	// 当前版本。STAGE6 把 schemaVersion 提到 7 之后,库里同时有 6 和 7 两行,
	// 只删 6 的话 MAX 仍是 7,门控 `curVersion < 6` 不成立、回填被跳过。
	// (这条最初写成 "= 6",在 STAGE6 升版本时立刻暴露)
	if _, err := s.DB().Exec(`DELETE FROM schema_version WHERE version >= ?`, stage5SchemaVersion); err != nil {
		t.Fatalf("rewind version: %v", err)
	}
	id, err := s.UserInsert(User{Name: "legacy-admin", Status: 1})
	if err != nil {
		t.Fatalf("insert legacy admin: %v", err)
	}
	if _, err := s.DB().Exec(`UPDATE users SET is_admin = 1, role = 'user' WHERE id = ?`, id); err != nil {
		t.Fatalf("set legacy is_admin: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 3) 重开 — 此刻 MAX(version)=5 < 6,回填应当执行
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen for backfill: %v", err)
	}
	u, err := s2.UserGet(id)
	if err != nil {
		t.Fatalf("get legacy admin: %v", err)
	}
	if u.Role != RoleOwner {
		t.Fatalf("回填后 role=%q,期望 %q(最早的 is_admin=1 用户应被兜底提为唯一 owner)",
			u.Role, RoleOwner)
	}
	if n, err := s2.CountActiveOwners(); err != nil || n != 1 {
		t.Fatalf("owner 数 = %d (err=%v),期望恰好 1", n, err)
	}

	// 4) 把它降级(rescue 之外的常规操作),然后再次重开
	if err := s2.UserUpdate(id, UserUpdatePatch{Role: strPtr(RoleUser)}); err != nil {
		t.Fatalf("demote: %v", err)
	}
	if err := s2.Close(); err != nil {
		t.Fatalf("close 2: %v", err)
	}
	s3, err := Open(path)
	if err != nil {
		t.Fatalf("reopen 3: %v", err)
	}
	defer func() { _ = s3.Close() }()

	u3, err := s3.UserGet(id)
	if err != nil {
		t.Fatalf("get after demote: %v", err)
	}
	// ← 这就是回归点:若门控失效,这里会因为 is_admin=1 而变回 admin/owner
	if u3.Role != RoleUser {
		t.Fatalf("重开后被重新提权:role=%q,期望 %q —— schema_version 门控失效(见契约 §4.2)",
			u3.Role, RoleUser)
	}
}

// ---------------------------------------------------------------------------
// CSV 组集合(§6.3)
// ---------------------------------------------------------------------------

func TestNormalizeGroupSetCSV(t *testing.T) {
	cases := []struct{ in, want string }{
		{" default , default , vip ", "default,vip"}, // trim + 去重 + 排序
		{"vip,default", "default,vip"},               // 排序
		{"svip", "svip"},
		{"", ""},
		{" , , ", ""},
		{"vip,,vip", "vip"},
	}
	for _, c := range cases {
		if got := NormalizeGroupSet(c.in); got != c.want {
			t.Errorf("NormalizeGroupSet(%q) = %q,期望 %q", c.in, got, c.want)
		}
	}
}

func TestValidGroupSet(t *testing.T) {
	ok := []string{"default", "vip", "svip", "default,vip", "default,vip,svip"}
	for _, in := range ok {
		if !ValidGroupSet(in) {
			t.Errorf("ValidGroupSet(%q) = false,期望 true", in)
		}
	}
	bad := []string{"", " , ", "gold", "vip,gold", "DEFAULT"}
	for _, in := range bad {
		if ValidGroupSet(in) {
			t.Errorf("ValidGroupSet(%q) = true,期望 false", in)
		}
	}
}

// ---------------------------------------------------------------------------
// owner 唯一性(§5 / §6.2)
// ---------------------------------------------------------------------------

func TestOwnerUniqueIndexBlocksSecondOwner(t *testing.T) {
	s := openTestStore(t)

	if _, err := s.UserInsert(User{Name: "o1", Status: 1, Role: RoleOwner}); err != nil {
		t.Fatalf("insert owner 1: %v", err)
	}
	_, err := s.UserInsert(User{Name: "o2", Status: 1, Role: RoleOwner})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("插入第二个 owner 的 err = %v,期望 ErrDuplicate(partial unique index 未生效)", err)
	}
	if n, err := s.CountActiveOwners(); err != nil || n != 1 {
		t.Fatalf("owner 数 = %d (err=%v),期望 1", n, err)
	}
}

// TestOwnerSoftDeleteDoesNotOccupyIndex 锁死"软删 owner 不占位"这条谓词。
//
// 若索引写成 `WHERE role='owner'`(不带 status<>3),一个被软删的 owner 会永久占位:
// 索引挡住新 owner、软删账号又登不进去、且没有"取消软删"接口 ⇒ 系统锁死。
func TestOwnerSoftDeleteDoesNotOccupyIndex(t *testing.T) {
	s := openTestStore(t)

	oldID, err := s.UserInsert(User{Name: "oldowner", Status: 1, Role: RoleOwner})
	if err != nil {
		t.Fatalf("insert owner: %v", err)
	}
	if _, err := s.DB().Exec(`UPDATE users SET status = ? WHERE id = ?`, UserDeleteStatus, oldID); err != nil {
		t.Fatalf("soft delete owner: %v", err)
	}
	// 软删后:可用 owner 数归零
	if n, err := s.CountActiveOwners(); err != nil || n != 0 {
		t.Fatalf("软删后 owner 数 = %d (err=%v),期望 0", n, err)
	}
	// 索引不应再占位 ⇒ 能建出新的 owner
	if _, err := s.UserInsert(User{Name: "newowner", Status: 1, Role: RoleOwner}); err != nil {
		t.Fatalf("软删 owner 占位了唯一索引,新 owner 建不出来: %v", err)
	}
}

func TestEnsureOwnerExistsIdempotent(t *testing.T) {
	s := openTestStore(t)

	// 空库:不应凭空造 owner(setup 会创建)
	if n, err := s.EnsureOwnerExists(); err != nil || n != 0 {
		t.Fatalf("空库 EnsureOwnerExists = (%d, %v),期望 (0, nil)", n, err)
	}

	a, err := s.UserInsert(User{Name: "a", Status: 1})
	if err != nil {
		t.Fatalf("insert a: %v", err)
	}
	if _, err := s.DB().Exec(`UPDATE users SET is_admin = 1 WHERE id = ?`, a); err != nil {
		t.Fatalf("mark a: %v", err)
	}
	if _, err := s.UserInsert(User{Name: "b", Status: 1}); err != nil {
		t.Fatalf("insert b: %v", err)
	}

	got, err := s.EnsureOwnerExists()
	if err != nil {
		t.Fatalf("EnsureOwnerExists: %v", err)
	}
	if got != a {
		t.Fatalf("提升的 user_id = %d,期望 %d(最早的 is_admin=1)", got, a)
	}
	// 幂等:第二次什么都不做
	got2, err := s.EnsureOwnerExists()
	if err != nil {
		t.Fatalf("EnsureOwnerExists 2: %v", err)
	}
	if got2 != 0 {
		t.Fatalf("第二次 EnsureOwnerExists 返回 %d,期望 0(幂等)", got2)
	}
	if n, _ := s.CountActiveOwners(); n != 1 {
		t.Fatalf("owner 数 = %d,期望 1", n)
	}
}

// TestEnsureOwnerExistsSkipsDisabledCandidates 覆盖"候选必须 status=1"这条强化。
//
// 把 owner 提给一个禁用/软删账号等于没提:系统仍然没有能登录的管理员。
func TestEnsureOwnerExistsSkipsDisabledCandidates(t *testing.T) {
	s := openTestStore(t)

	dis, err := s.UserInsert(User{Name: "disabled-admin", Status: 1})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := s.DB().Exec(`UPDATE users SET is_admin = 1, status = 2 WHERE id = ?`, dis); err != nil {
		t.Fatalf("disable: %v", err)
	}
	enabled, err := s.UserInsert(User{Name: "enabled", Status: 1})
	if err != nil {
		t.Fatalf("insert enabled: %v", err)
	}

	got, err := s.EnsureOwnerExists()
	if err != nil {
		t.Fatalf("EnsureOwnerExists: %v", err)
	}
	if got != enabled {
		t.Fatalf("提升的 user_id = %d,期望 %d(禁用用户不可当 owner)", got, enabled)
	}
}

// ---------------------------------------------------------------------------
// TransferOwner(§6.5 / 定案 A3)
// ---------------------------------------------------------------------------

func TestTransferOwner(t *testing.T) {
	s := openTestStore(t)

	oldID, err := s.UserInsert(User{Name: "owner", Status: 1, Role: RoleOwner})
	if err != nil {
		t.Fatalf("insert owner: %v", err)
	}
	newID, err := s.UserInsert(User{Name: "admin", Status: 1, Role: RoleAdmin})
	if err != nil {
		t.Fatalf("insert target: %v", err)
	}

	if err := s.TransferOwner(oldID, newID); err != nil {
		t.Fatalf("TransferOwner: %v", err)
	}
	old, _ := s.UserGet(oldID)
	nw, _ := s.UserGet(newID)
	if old.Role != RoleAdmin {
		t.Errorf("旧 owner role = %q,期望 %q", old.Role, RoleAdmin)
	}
	if nw.Role != RoleOwner {
		t.Errorf("新 owner role = %q,期望 %q", nw.Role, RoleOwner)
	}
	if n, _ := s.CountActiveOwners(); n != 1 {
		t.Errorf("转让后 owner 数 = %d,期望 1", n)
	}

	// ① old == new → ErrInvalid(调用方应短路成幂等 200,不能落到这里执行 UPDATE)
	if err := s.TransferOwner(newID, newID); !errors.Is(err, ErrInvalid) {
		t.Errorf("自我转让 err = %v,期望 ErrInvalid", err)
	}
	// ② 目标不存在 → ErrNotFound
	if err := s.TransferOwner(newID, 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("目标不存在 err = %v,期望 ErrNotFound", err)
	}
	// ③ 目标禁用 → ErrInvalid(转给登不进的账号 = 仍然没有可用 owner)
	disID, err := s.UserInsert(User{Name: "disabled", Status: 2})
	if err != nil {
		t.Fatalf("insert disabled: %v", err)
	}
	if err := s.TransferOwner(newID, disID); !errors.Is(err, ErrInvalid) {
		t.Errorf("目标禁用 err = %v,期望 ErrInvalid", err)
	}
	// 转让失败不应改动任何角色
	nw2, _ := s.UserGet(newID)
	if nw2.Role != RoleOwner {
		t.Errorf("失败的转让改动了角色:role=%q", nw2.Role)
	}
}

// ---------------------------------------------------------------------------
// HasUsableOwner(定案 A4:清空 admin_key 的前置检查)
// ---------------------------------------------------------------------------

func TestHasUsableOwner(t *testing.T) {
	s := openTestStore(t)

	// 无用户 → false
	if ok, err := s.HasUsableOwner(); err != nil || ok {
		t.Fatalf("空库 HasUsableOwner = (%v, %v),期望 (false, nil)", ok, err)
	}
	// owner 但没设密码 → false(登录不了等于没有 owner)
	id, err := s.UserInsert(User{Name: "o", Status: 1, Role: RoleOwner})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if ok, _ := s.HasUsableOwner(); ok {
		t.Fatal("无密码 owner 不应算可用")
	}
	// 设了密码 → true
	if _, err := s.DB().Exec(`UPDATE users SET password_hash = 'x' WHERE id = ?`, id); err != nil {
		t.Fatalf("set password: %v", err)
	}
	if ok, err := s.HasUsableOwner(); err != nil || !ok {
		t.Fatalf("HasUsableOwner = (%v, %v),期望 (true, nil)", ok, err)
	}
	// 禁用了 → false
	if _, err := s.DB().Exec(`UPDATE users SET status = 2 WHERE id = ?`, id); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if ok, _ := s.HasUsableOwner(); ok {
		t.Fatal("被禁用的 owner 不应算可用")
	}
}

// ---------------------------------------------------------------------------
// is_admin 冻结(§6.1)
// ---------------------------------------------------------------------------

// TestIsAdminIsDerivedFromRole 锁死"运行期不再读 is_admin 列":
// 把列改成 1 也必须不影响派生值,把 role 改成 admin 则派生值必须为 true。
func TestIsAdminIsDerivedFromRole(t *testing.T) {
	s := openTestStore(t)

	id, err := s.UserInsert(User{Name: "u", Status: 1, Role: RoleUser})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// 新建用户的 is_admin 列必须恒为 0(冻结列,不做双写)
	var raw int
	if err := s.DB().QueryRow(`SELECT is_admin FROM users WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatalf("read raw is_admin: %v", err)
	}
	if raw != 0 {
		t.Fatalf("新建用户的 is_admin 列 = %d,期望 0(冻结列,不做双写)", raw)
	}
	// 直接改列(模拟历史脏值):派生值不应受影响
	if _, err := s.DB().Exec(`UPDATE users SET is_admin = 1 WHERE id = ?`, id); err != nil {
		t.Fatalf("poke is_admin: %v", err)
	}
	u, _ := s.UserGet(id)
	if u.IsAdmin {
		t.Fatal("IsAdmin 竟然来自 is_admin 列 —— 该列已冻结,必须由 role 派生")
	}
	// 改 role → 派生值随之变化
	if err := s.UserUpdate(id, UserUpdatePatch{Role: strPtr(RoleAdmin)}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	u2, _ := s.UserGet(id)
	if !u2.IsAdmin {
		t.Fatal("role=admin 时 IsAdmin 应为 true")
	}
}

// ---------------------------------------------------------------------------
// 渠道组集合(§3.3)
// ---------------------------------------------------------------------------

func TestChannelGroupNormalizedAndValidated(t *testing.T) {
	s := openTestStore(t)

	// 省略 → 默认 default
	id, err := s.ChannelInsert(Channel{
		Name: "c1", Provider: "volcano",
		CredentialsJSON: `{"api_key":"k","scope":{"resource_id":"r"}}`,
	})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	c, err := s.ChannelGet(id)
	if err != nil {
		t.Fatalf("get channel: %v", err)
	}
	if c.ChannelGroup != "default" {
		t.Fatalf("默认 channel_group = %q,期望 default", c.ChannelGroup)
	}

	// 归一化:vip,default / 带空格 → default,vip
	g := " vip , default "
	if err := s.ChannelUpdate(id, ChannelUpdate{ChannelGroup: &g}); err != nil {
		t.Fatalf("update channel_group: %v", err)
	}
	c2, _ := s.ChannelGet(id)
	if c2.ChannelGroup != "default,vip" {
		t.Fatalf("归一化后 = %q,期望 default,vip", c2.ChannelGroup)
	}

	// 非法值 → ErrInvalid
	bad := "gold"
	if err := s.ChannelUpdate(id, ChannelUpdate{ChannelGroup: &bad}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("非法 channel_group err = %v,期望 ErrInvalid", err)
	}
	// 空集 → ErrInvalid(禁止清空)
	empty := " "
	if err := s.ChannelUpdate(id, ChannelUpdate{ChannelGroup: &empty}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("空 channel_group err = %v,期望 ErrInvalid", err)
	}
}

// strPtr 是 *string 的小工具(测试内共用)。
func strPtr(s string) *string { return &s }
