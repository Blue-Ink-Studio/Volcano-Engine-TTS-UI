package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Token 是 tokens 表的一行。
//
// STAGE4 语义变更(决策 C):
//   - 扣费主源已迁到 users.balance;本表退化为「认证凭证 + 可选准入限额」
//   - LimitQuota 是**可选准入阈值**:nil = 不限额度;非 nil = used_quota 超过即拒绝(402)
//   - RemainQuota **冻结**为历史值(迁移不改、不再更新),仅供只读展示
//   - UsedQuota 语义从"累计消费镜像"变为**限额预占计数器**:
//     预扣 +Pre、结算 +(Actual-Pre)、退款 -Pre ⇒ 净效果等于累计 actual
type Token struct {
	ID          int64  `json:"id"`
	UserID      int64  `json:"user_id"`
	Name        string `json:"name"`
	KeyHash     string `json:"-"`            // 不暴露 hash 给前端
	KeyPrefix   string `json:"key_prefix"`   // 明文前 8 字符,对账识别
	RemainQuota int64  `json:"remain_quota"` // 阶段 3b 遗留:已冻结,只读展示
	UsedQuota   int64  `json:"used_quota"`   // 限额预占计数器(含历史用量)
	// LimitQuota 是可选的**准入限额**:nil = 不限额度。
	// ⚠️ Unlimited=true 时本字段被**忽略**(不计费也就无所谓限额);
	// 不会因为置 unlimited 而自动清空它,以免"切换开关丢配置"。
	LimitQuota *int64 `json:"limit_quota"`
	Unlimited   bool   `json:"unlimited"`    // 1=不限(不计费,但仍受欠费停服约束)
	Status      int    `json:"status"`       // 1=enabled 2=disabled
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// tokenColumns 是 tokens 表的统一 SELECT 列顺序(四处查询共用,防 scan 顺序漂移)。
const tokenColumns = `id, user_id, name, key_hash, key_prefix, remain_quota, used_quota, limit_quota, unlimited, status, created_at, updated_at`

// HashKey 返回 sha256(key) 的 hex 编码(64 字符小写)。key_hash 列存这个值。
// 决策 #1:用 sha256 + hex 字符串,索引友好。
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// KeyPrefix 返回 key 的前 8 字符(明文)。key_prefix 列存这个值,仅用于对账识别。
// key 长度 < 8 时返整个 key。
func KeyPrefix(key string) string {
	if len(key) <= 8 {
		return key
	}
	return key[:8]
}

// TokenInsert 新建 token;name + user_id 不强唯一(允许同 user 多 token),
// 但 key_hash UNIQUE → 同 key 不能插两次。
func (s *Store) TokenInsert(t Token) (int64, error) {
	t.Name = strings.TrimSpace(t.Name)
	t.KeyHash = strings.TrimSpace(t.KeyHash)
	t.KeyPrefix = strings.TrimSpace(t.KeyPrefix)
	if t.UserID == 0 {
		return 0, fmt.Errorf("%w: user_id is required", ErrInvalid)
	}
	if t.Name == "" {
		return 0, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	if t.KeyHash == "" {
		return 0, fmt.Errorf("%w: key_hash is required", ErrInvalid)
	}
	if t.Status == 0 {
		t.Status = 1
	}
	if t.Status != 1 && t.Status != 2 {
		return 0, fmt.Errorf("%w: status must be 1 or 2", ErrInvalid)
	}
	return insertToken(s.db, t)
}

// sqlExecer 抽象 db 与 tx 的 Exec,让 insertToken 可被普通插入与事务复用。
type sqlExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// insertToken 是 TokenInsert 与 SeedDefaultTokens 共用的核心插入(UNIQUE 冲突 → ErrDuplicate)。
func insertToken(ex sqlExecer, t Token) (int64, error) {
	res, err := ex.Exec(`
		INSERT INTO tokens (user_id, name, key_hash, key_prefix, remain_quota, used_quota, limit_quota, unlimited, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))`,
		t.UserID, t.Name, t.KeyHash, t.KeyPrefix, t.RemainQuota, t.UsedQuota,
		t.LimitQuota, boolToInt(t.Unlimited), t.Status)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrDuplicate
		}
		return 0, fmt.Errorf("store: token insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: token insert lastid: %w", err)
	}
	return id, nil
}

