package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// User 是 users 表的一行。
type User struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Status    int    `json:"status"`
	Quota     int64  `json:"quota"`      // 3b 用
	UsedQuota int64  `json:"used_quota"` // 3b 用
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// UserInsert 新建用户;name 冲突 → ErrDuplicate。
func (s *Store) UserInsert(u User) (int64, error) {
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
	res, err := s.db.Exec(`
		INSERT INTO users (name, status, quota, used_quota, created_at, updated_at)
		VALUES (?, ?, ?, ?, datetime('now'), datetime('now'))`,
		u.Name, u.Status, u.Quota, u.UsedQuota)
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

// UserGet 按 id 查;未命中 → ErrNotFound。
func (s *Store) UserGet(id int64) (*User, error) {
	row := s.db.QueryRow(`SELECT id, name, status, quota, used_quota, created_at, updated_at
	                     FROM users WHERE id = ?`, id)
	var u User
	err := row.Scan(&u.ID, &u.Name, &u.Status, &u.Quota, &u.UsedQuota, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: user get id=%d: %w", id, err)
	}
	return &u, nil
}

// UserGetByName 按 name 查;未命中 → ErrNotFound。
// seed 阶段用(决策 #4:先查"default" user 是否已存在,避免重复创建)。
func (s *Store) UserGetByName(name string) (*User, error) {
	row := s.db.QueryRow(`SELECT id, name, status, quota, used_quota, created_at, updated_at
	                     FROM users WHERE name = ?`, strings.TrimSpace(name))
	var u User
	err := row.Scan(&u.ID, &u.Name, &u.Status, &u.Quota, &u.UsedQuota, &u.CreatedAt, &u.UpdatedAt)
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
	rows, err := s.db.Query(`SELECT id, name, status, quota, used_quota, created_at, updated_at
	                        FROM users ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: user list: %w", err)
	}
	defer rows.Close()
	out := make([]User, 0, 8)
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Name, &u.Status, &u.Quota, &u.UsedQuota, &u.CreatedAt, &u.UpdatedAt); err != nil {
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

	q := `SELECT u.id, u.name, u.status, u.quota, u.used_quota, u.created_at, u.updated_at,
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
	Name   *string
	Status *int
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

	sets := make([]string, 0, 3)
	args := make([]any, 0, 4)
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
