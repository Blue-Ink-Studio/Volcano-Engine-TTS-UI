package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrInsufficientBalance 表示余额不足以完成本次**手动**调整(不允许扣成负)。
//
// ⚠️ 该约束只适用于手动调整/兑换/赠送路径(UserAddBalance*);
// 调用结算的补扣(UsageSettle)不受此约束 —— 允许把余额扣成负,
// 负余额由"欠费停服"处理。见 docs/STAGE4_PORTAL_BILLING_PLAN.md 决策 X24。
var ErrInsufficientBalance = errors.New("store: insufficient balance")

// 角色常量(STAGE5):role 是权限的**唯一真源**。
const (
	RoleOwner = "owner" // 超管:唯一且可转让
	RoleAdmin = "admin" // 管理员:可管用户/充值,但动不了 owner
	RoleUser  = "user"  // 普通用户
)

// RoleIsAdmin 把 role 映射为对外派生的 is_admin(STAGE5 §6.1)。
// 前端读写路径都基于它,从而"删列不删语义"。
func RoleIsAdmin(role string) bool { return role == RoleOwner || role == RoleAdmin }

// ValidRole 校验 role 取值。
func ValidRole(role string) bool {
	switch role {
	case RoleOwner, RoleAdmin, RoleUser:
		return true
	}
	return false
}

// 用户分组白名单(STAGE5 §3.3:本期只做数据层约束,路由匹配后续做)。
//
// 硬编码枚举 —— 本期没有 group 表;将来做分组表时再迁。
var allowedUserGroups = []string{"default", "vip", "svip"}

// ValidUserGroup 校验 `user_group` 的集合(语义等同 ValidGroupSet,保留此名以贴合调用点)。
func ValidUserGroup(csv string) bool { return ValidGroupSet(csv) }