// TokenGetByKeyHash 按 hash 查 token(无 JOIN user;调用方按 user_id 再查 user 状态)。
// 未命中 → ErrNotFound。
//
// 注意:不在此函数内 JOIN users,因为 status 校验分两步:
//   1) token.status=1  → 否则 403 token_disabled
//   2) user.status=1   → 否则 403 user_disabled
// 中间件顺序读两步,语义清晰。
func (s *Store) TokenGetByKeyHash(hash string) (*Token, error) {
	row := s.db.QueryRow(`SELECT `+tokenColumns+`
	                     FROM tokens WHERE key_hash = ?`, strings.TrimSpace(hash))
	t, err := scanToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: token get by hash: %w", err)
	}
	return &t, nil
}

// TokenCountEnabled 统计 enabled token 数。
func (s *Store) TokenCountEnabled() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM tokens WHERE status = 1`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: token count enabled: %w", err)
	}
	return n, nil
}

// TokenCount 统计 tokens 表总行数(不限 status)。seed 触发判断用:全表空才 seed。
func (s *Store) TokenCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM tokens`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: token count: %w", err)
	}
	return n, nil
}

// SeedDefaultTokens 原子 seed:把 authKeys 逐个插成 defaultUserName 下的 unlimited token。
// 单一事务:default user + 全部 tokens 成功才 commit,任一失败整体回滚(决策 #3)。
//
// 行为契约(参考 SetupApply 事务风格):
//   - 默认 user 不存在则建(name=defaultUserName,status=1),已存在则复用;
//   - token 已存在(key_hash UNIQUE 冲突)→ 静默跳过(ErrDuplicate,事务继续),
//     兼容"同一 key 重复 seed"场景;
//   - 其它 token 错误 → 整体回滚;
//   - 全部成功 → tx.Commit,返回实际插入的 token 数(不含被 ErrDuplicate 跳过的)。
func (s *Store) SeedDefaultTokens(authKeys []string, defaultUserName string) (int, error) {
	name := strings.TrimSpace(defaultUserName)
	if name == "" {
		return 0, fmt.Errorf("%w: default user name is required", ErrInvalid)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: seed default tokens begin: %w", err)
	}
	// defer Rollback:Commit 成功时 Rollback 返 sql.ErrTxDone,无害。
	defer func() {
		_ = tx.Rollback()
	}()

	// 1) 建/复用 default user(INSERT OR IGNORE + SELECT id,不依赖 UserInsert 的 db.Exec)
	var uid int64
	if _, err := tx.Exec(`INSERT OR IGNORE INTO users (name, status, quota, used_quota, created_at, updated_at)
	                      VALUES (?, 1, 0, 0, datetime('now'), datetime('now'))`, name); err != nil {
		return 0, fmt.Errorf("store: seed default user insert: %w", err)
	}
	if err := tx.QueryRow(`SELECT id FROM users WHERE name = ?`, name).Scan(&uid); err != nil {
		return 0, fmt.Errorf("store: seed default user select: %w", err)
	}

	// 2) 逐个插 token;ErrDuplicate 跳过,其它整体回滚
	seeded := 0
	for _, key := range authKeys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		t := Token{
			UserID:    uid,
			Name:      "seeded",
			KeyHash:   HashKey(key),
			KeyPrefix: KeyPrefix(key),
			Unlimited: true, // 3a 不扣费,seed 出来全 unlimited
			Status:    1,
		}
		if _, err := insertToken(tx, t); err != nil {
			if errors.Is(err, ErrDuplicate) {
				continue
			}
			return seeded, fmt.Errorf("store: seed token (prefix=%s): %w", t.KeyPrefix, err)
		}
		seeded++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: seed default tokens commit: %w", err)
	}
	return seeded, nil
}

