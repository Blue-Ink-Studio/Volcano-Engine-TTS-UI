package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Channel 是一行渠道记录:
//   - 一个上游账号(Provider=adapter 名 + 该账号的凭据)
//   - 可限定支持的对外音色(Voices);空 = 不限(接受一切)
//   - Priority 分档(大者优先),Weight 同档加权
//
// JSON tag 跟前台/admin.html 直接读 Go 字段对齐(参见 store/voices.go 顶部说明)。
type Channel struct {
	ID       int64             `json:"id"`
	Name     string            `json:"name"`
	Provider string            `json:"provider"`
	// CredentialsJSON 是序列化后的 provider.Credentials:
	//   {"api_key": "...", "scope": {"resource_id": "...", ...}}
	// 我们落 JSON 列而非拆字段,因为不同 provider 需要的 scope 维度差异很大
	// (火山要 resource_id,Azure 要 region,自建要 base_url ...),
	// 拆字段会被某一家绑死。读时反序列化为 provider.Credentials。
	CredentialsJSON string `json:"credentials_json"`
	Voices         string `json:"voices"` // 逗号分隔的对外 voice 名;空 = 不限
	Priority       int    `json:"priority"`
	Weight         int    `json:"weight"`
	// Status: 1 = enabled, 2 = disabled(对齐 voices.enabled 的 0/1 风格,
	// 但本阶段管理接口未上线,只用 int 留扩展位,后续可加 "维护中" 之类状态)。
	Status    int    `json:"status"`
	AutoBan   bool   `json:"auto_ban"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ChannelList 列出所有渠道(按 priority DESC, id ASC),includeDisabled=false 时只返回 enabled。
// 排序规则固定,便于 router 启动期一次性加载后做 priority 分桶。
func (s *Store) ChannelList(includeDisabled bool) ([]Channel, error) {
	q := `SELECT id, name, provider, credentials_json, voices, priority, weight, status, auto_ban, created_at, updated_at
	      FROM channels`
	if !includeDisabled {
		q += ` WHERE status = 1`
	}
	q += ` ORDER BY priority DESC, id ASC`

	rows, err := s.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("store: channel list: %w", err)
	}
	defer rows.Close()

	out := make([]Channel, 0, 4)
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: channel list rows: %w", err)
	}
	return out, nil
}

// ChannelGet 按 id 查;未命中返回 ErrNotFound。
func (s *Store) ChannelGet(id int64) (*Channel, error) {
	row := s.db.QueryRow(`SELECT id, name, provider, credentials_json, voices, priority, weight, status, auto_ban, created_at, updated_at
	                     FROM channels WHERE id = ?`, id)
	c, err := scanChannel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: channel get id=%d: %w", id, err)
	}
	return &c, nil
}

// ChannelInsert 新增一条渠道;name 冲突返回 ErrDuplicate。
// 输入校验错误(必填项缺失)返回 wrap ErrInvalid;服务端错误(DB 失败等)不被 wrap。
func (s *Store) ChannelInsert(c Channel) (int64, error) {
	c.Name = strings.TrimSpace(c.Name)
	c.Provider = strings.TrimSpace(c.Provider)
	c.CredentialsJSON = strings.TrimSpace(c.CredentialsJSON)
	c.Voices = normalizeVoicesCSV(c.Voices)

	if c.Name == "" {
		return 0, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	if c.Provider == "" {
		return 0, fmt.Errorf("%w: provider is required", ErrInvalid)
	}
	if c.CredentialsJSON == "" {
		return 0, fmt.Errorf("%w: credentials_json is required", ErrInvalid)
	}
	// credentials_json 必须是合法 JSON;否则后续 router 加载会崩,这里挡掉。
	if !json.Valid([]byte(c.CredentialsJSON)) {
		return 0, fmt.Errorf("%w: credentials_json is not valid JSON", ErrInvalid)
	}
	if c.Weight <= 0 {
		c.Weight = 1 // 默认权重 1
	}
	if c.Status != 1 && c.Status != 2 {
		c.Status = 1 // 默认启用
	}

	res, err := s.db.Exec(`
		INSERT INTO channels (name, provider, credentials_json, voices, priority, weight, status, auto_ban, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))`,
		c.Name, c.Provider, c.CredentialsJSON, c.Voices, c.Priority, c.Weight, c.Status, boolToInt(c.AutoBan))
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrChannelDuplicate
		}
		return 0, fmt.Errorf("store: channel insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: channel insert lastid: %w", err)
	}
	return id, nil
}

// ChannelSetStatus 改状态(1=enabled, 2=disabled);未命中返回 ErrNotFound。
// 阶段 1 没有 admin 接口,但 router 启动期之后可被 auto_ban 流程调用,
// 所以这个方法必须先有,管理接口只是包装。
func (s *Store) ChannelSetStatus(id int64, status int) error {
	if status != 1 && status != 2 {
		return fmt.Errorf("%w: status must be 1 (enabled) or 2 (disabled)", ErrInvalid)
	}
	res, err := s.db.Exec(`UPDATE channels SET status=?, updated_at=datetime('now') WHERE id = ?`, status, id)
	if err != nil {
		return fmt.Errorf("store: channel set status id=%d: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ChannelUpdate 部分更新渠道;所有字段都是 *T 指针,nil=不改,非 nil=改。
//   - 校验:credentials_json(若改)必须是合法 JSON;weight(若改)必须 > 0;
//     status(若改)必须 1 或 2
//   - name(若改)由 UNIQUE 约束兜底 → ErrChannelDuplicate → 409
//   - "自己改自己" (UPDATE 同一行 name 为原值) SQLite UNIQUE 行间比较不触发 → 200
//   - SQLite 对"命中行但值未变" 的 UPDATE RowsAffected=1(不是 0),所以 affected_rows=0
//     才是真 404,no-op UPDATE 不会误判
//
// 阶段 2 决策:#3 改 name 重名 409, #5 写操作前校验, #10 name 允许改
type ChannelUpdate struct {
	Name            *string
	CredentialsJSON *string
	Voices          *string
	Priority        *int
	Weight          *int
	Status          *int
	AutoBan         *bool
}

// ChannelUpdate 按字段部分更新;未命中 → ErrNotFound;name 重名 → ErrChannelDuplicate;
// credentials_json 不合法 / weight ≤ 0 / status ∉ {1,2} → wrap ErrInvalid。
// 空 patch(全部 nil)是合法的 noop → 返回 nil,rows_affected=0 但 id 存在也返 nil
// (不报错,等效于"读+写回,值不变")。
func (s *Store) ChannelUpdate(id int64, p ChannelUpdate) error {
	// 1) 校验入参
	if p.CredentialsJSON != nil {
		*p.CredentialsJSON = strings.TrimSpace(*p.CredentialsJSON)
		if !json.Valid([]byte(*p.CredentialsJSON)) {
			return fmt.Errorf("%w: credentials_json is not valid JSON", ErrInvalid)
		}
	}
	if p.Weight != nil && *p.Weight <= 0 {
		return fmt.Errorf("%w: weight must be > 0", ErrInvalid)
	}
	if p.Status != nil && *p.Status != 1 && *p.Status != 2 {
		return fmt.Errorf("%w: status must be 1 (enabled) or 2 (disabled)", ErrInvalid)
	}
	if p.Voices != nil {
		v := normalizeVoicesCSV(*p.Voices)
		p.Voices = &v
	}

	// 2) 动态构造 UPDATE —— 避免拼 SQL 字符串,改用 slice 占位符
	//    (空 patch 时这里跳过 SQL,直接 200 noop)
	sets := make([]string, 0, 7)
	args := make([]any, 0, 8)
	if p.Name != nil {
		sets = append(sets, "name=?")
		args = append(args, strings.TrimSpace(*p.Name))
	}
	if p.CredentialsJSON != nil {
		sets = append(sets, "credentials_json=?")
		args = append(args, *p.CredentialsJSON)
	}
	if p.Voices != nil {
		sets = append(sets, "voices=?")
		args = append(args, *p.Voices)
	}
	if p.Priority != nil {
		sets = append(sets, "priority=?")
		args = append(args, *p.Priority)
	}
	if p.Weight != nil {
		sets = append(sets, "weight=?")
		args = append(args, *p.Weight)
	}
	if p.Status != nil {
		sets = append(sets, "status=?")
		args = append(args, *p.Status)
	}
	if p.AutoBan != nil {
		sets = append(sets, "auto_ban=?")
		args = append(args, boolToInt(*p.AutoBan))
	}
	if len(sets) == 0 {
		// 空 patch:合法 noop;但仍要确认 id 存在(否则 404)
		// 用一次 SELECT 替代 UPDATE,免去 0 行 vs 1 行的歧义
		var n int
		if err := s.db.QueryRow(`SELECT 1 FROM channels WHERE id = ?`, id).Scan(&n); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("store: channel update noop check id=%d: %w", id, err)
		}
		return nil
	}
	sets = append(sets, "updated_at=datetime('now')")
	args = append(args, id)

	q := "UPDATE channels SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	res, err := s.db.Exec(q, args...)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrChannelDuplicate
		}
		return fmt.Errorf("store: channel update id=%d: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ChannelDelete 按 id 删;未命中返回 ErrNotFound。
func (s *Store) ChannelDelete(id int64) error {
	res, err := s.db.Exec(`DELETE FROM channels WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: channel delete id=%d: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ChannelCount 统计行数;阶段 1 暂未使用,保留为未来 admin 仪表盘用。
func (s *Store) ChannelCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM channels`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: channel count: %w", err)
	}
	return n, nil
}

// ErrChannelDuplicate 表示 channel name 唯一冲突;与 voices.ErrDuplicate 同名但消息不同,
// 避免 channel 重名被翻译成"voice name already exists"。
var ErrChannelDuplicate = errors.New("store: channel name already exists")

// scanChannel 把 row 扫描成 Channel;接受 *sql.Row 或 *sql.Rows(都实现 Scan)。
func scanChannel(r scanner) (Channel, error) {
	var c Channel
	var autoBan int
	err := r.Scan(&c.ID, &c.Name, &c.Provider, &c.CredentialsJSON, &c.Voices,
		&c.Priority, &c.Weight, &c.Status, &autoBan, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return c, err
	}
	c.AutoBan = autoBan != 0
	return c, nil
}

// normalizeVoicesCSV 把 "a,,b, a ,c" 规整成 "a,b,c" —— 重复项去重,
// 顺序保留(便于 hash 比较和稳定展示);空字符串返 ""(语义=不限)。
func normalizeVoicesCSV(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, ",")
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return strings.Join(out, ",")
}
