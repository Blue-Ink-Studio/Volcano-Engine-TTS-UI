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