// ValidGroupSet 校验**组集合 CSV**:非空、每一项都在白名单内。
//
// `user_group`(用户可用的组)与 `channel_group`(渠道服务的组)取值域相同,
// 因此共用一个校验器 —— 将来做分组表时只需改这里一处。
func ValidGroupSet(csv string) bool {
	set, ok := parseCSVSet(csv)
	if !ok || len(set) == 0 {
		return false
	}
	for g := range set {
		found := false
		for _, a := range allowedUserGroups {
			if g == a {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// NormalizeUserGroup 归一化组集合 CSV(语义等同 NormalizeGroupSet,保留此名以贴合调用点)。
func NormalizeUserGroup(csv string) string { return NormalizeGroupSet(csv) }

// NormalizeGroupSet 归一化**组集合 CSV**:trim + 去重 + 排序,再拼回 CSV。
//
// 为什么必须归一化:同一语义的多种写法("default, vip" / "vip,default" / "default,default")
// 会让将来的匹配逻辑与排查都变难;而且 `',vip,'` 这种带分隔符的成员判断要求
// 前后无空格(见契约 §6.3)。
//
// user_group 与 channel_group 共用(取值域相同)。
func NormalizeGroupSet(csv string) string { return normalizeCSVSet(csv) }

// parseCSVSet 把 CSV 拆成集合;返回 (集合, 是否解析成功)。
// 空串 → 空集合(由调用方判断是否允许)。
func parseCSVSet(csv string) (map[string]struct{}, bool) {
	set := make(map[string]struct{})
	for _, raw := range strings.Split(csv, ",") {
		g := strings.TrimSpace(raw)
		if g == "" {
			continue
		}
		set[g] = struct{}{}
	}
	return set, true
}

// normalizeCSVSet = trim + 去重 + **字典序排序**,再拼回 CSV。
func normalizeCSVSet(csv string) string {
	set, _ := parseCSVSet(csv)
	if len(set) == 0 {
		return ""
	}
	out := make([]string, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// User 是 users 表的一行。
//
// STAGE4 引入钱包与登录态;STAGE5 引入 role/user_group 并**冻结 is_admin**:
//   - Role:权限唯一真源(owner/admin/user)
//   - UserGroup:组集合 CSV(如 "default,vip"),默认 "default"
//   - IsAdmin:**派生字段**,由 Role 计算而来,**不再来自 is_admin 列**
//     (该列已冻结、新建恒 0,唯一的用途是升级时的历史输入)
type User struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Status    int    `json:"status"`
	Quota     int64  `json:"quota"`      // 3b 遗留,已废弃;保留列但不读写
	UsedQuota int64  `json:"used_quota"` // 聚合对账镜像(结算时累加 actual)
	// PasswordHash 永远不参与 JSON 序列化(json:"-"),防止任何直接序列化泄露。
	PasswordHash string `json:"-"`
	Role         string `json:"role"`
	UserGroup    string `json:"user_group"`
	IsAdmin      bool   `json:"is_admin"` // 派生:RoleIsAdmin(Role)
	Balance      int64  `json:"balance"`
	TokenVersion int64  `json:"token_version"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// userColumns 是 users 表的统一 SELECT 列顺序;
// UserGet/UserGetByName/UserList/UserSearch 共用,避免四处 scan 顺序漂移。
//
// ⚠️ STAGE5 起**不再 SELECT `is_admin`**(冻结列,运行期一律不读,见契约 §6.1);
// `IsAdmin` 由 Role 派生。迁移路径仍可读该列(它是历史输入),见 db.go 的回填。
const userColumns = `id, name, status, quota, used_quota, password_hash, role, user_group, balance, token_version, created_at, updated_at`

// scanUser 按 userColumns 的顺序把一行扫成 User。
func scanUser(r scanner) (User, error) {
	var u User
	err := r.Scan(&u.ID, &u.Name, &u.Status, &u.Quota, &u.UsedQuota,
		&u.PasswordHash, &u.Role, &u.UserGroup, &u.Balance, &u.TokenVersion,
		&u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return u, err
	}
	u.IsAdmin = RoleIsAdmin(u.Role)
	return u, nil
}

// execer 抽象 *sql.DB 与 *sql.Tx 的 Exec,便于插入语句在两种上下文复用。
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// insertUser 是 UserInsert / UserCreateWithBalance 共用的插入核心(UNIQUE 冲突 → ErrDuplicate)。
func insertUser(e execer, u User) (int64, error) {
	u.Name = strings.TrimSpace(u.Name)
	if u.Name == "" {
		return 0, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	if u.Status == 0 {
		u.Status = 1
	}
	if u.Status != 1 && u.Status != 2 {
		return 0, fmt.Errorf("%w: status must be 1 or 2", ErrInvalid)
	}

	// STAGE5:role / user_group 的默认值与校验。
	// is_admin **恒写 0** —— 它是冻结列,不再反映真实角色(契约 §6.1);
	// role 才是唯一真源。owner 的唯一性由 partial unique index 兜底(第二个会 ErrDuplicate)。
	if u.Role == "" {
		u.Role = RoleUser
	}
	if !ValidRole(u.Role) {
		return 0, fmt.Errorf("%w: role must be owner/admin/user", ErrInvalid)
	}
	if u.UserGroup == "" {
		u.UserGroup = "default"
	}
	u.UserGroup = NormalizeUserGroup(u.UserGroup)
	if !ValidUserGroup(u.UserGroup) {
		return 0, fmt.Errorf("%w: user_group must be a non-empty subset of default/vip/svip", ErrInvalid)
	}

	res, err := e.Exec(`
		INSERT INTO users (name, status, quota, used_quota, password_hash, role, user_group, is_admin, balance, token_version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, 0, datetime('now'), datetime('now'))`,
		u.Name, u.Status, u.Quota, u.UsedQuota, u.PasswordHash, u.Role, u.UserGroup, u.Balance)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrDuplicate
		}
		return 0, fmt.Errorf("store: user insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: user insert lastid: %w", err)
	}
	return id, nil
}

// UserInsert 新建用户(不写初始余额流水);name 冲突 → ErrDuplicate。
//
// 需要审计初始余额时请用 UserCreateWithBalance(admin 后台建号 + 显式填余额的场景)。
func (s *Store) UserInsert(u User) (int64, error) {
	return insertUser(s.db, u)
}

// UserCreateWithBalance 原子创建用户 + 写初始余额流水(决策 D33:admin 建号填 balance 要记账)。
//
// balance <= 0 时不写 charge_logs(0 元无需审计),此时等价于 UserInsert。
// source 建议 ChargeSourceAdminCreate。
func (s *Store) UserCreateWithBalance(u User, balance int64, source, remark string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: user create begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	id, err := insertUser(tx, u)
	if err != nil {
		return 0, err
	}
	if balance > 0 {
		if _, err := s.UserAddBalanceTx(tx, id, balance, ChargeTypeGrant, source, remark); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: user create commit: %w", err)
	}
	return id, nil
}

// UserGet 按 id 查;未命中 → ErrNotFound。
func (s *Store) UserGet(id int64) (*User, error) {
	row := s.db.QueryRow(`SELECT `+userColumns+`
	                     FROM users WHERE id = ?`, id)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: user get id=%d: %w", id, err)
	}
	return &u, nil
}

// UserGetByName 按 name 查;未命中 → ErrNotFound。
// seed 阶段用(决策 #4:先查"default" user 是否已存在,避免重复创建);
// STAGE4 起也用于登录(凭 name 取 password_hash / is_admin / token_version)。
func (s *Store) UserGetByName(name string) (*User, error) {
	row := s.db.QueryRow(`SELECT `+userColumns+`
	                     FROM users WHERE name = ?`, strings.TrimSpace(name))
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: user getbyname %q: %w", name, err)
	}
	return &u, nil
}

// UserCount 统计用户数;admin 仪表盘用(预留)。
func (s *Store) UserCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: user count: %w", err)
	}
	return n, nil
}

// UserList 列全表(ORDER BY id ASC)。user 表行数很小(几十~几百),
// 不分页,直接全量;3b 加配额后可加 limit/offset。
func (s *Store) UserList() ([]User, error) {
	rows, err := s.db.Query(`SELECT ` + userColumns + `
	                        FROM users ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: user list: %w", err)
	}
	defer rows.Close()
	out := make([]User, 0, 8)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("store: user list scan: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: user list rows: %w", err)
	}
	return out, nil
}

// ---- 阶段 3b:用户写操作 + 带 token_count 的列表 ----------------------

// UserDeleteStatus 是软删标记(决策 #2:users.status=3 deleted)。
const UserDeleteStatus = 3

// UserListItem 是列表项:user 字段 + 有效 token 数(决策 #1,JOIN COUNT)。
type UserListItem struct {
	User
	TokenCount int `json:"token_count"`
}

// UserSearch 按 name 模糊搜索 + 分页,返回 (列表, 满足条件的总数)。
//
//   - search 为空 → 全部;非空 → name LIKE %search%(转义 % 与 _ 防通配注入)
//   - 软删用户(decision #2 status=3)默认**仍然返回**(管理端要能看到/审计),
//     由前端按 status 展示;若后续要隐藏,在此加 WHERE status<>3 即可
//   - token_count 只统计有效 token(status=1),与契约"有效 token 数"一致
//   - ORDER BY id ASC 稳定顺序
func (s *Store) UserSearch(search string, limit, offset int) ([]UserListItem, int, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	search = strings.TrimSpace(search)

	where := ""
	args := make([]any, 0, 3)
	if search != "" {
		// 转义 LIKE 通配符,避免用户输入的 % / _ 变成通配
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(search)
		where = ` WHERE u.name LIKE ? ESCAPE '\'`
		args = append(args, "%"+esc+"%")
	}

	// 总数
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users u`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: user search count: %w", err)
	}

	q := `SELECT u.id, u.name, u.status, u.quota, u.used_quota,
	             u.password_hash, u.role, u.user_group, u.balance, u.token_version,
	             u.created_at, u.updated_at,
	             (SELECT COUNT(*) FROM tokens t WHERE t.user_id = u.id AND t.status = 1)
	      FROM users u` + where + ` ORDER BY u.id ASC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: user search: %w", err)
	}
	defer rows.Close()
	out := make([]UserListItem, 0, limit)
	for rows.Next() {
		var it UserListItem
		if err := rows.Scan(&it.ID, &it.Name, &it.Status, &it.Quota, &it.UsedQuota,
			&it.PasswordHash, &it.Role, &it.UserGroup, &it.Balance, &it.TokenVersion,
			&it.CreatedAt, &it.UpdatedAt, &it.TokenCount); err != nil {
			return nil, 0, fmt.Errorf("store: user search scan: %w", err)
		}
		it.IsAdmin = RoleIsAdmin(it.Role) // 派生,不读 is_admin 列
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: user search rows: %w", err)
	}
	return out, total, nil
}

// UserUpdatePatch 是 user 部分更新的入参(指针语义:nil=不改)。
//
// STAGE5:`IsAdmin *bool` 已由 `Role`/`UserGroup` 取代 —— role 是唯一真源。
// API 层的 `is_admin` 兼容别名在 **controller** 里映射为 role(契约 §6.2),
// store 层不再感知 `is_admin`(那是冻结列)。
//
// Status 只接受 1(enabled)/2(disabled):3(deleted)必须走 UserSoftDelete,
// 避免"用 PATCH 绕过删除语义"。
type UserUpdatePatch struct {
	Name      *string
	Status    *int
	Role      *string
	UserGroup *string
}

// UserUpdate 按字段部分更新;未命中 → ErrNotFound;
// name 重名 → ErrDuplicate;软删用户(status=3)视为不存在 → ErrNotFound。
func (s *Store) UserUpdate(id int64, p UserUpdatePatch) error {
	if p.Status != nil && *p.Status != 1 && *p.Status != 2 {
		return fmt.Errorf("%w: status must be 1 (enabled) or 2 (disabled); use DELETE for soft delete", ErrInvalid)
	}

	// 先取行:校验存在 + 排除软删
	cur, err := s.UserGet(id)
	if err != nil {
		return err
	}
	if cur.Status == UserDeleteStatus {
		return ErrNotFound
	}

	sets := make([]string, 0, 5)
	args := make([]any, 0, 6)
	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" {
			return fmt.Errorf("%w: name is required", ErrInvalid)
		}
		sets = append(sets, "name=?")
		args = append(args, name)
	}
	if p.Status != nil {
		sets = append(sets, "status=?")
		args = append(args, *p.Status)
	}
	if p.Role != nil {
		if !ValidRole(*p.Role) {
			return fmt.Errorf("%w: role must be owner/admin/user", ErrInvalid)
		}
		// owner 的"上位"必须走 TransferOwner(原子,契约 §6.5);
		// 通用 PATCH 只负责把 role 写成 admin/user,以及由 controller 拦住的 last_owner 场景。
		sets = append(sets, "role=?")
		args = append(args, *p.Role)
	}
	if p.UserGroup != nil {
		norm := NormalizeUserGroup(*p.UserGroup)
		if !ValidUserGroup(norm) {
			return fmt.Errorf("%w: user_group must be a non-empty subset of default/vip/svip", ErrInvalid)
		}
		sets = append(sets, "user_group=?")
		args = append(args, norm)
	}
	if len(sets) == 0 {
		return nil // 空 patch:合法 noop(上面已确认存在且未软删)
	}
	sets = append(sets, "updated_at=datetime('now')")
	args = append(args, id)

	if _, err := s.db.Exec(`UPDATE users SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicate
		}
		return fmt.Errorf("store: user update id=%d: %w", id, err)
	}
	return nil
}

// UserSoftDelete 软删(decision #2:置 status=3,数据保留,usage 保留)。
// 已软删或不存在 → ErrNotFound(幂等语义:DELETE 两次第二次 404)。
//
// 说明:不改该 user 下 token 的 status —— 鉴权链路的 user.status 检查
// (middleware.AuthenticateWithStore)已足以让这些 key 直接 403 user_disabled。
func (s *Store) UserSoftDelete(id int64) error {
	res, err := s.db.Exec(`UPDATE users SET status=?, updated_at=datetime('now') WHERE id = ? AND status <> ?`,
		UserDeleteStatus, id, UserDeleteStatus)
	if err != nil {
		return fmt.Errorf("store: user soft delete id=%d: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ===== STAGE4:钱包 / 凭据 / 管理员保护 =====

// charge_logs.type 取值(决策 D25)。
// 调用消费在 usage.actual_quota、退款在 usage.status,均**不**写本表。
const (
	ChargeTypeCharge       = "charge"        // 充值(资金流入)
	ChargeTypeRedeem       = "redeem"        // 充值码兑换
	ChargeTypeManualAdjust = "manual_adjust" // 手动调整(通常为负:扣回 / 纠错)
	ChargeTypeGrant        = "grant"         // 赠送(注册赠送 / 建号初始额度)
)

// charge_logs.source 取值(决策 D33)。
const (
	ChargeSourceManual        = "manual"
	ChargeSourceRedeem        = "redeem"
	ChargeSourceAdminCreate   = "admin_create"
	ChargeSourceRegisterBonus = "register_bonus"
)

// UserAddBalanceTx 在给定事务内调整用户余额并写 charge_logs,返回变更后的余额。
//
// 契约(§5.3 / X18 / X24):
//   - delta == 0 → ErrInvalid(空操作无意义)
//   - delta < 0 且会扣成负 → ErrInsufficientBalance
//     ⚠️ 该非负约束**只**适用于手动调整类路径;调用结算的补扣走 UsageSettle,不受约束
//   - 余额更新与流水插入必须**同事务**(X18),否则会出现"钱变了但没流水"
//   - typ 见 ChargeType*;source 见 ChargeSource*
//
// 并发安全:本 Store 是 SetMaxOpenConns(1),事务持有唯一连接期间不会有其它语句穿插,
// 因此"先读余额再写"是安全的(与 store/usage.go 的 UsageRefund 同一依据)。
func (s *Store) UserAddBalanceTx(tx *sql.Tx, userID, delta int64, typ, source, remark string) (int64, error) {
	if tx == nil {
		return 0, fmt.Errorf("%w: tx is required", ErrInvalid)
	}
	if delta == 0 {
		return 0, fmt.Errorf("%w: delta must not be zero", ErrInvalid)
	}
	var cur int64
	err := tx.QueryRow(`SELECT balance FROM users WHERE id = ? AND status <> ?`,
		userID, UserDeleteStatus).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: read balance user=%d: %w", userID, err)
	}
	next := cur + delta
	if next < 0 {
		return 0, ErrInsufficientBalance
	}
	if _, err := tx.Exec(`UPDATE users SET balance = ?, updated_at = datetime('now') WHERE id = ?`,
		next, userID); err != nil {
		return 0, fmt.Errorf("store: set balance user=%d: %w", userID, err)
	}
	if _, err := tx.Exec(`
		INSERT INTO charge_logs (user_id, type, delta, balance_after, source, remark, created_at)
		VALUES (?, ?, ?, ?, ?, ?, datetime('now'))`,
		userID, typ, delta, next, source, remark); err != nil {
		return 0, fmt.Errorf("store: insert charge_log user=%d: %w", userID, err)
	}
	return next, nil
}

// UserAddBalance 是 UserAddBalanceTx 的自管事务版本(单点调用:手动充值 / 兑换 / 赠送)。
func (s *Store) UserAddBalance(userID, delta int64, typ, source, remark string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: add balance begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	bal, err := s.UserAddBalanceTx(tx, userID, delta, typ, source, remark)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: add balance commit: %w", err)
	}
	return bal, nil
}

// UserSetPassword 写入密码散列并把 token_version +1(改密踢旧,决策 D9),返回新版本号。
//
// hash 必须是完整 bcrypt 串;空串表示清空密码(仅特殊场景,正常流程不应传入)。
// 软删或不存在 → ErrNotFound。
func (s *Store) UserSetPassword(userID int64, hash string) (int64, error) {
	res, err := s.db.Exec(`
		UPDATE users SET password_hash = ?, token_version = token_version + 1, updated_at = datetime('now')
		WHERE id = ? AND status <> ?`, hash, userID, UserDeleteStatus)
	if err != nil {
		return 0, fmt.Errorf("store: set password user=%d: %w", userID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	u, err := s.UserGet(userID)
	if err != nil {
		return 0, err
	}
	return u.TokenVersion, nil
}

// HasUsableOwner 报告是否存在**可用的 owner**:`role='owner'` 且 `status=1` 且**已设密码**。
//
// 用途(STAGE5 定案 A4):清空 `admin_key` 前的唯一硬前提检查。
// 管理入口现在以账号为主、admin_key 只是运维后门,所以真正会"锁死"的是
// "没有能登录的 owner",而不是旧判断里的"auth_key 为空"(auth_key 已退役)。
//
// 之所以还要查 `password_hash <> ''`:一个没设密的 owner 登录不了(D29),
// 等于没有 owner。
func (s *Store) HasUsableOwner() (bool, error) {
	var n int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM users
		WHERE role = ? AND status = 1 AND password_hash <> ''`, RoleOwner).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: has usable owner: %w", err)
	}
	return n > 0, nil
}

