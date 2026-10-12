package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrInsufficientBalance 表示余额不足以完成本次**手动**调整(不允许扣成负)。
//
// ⚠️ 该约束只适用于手动调整/兑换/赠送路径(UserAddBalance*);
// 调用结算的补扣(UsageSettle)不受此约束 —— 允许把余额扣成负,
// 负余额由"欠费停服"处理。见 docs/STAGE4_PORTAL_BILLING_PLAN.md 决策 X24。
var ErrInsufficientBalance = errors.New("store: insufficient balance")

// User 是 users 表的一行。
//
// STAGE4 新增字段:
//   - PasswordHash:bcrypt 串(空串 = 未设密,配合 D29 永不签发凭证)
//   - IsAdmin:是否管理员(seed 的 default 用户迁移时置 1)
//   - Balance:钱包余额 —— 调用扣费的**主源**(决策 C),可为负(欠费)
//   - TokenVersion:改密踢旧用(D9);JWT claims 带 ver,不一致即 401
type User struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Status    int    `json:"status"`
	Quota     int64  `json:"quota"`      // 3b 遗留,已废弃;保留列但不读写
	UsedQuota int64  `json:"used_quota"` // 聚合对账镜像(结算时累加 actual)
	// PasswordHash 永远不参与 JSON 序列化(json:"-"),防止任何直接序列化泄露。
	PasswordHash string `json:"-"`
	IsAdmin      bool   `json:"is_admin"`
	Balance      int64  `json:"balance"`
	TokenVersion int64  `json:"token_version"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// userColumns 是 users 表的统一 SELECT 列顺序;
// UserGet/UserGetByName/UserList/UserSearch 共用,避免四处 scan 顺序漂移。
const userColumns = `id, name, status, quota, used_quota, password_hash, is_admin, balance, token_version, created_at, updated_at`

// scanUser 按 userColumns 的顺序把一行扫成 User。
func scanUser(r scanner) (User, error) {
	var u User
	err := r.Scan(&u.ID, &u.Name, &u.Status, &u.Quota, &u.UsedQuota,
		&u.PasswordHash, &u.IsAdmin, &u.Balance, &u.TokenVersion, &u.CreatedAt, &u.UpdatedAt)
	return u, err
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
	res, err := e.Exec(`
		INSERT INTO users (name, status, quota, used_quota, password_hash, is_admin, balance, token_version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, datetime('now'), datetime('now'))`,
		u.Name, u.Status, u.Quota, u.UsedQuota, u.PasswordHash, u.IsAdmin, u.Balance)
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
	             u.password_hash, u.is_admin, u.balance, u.token_version,
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
			&it.PasswordHash, &it.IsAdmin, &it.Balance, &it.TokenVersion,
			&it.CreatedAt, &it.UpdatedAt, &it.TokenCount); err != nil {
			return nil, 0, fmt.Errorf("store: user search scan: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: user search rows: %w", err)
	}
	return out, total, nil
}

// UserUpdatePatch 是 user 部分更新的入参(指针语义:nil=不改)。
// Status 只接受 1(enabled)/2(disabled):3(deleted)必须走 UserSoftDelete,
// 避免"用 PATCH 绕过删除语义"。
type UserUpdatePatch struct {
	Name    *string
	Status  *int
	IsAdmin *bool // STAGE4:管理员标记升降级(受"最后一个 admin 保护"约束,见 D26)
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

	sets := make([]string, 0, 4)
	args := make([]any, 0, 5)
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
	if p.IsAdmin != nil {
		sets = append(sets, "is_admin=?")
		args = append(args, *p.IsAdmin)
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

// UserCountActiveAdmins 统计仍可用的管理员数(is_admin=1 且 status=1)。
// 供"最后一个 admin 保护"判定(决策 D26):不允许把唯一管理员降级 / 禁用 / 删除。
func (s *Store) UserCountActiveAdmins() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE is_admin = 1 AND status = 1`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count active admins: %w", err)
	}
	return n, nil
}

// UserListAll 返回全部用户(不分页),供 admin 侧需要全量遍历的场景(如批量充值脚本)。
func (s *Store) UserListAll() ([]User, error) {
	return s.UserList()
}
