package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Voice 是一行音色记录;时间字段保持 ISO8601 字符串(SQLite TEXT 默认)。
//
// JSON tag 是为了前端(admin.html)能直接读取 — 之前没加 tag 时 Go 的
// "Name" / "Speaker" 等大写字段会原样输出,前端用 v.name 拿到 undefined,
// 整张表看起来"空"但其实有数据。补 tag 后前端能正常显示。
type Voice struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Speaker     string `json:"speaker"`
	ResourceID  string `json:"resource_id"`
	Model       string `json:"model"`
	Language    string `json:"language"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// ErrDuplicate 表示 name 唯一冲突;controller 翻译为 409。
var ErrDuplicate = errors.New("store: voice name already exists")

// ErrInUse 表示试图删除被 default_speaker 引用的音色;controller 翻译为 409。
var ErrInUse = errors.New("store: voice is referenced by default_speaker")

// ErrNotFound 表示按 id/name 找不到;controller 翻译为 404。
var ErrNotFound = errors.New("store: voice not found")

// voiceNameRe 限制 voice 名为 [a-zA-Z0-9_-]{1,64};SQL 注入 + 路径穿越防护。
var voiceNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// VoiceList 列出所有音色;includeDisabled=false 时只返回 enabled=1。
// 按 id 升序,稳定顺序便于前端展示。
func (s *Store) VoiceList(includeDisabled bool) ([]Voice, error) {
	q := `SELECT id, name, speaker, resource_id, model, language, description, enabled, created_at, updated_at
	      FROM voices`
	if !includeDisabled {
		q += ` WHERE enabled = 1`
	}
	q += ` ORDER BY id ASC`

	rows, err := s.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("store: voice list: %w", err)
	}
	defer rows.Close()

	out := make([]Voice, 0, 8)
	for rows.Next() {
		v, err := scanVoice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: voice list rows: %w", err)
	}
	return out, nil
}

// VoiceGet 按 id 查;未命中返回 ErrNotFound。
func (s *Store) VoiceGet(id int64) (*Voice, error) {
	row := s.db.QueryRow(`SELECT id, name, speaker, resource_id, model, language, description, enabled, created_at, updated_at
	                     FROM voices WHERE id = ?`, id)
	v, err := scanVoice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: voice get id=%d: %w", id, err)
	}
	return &v, nil
}

// VoiceGetByName 按 name 查;未命中返回 ErrNotFound。
// tts.go 路由用这个,要求 name 走参数化查询。
func (s *Store) VoiceGetByName(name string) (*Voice, error) {
	row := s.db.QueryRow(`SELECT id, name, speaker, resource_id, model, language, description, enabled, created_at, updated_at
	                     FROM voices WHERE name = ?`, name)
	v, err := scanVoice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: voice getbyname %q: %w", name, err)
	}
	return &v, nil
}

// GetVoiceForTTS 实现 setting.Store 接口,返 voice 行的 TTS 关键字段。
//   - found=false: voice 不存在(ErrNotFound 翻译为 found=false)
//   - err != nil:  真错误(db 失败等)
// 这个方法存在是为了让 *Store 满足 setting.Store 接口,且不引起
// setting → store → setting 循环 import。
func (s *Store) GetVoiceForTTS(name string) (speaker, resourceID, model string, found bool, err error) {
	v, err := s.VoiceGetByName(name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", "", "", false, nil
		}
		return "", "", "", false, err
	}
	return v.Speaker, v.ResourceID, v.Model, true, nil
}

// VoiceInsert 新增音色;name 冲突返回 ErrDuplicate。
// 空字符串/格式不合法返回 error;不依赖 SQLite 约束作为唯一校验。
func (s *Store) VoiceInsert(v Voice) (int64, error) {
	v.Name = strings.TrimSpace(v.Name)
	v.Speaker = strings.TrimSpace(v.Speaker)
	v.ResourceID = strings.TrimSpace(v.ResourceID)
	v.Model = strings.TrimSpace(v.Model)
	v.Language = strings.TrimSpace(v.Language)
	v.Description = strings.TrimSpace(v.Description)

	if err := validateVoiceName(v.Name); err != nil {
		return 0, err
	}
	if v.Speaker == "" {
		return 0, fmt.Errorf("store: voice insert: speaker is required")
	}
	if v.ResourceID == "" {
		return 0, fmt.Errorf("store: voice insert: resource_id is required")
	}

	res, err := s.db.Exec(`
		INSERT INTO voices (name, speaker, resource_id, model, language, description, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))`,
		v.Name, v.Speaker, v.ResourceID, v.Model, v.Language, v.Description, boolToInt(v.Enabled))
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrDuplicate
		}
		return 0, fmt.Errorf("store: voice insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: voice insert lastid: %w", err)
	}
	return id, nil
}

// VoiceUpdate 整行替换;name 仍需保持唯一。
// 不允许把 name 改成空/不合法。
func (s *Store) VoiceUpdate(v Voice) error {
	v.Name = strings.TrimSpace(v.Name)
	v.Speaker = strings.TrimSpace(v.Speaker)
	v.ResourceID = strings.TrimSpace(v.ResourceID)
	v.Model = strings.TrimSpace(v.Model)
	v.Language = strings.TrimSpace(v.Language)
	v.Description = strings.TrimSpace(v.Description)

	if err := validateVoiceName(v.Name); err != nil {
		return err
	}
	if v.Speaker == "" {
		return fmt.Errorf("store: voice update: speaker is required")
	}
	if v.ResourceID == "" {
		return fmt.Errorf("store: voice update: resource_id is required")
	}

	res, err := s.db.Exec(`
		UPDATE voices SET name=?, speaker=?, resource_id=?, model=?, language=?, description=?, enabled=?, updated_at=datetime('now')
		WHERE id = ?`,
		v.Name, v.Speaker, v.ResourceID, v.Model, v.Language, v.Description, boolToInt(v.Enabled), v.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicate
		}
		return fmt.Errorf("store: voice update id=%d: %w", v.ID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// VoiceDelete 按 id 删;若被 settings.default_speaker 引用则返回 ErrInUse。
func (s *Store) VoiceDelete(id int64) error {
	v, err := s.VoiceGet(id)
	if err != nil {
		return err
	}

	// 检查 default_speaker 引用
	defVal, defOK, err := s.SettingsGet("default_speaker")
	if err != nil {
		return err
	}
	if defOK && defVal == v.Name {
		return ErrInUse
	}

	res, err := s.db.Exec(`DELETE FROM voices WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: voice delete id=%d: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// VoiceToggleEnabled 翻转启用状态;返回更新后的值。