// CountActiveOwners 统计**可用 owner** 数(role='owner' 且未软删)。
//
// STAGE5 取代了 STAGE4 的 UserCountActiveAdmins:
//   - 唯一需要保护的是 owner(唯一且不可降级/删除/禁用)。"最后一个 admin"不再需要保护 ——
//     admin 被全部降级并无害,owner 随时能再提人(契约 §5/C2)。
func (s *Store) CountActiveOwners() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = ? AND status <> ?`,
		RoleOwner, UserDeleteStatus).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count active owners: %w", err)
	}
	return n, nil
}

// CurrentOwnerID 返回当前 owner 的 user_id;没有 owner 时返回 0。
// 供 TransferOwner 定位"旧 owner"(契约 §6.5)。
func (s *Store) CurrentOwnerID() (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM users WHERE role = ? AND status <> ? LIMIT 1`,
		RoleOwner, UserDeleteStatus).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: current owner id: %w", err)
	}
	return id, nil
}

// TransferOwner 原子转让 owner:旧 owner → admin,新 owner → owner。
//
// 为什么必须单条语句:partial unique index(`WHERE role='owner' AND status<>3`)是
// **每条语句后即时校验**的。若拆成两条且先升新,中间态会出现两个 owner ⇒ UNIQUE 失败。
// 单条 `UPDATE ... CASE ... WHERE id IN (old,new)` 没有中间态。
//
// 校验(契约 §6.5 / 定案 A3):
//   - oldID == newID → ErrInvalid(调用方应短路为幂等 no-op,返回 200)
//   - 目标不存在 → ErrNotFound
//   - 目标非启用(status != 1,含软删/禁用)→ ErrInvalid
//     (把 owner 转给一个登不进的账号 = 角色上仍然没有可用 owner,等于自锁)
func (s *Store) TransferOwner(oldID, newID int64) error {
	if oldID == newID {
		return fmt.Errorf("%w: cannot transfer owner to self", ErrInvalid)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: transfer owner begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var status int
	err = tx.QueryRow(`SELECT status FROM users WHERE id = ?`, newID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: transfer owner read target: %w", err)
	}
	if status != 1 {
		return fmt.Errorf("%w: target user must be enabled (status=1)", ErrInvalid)
	}

	// 单条 CASE UPDATE:无中间态。
	if _, err := tx.Exec(`
		UPDATE users SET role = CASE WHEN id = ? THEN ? WHEN id = ? THEN ? END,
		                  updated_at = datetime('now')
		WHERE id IN (?, ?)`,
		oldID, RoleAdmin, newID, RoleOwner, oldID, newID); err != nil {
		return fmt.Errorf("store: transfer owner update: %w", err)
	}

	// 回读断言:newID 必须真的成为 owner(否则说明按 id 没命中,属数据异常)。
	var role string
	if err := tx.QueryRow(`SELECT role FROM users WHERE id = ?`, newID).Scan(&role); err != nil {
		return fmt.Errorf("store: transfer owner readback: %w", err)
	}
	if role != RoleOwner {
		return fmt.Errorf("store: transfer owner failed: user %d role=%q", newID, role)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: transfer owner commit: %w", err)
	}
	return nil
}

// UserListAll 返回全部用户(不分页),供 admin 侧需要全量遍历的场景(如批量充值脚本)。
func (s *Store) UserListAll() ([]User, error) {
	return s.UserList()
}