func scanToken(r scanner) (Token, error) {
	var t Token
	var unlimited int
	// LimitQuota 是 *int64:扫描 **int64 时 database/sql 会把 NULL 置为 nil
	// (与 usage.channel_id 同一套 nullable 处理)。
	err := r.Scan(&t.ID, &t.UserID, &t.Name, &t.KeyHash, &t.KeyPrefix,
		&t.RemainQuota, &t.UsedQuota, &t.LimitQuota, &unlimited, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return t, err
	}
	t.Unlimited = unlimited != 0
	return t, nil
}

// ---- 阶段 3b:额度原子原语 ------------------------------------------

// TokenDecreaseQuota 原子预扣:仅当 remain_quota >= amount 时扣减,返回是否成功。
//
// SQL 条件与 SET 在同一语句内完成 → 由 SQLite 保证"检查+扣减"不可分割,
// 无需显式事务;RowsAffected=0 即额度不足。
//
// **本方法只服务非 unlimited token**:billing.PreConsume 在 unlimited 时短路、
// 根本不调用这里(见 billing 包注释)。这样 SQL 里不必再写 `unlimited=1` 分支,
// 避免"unlimited 走了 WHERE 命中但 SET 依然扣减"的坑。
//
// amount < 0 → ErrInvalid;amount == 0 → 恒真(无副作用,供调用方统一路径)。
func (s *Store) TokenDecreaseQuota(id, amount int64) (bool, error) {
	if amount < 0 {
		return false, fmt.Errorf("%w: amount must be >= 0", ErrInvalid)
	}
	if amount == 0 {
		// 无额度可扣:确认 token 存在即可,避免"0 预扣"把不存在 id 判成成功
		if _, err := s.TokenGet(id); err != nil {
			return false, err
		}
		return true, nil
	}
	res, err := s.db.Exec(`UPDATE tokens SET remain_quota = remain_quota - ?, updated_at = datetime('now')
	                       WHERE id = ? AND remain_quota >= ?`, amount, id, amount)
	if err != nil {
		return false, fmt.Errorf("store: token decrease quota id=%d: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// TokenIncreaseQuota 退还/充值:remain_quota += amount(amount >= 0)。
// 幂等性由调用方保证(billing 的退款走 UsageRefund 的 tx 内判重)。
func (s *Store) TokenIncreaseQuota(id, amount int64) error {
	if amount < 0 {
		return fmt.Errorf("%w: amount must be >= 0", ErrInvalid)
	}
	if _, err := s.db.Exec(`UPDATE tokens SET remain_quota = remain_quota + ?, updated_at = datetime('now') WHERE id = ?`,
		amount, id); err != nil {
		return fmt.Errorf("store: token increase quota id=%d: %w", id, err)
	}
	return nil
}

// TokenGet 按 id 查;未命中 → ErrNotFound。
func (s *Store) TokenGet(id int64) (*Token, error) {
	row := s.db.QueryRow(`SELECT `+tokenColumns+`
	                     FROM tokens WHERE id = ?`, id)
	t, err := scanToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: token get id=%d: %w", id, err)
	}
	return &t, nil
}

// TokenListByUser 列某 user 名下全部 token(含 disabled),ORDER BY id ASC。
// 决策 #1(GET /users/{id}/tokens):不返回明文 key,只返回 key_prefix。
func (s *Store) TokenListByUser(userID int64) ([]Token, error) {
	rows, err := s.db.Query(`SELECT `+tokenColumns+`
	                        FROM tokens WHERE user_id = ? ORDER BY id ASC`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: token list by user id=%d: %w", userID, err)
	}
	defer rows.Close()
	out := make([]Token, 0, 8)
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, fmt.Errorf("store: token list scan: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: token list rows: %w", err)
	}
	return out, nil
}

// OptInt64 是三态整数,用于 PATCH 的「字段未出现 / 显式 null / 具体值」语义(决策 X6)。
//
//   - 字段未出现        → Set=false                 → 不改
//   - 字段出现且为 null → Set=true, Value=nil       → 清为"不限"
//   - 字段出现且有整数  → Set=true, Value=&n        → 设为 n
//
// 不能直接用 *int64:Go 的 encoding/json 无法把"字段缺失"与"显式 null"区分开。
type OptInt64 struct {
	Set   bool
	Value *int64
}

// UnmarshalJSON 记录字段是否出现,并区分 null 与具体值。
func (o *OptInt64) UnmarshalJSON(b []byte) error {
	o.Set = true
	if strings.TrimSpace(string(b)) == "null" {
		o.Value = nil
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("value must be an integer or null: %w", err)
	}
	o.Value = &n
	return nil
}

// TokenUpdatePatch 是 token 部分更新的入参(指针语义:nil=不改)。
//
// STAGE4 变更:原 `Quota *int64`(直接写 remain_quota)已**移除** ——
// remain_quota 冻结为只读历史值(决策 C);改用 LimitQuota 表达可选准入限额。
type TokenUpdatePatch struct {
	Name       *string
	Unlimited  *bool
	Status     *int
	LimitQuota OptInt64
}

// TokenUpdate 按字段部分更新;未命中 → ErrNotFound;status ∉ {1,2} → ErrInvalid。
// 空 patch 合法(noop),但仍确认 id 存在。
func (s *Store) TokenUpdate(id int64, p TokenUpdatePatch) error {
	if p.Status != nil && *p.Status != 1 && *p.Status != 2 {
		return fmt.Errorf("%w: status must be 1 (enabled) or 2 (disabled)", ErrInvalid)
	}
	if p.LimitQuota.Set && p.LimitQuota.Value != nil && *p.LimitQuota.Value < 0 {
		return fmt.Errorf("%w: limit_quota must be >= 0", ErrInvalid)
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
	if p.Unlimited != nil {
		sets = append(sets, "unlimited=?")
		args = append(args, boolToInt(*p.Unlimited))
	}
	if p.Status != nil {
		sets = append(sets, "status=?")
		args = append(args, *p.Status)
	}
	if p.LimitQuota.Set {
		if p.LimitQuota.Value == nil {
			// 显式 null:清为"不限"
			sets = append(sets, "limit_quota=NULL")
		} else {
			sets = append(sets, "limit_quota=?")
			args = append(args, *p.LimitQuota.Value)
		}
	}
	if len(sets) == 0 {
		if _, err := s.TokenGet(id); err != nil {
			return err
		}
		return nil
	}
	sets = append(sets, "updated_at=datetime('now')")
	args = append(args, id)
	if _, err := s.db.Exec(`UPDATE tokens SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		return fmt.Errorf("store: token update id=%d: %w", id, err)
	}
	// 用 TokenGet 兜底判存在:SQLite 对"命中但值未变"的 UPDATE RowsAffected=1,
	// 但 noop patch 走的是上面的空分支,这里仍以查库为准,语义最稳。
	if _, err := s.TokenGet(id); err != nil {
		return err
	}
	return nil
}

// TokenDelete 物理删 token(决策 #5:usage 流水保留,不级联删)。
// 未命中 → ErrNotFound。
func (s *Store) TokenDelete(id int64) error {
	res, err := s.db.Exec(`DELETE FROM tokens WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: token delete id=%d: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GenerateTokenKey 生成 32 字节随机 key 的 hex 形式(64 字符)。
// 决策 #1(§6):发令牌的明文 key 由服务端随机生成,调用方不自传。
// 用 crypto/rand,失败直接返 error(不降级,避免弱 key)。
func GenerateTokenKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store: generate token key: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// TokenCreateWithKey 服务端生成明文 key 并入库,返回 (tokenID, 明文key)。
// **明文 key 只在此刻返回一次**,之后任何查询只有 key_hash / key_prefix(决策 #1)。
//
// STAGE4:limitQuota 是可选**准入限额**(nil = 不限额度,只受 users.balance 约束)。
// 不再接受初始 remain_quota —— 该列已冻结(决策 C / D11 / D18)。
func (s *Store) TokenCreateWithKey(userID int64, name string, unlimited bool, limitQuota *int64) (int64, string, error) {
	if limitQuota != nil && *limitQuota < 0 {
		return 0, "", fmt.Errorf("%w: limit_quota must be >= 0", ErrInvalid)
	}
	key, err := GenerateTokenKey()
	if err != nil {
		return 0, "", err
	}
	id, err := s.TokenInsert(Token{
		UserID:     userID,
		Name:       name,
		KeyHash:    HashKey(key),
		KeyPrefix:  KeyPrefix(key),
		LimitQuota: limitQuota,
		Unlimited:  unlimited,
		Status:     1,
	})
	if err != nil {
		return 0, "", err
	}
	return id, key, nil
}

// ---- STAGE4:自助接口的归属校验原语 ----------------------------------

// TokenGetOwned 按 id + user_id 取 token;不属于该 user 或不存在 → ErrNotFound。
//
// 刻意返回 ErrNotFound 而不是 403:不泄露"该 id 存在但不属于你"(避免枚举他人 id)。
func (s *Store) TokenGetOwned(id, userID int64) (*Token, error) {
	row := s.db.QueryRow(`SELECT `+tokenColumns+`
	                     FROM tokens WHERE id = ? AND user_id = ?`, id, userID)
	t, err := scanToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: token get owned id=%d user=%d: %w", id, userID, err)
	}
	return &t, nil
}

// TokenUpdateOwned 是带归属约束的部分更新(自助接口用)。
// WHERE 同时限定 id 与 user_id,越权更新不会命中 → ErrNotFound。
func (s *Store) TokenUpdateOwned(id, userID int64, p TokenUpdatePatch) error {
	if p.Status != nil && *p.Status != 1 && *p.Status != 2 {
		return fmt.Errorf("%w: status must be 1 (enabled) or 2 (disabled)", ErrInvalid)
	}
	if p.LimitQuota.Set && p.LimitQuota.Value != nil && *p.LimitQuota.Value < 0 {
		return fmt.Errorf("%w: limit_quota must be >= 0", ErrInvalid)
	}

	sets := make([]string, 0, 5)
	args := make([]any, 0, 7)
	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" {
			return fmt.Errorf("%w: name is required", ErrInvalid)
		}
		sets = append(sets, "name=?")
		args = append(args, name)
	}
	if p.Unlimited != nil {
		sets = append(sets, "unlimited=?")
		args = append(args, boolToInt(*p.Unlimited))
	}
	if p.Status != nil {
		sets = append(sets, "status=?")
		args = append(args, *p.Status)
	}
	if p.LimitQuota.Set {
		if p.LimitQuota.Value == nil {
			sets = append(sets, "limit_quota=NULL")
		} else {
			sets = append(sets, "limit_quota=?")
			args = append(args, *p.LimitQuota.Value)
		}
	}
	if len(sets) == 0 {
		if _, err := s.TokenGetOwned(id, userID); err != nil {
			return err
		}
		return nil
	}
	sets = append(sets, "updated_at=datetime('now')")
	args = append(args, id, userID)
	res, err := s.db.Exec(`UPDATE tokens SET `+strings.Join(sets, ", ")+` WHERE id = ? AND user_id = ?`, args...)
	if err != nil {
		return fmt.Errorf("store: token update owned id=%d user=%d: %w", id, userID, err)
	}
	// 归属校验兜底:越权(命中 0 行且非"值未变")时判 ErrNotFound。
	if _, err := s.TokenGetOwned(id, userID); err != nil {
		return err
	}
	_ = res
	return nil
}

// TokenDeleteOwned 删除属于该 user 的 token;不属于或不存在 → ErrNotFound。
func (s *Store) TokenDeleteOwned(id, userID int64) error {
	res, err := s.db.Exec(`DELETE FROM tokens WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("store: token delete owned id=%d user=%d: %w", id, userID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