func (s *Store) VoiceToggleEnabled(id int64, enabled bool) error {
	res, err := s.db.Exec(`UPDATE voices SET enabled=?, updated_at=datetime('now') WHERE id = ?`,
		boolToInt(enabled), id)
	if err != nil {
		return fmt.Errorf("store: voice toggle id=%d: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// VoiceCount 统计行数;M2 仪表盘用。
func (s *Store) VoiceCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM voices`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: voice count: %w", err)
	}
	return n, nil
}

// VoiceCountEnabled 统计 enabled=1 的行数;仪表盘用。
func (s *Store) VoiceCountEnabled() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM voices WHERE enabled = 1`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: voice count enabled: %w", err)
	}
	return n, nil
}

// scanVoice 把 row 扫描成 Voice;接受 *sql.Row 或 *sql.Rows(都实现 Scan)。
type scanner interface {
	Scan(dest ...any) error
}

func scanVoice(r scanner) (Voice, error) {
	var v Voice
	var enabled int
	err := r.Scan(&v.ID, &v.Name, &v.Speaker, &v.ResourceID, &v.Model, &v.Language, &v.Description, &enabled, &v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		return v, err
	}
	v.Enabled = enabled != 0
	return v, nil
}

func validateVoiceName(name string) error {
	if name == "" {
		return fmt.Errorf("store: voice name is required")
	}
	if !voiceNameRe.MatchString(name) {
		return fmt.Errorf("store: voice name %q invalid (must match [a-zA-Z0-9_-]{1,64})", name)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// isUniqueViolation 判定 SQLite 唯一约束错误。
// modernc.org/sqlite 错误信息中包含 "UNIQUE constraint failed: <table>.<col>";做大小写不敏感包含判定。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint failed")
}

// VoiceInsertedAt 返回当前时间字符串(UTC, RFC3339);留作未来 Voice 构造时使用,
// 暂不导出。
func voiceNow() string { return time.Now().UTC().Format(time.RFC3339) }
