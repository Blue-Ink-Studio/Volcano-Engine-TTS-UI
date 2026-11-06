package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
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
	ID int64 `json:"id"`
	// ChannelID 是音色归属的渠道(STAGE6)。NULL 仅出现在历史兜底行;
	// 新写入一律必填(见 VoiceInsert)。
	//
	// 用 *int64 而非 int64:DB 列可空,用指针能区分"没有归属"和"归属 id=0"。
	ChannelID *int64 `json:"channel_id,omitempty"`
	// ChannelName 只在列表/详情查询时 JOIN 出来做展示,不参与写入。
	ChannelName string `json:"channel_name,omitempty"`
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

// ErrDuplicate 表示 (channel_id, name) 唯一冲突;controller 翻译为 409 voice_duplicate。
var ErrDuplicate = errors.New("store: voice name already exists in this channel")

// ErrInUse 已**废弃**(STAGE6):全局 settings.default_speaker 被移除后,
// 不再存在"音色被默认音色引用而拒绝删除"的场景。
//
// 渠道级 default_voice 的悬空问题改为**自动清理**(删除音色时把该渠道的
// default_voice 置空并打 WARN),不阻断管理操作。
//
// 符号保留只是为了让 externals 的 errors.Is 判断继续编译;STAGE6 之后不再返回。
var ErrInUse = errors.New("store: voice is referenced by default_speaker (deprecated)")

// ErrNotFound 表示按 id/name 找不到;controller 翻译为 404。
var ErrNotFound = errors.New("store: voice not found")

// ErrInvalid 表示客户端输入不合法(name 格式 / 必填字段缺失);
// controller 用 errors.Is(err, ErrInvalid) 翻译为 400。
// 服务端错误(DB 失败等)不会被 wrap,controller 应翻译为 500。
var ErrInvalid = errors.New("store: voice invalid")

// voiceNameRe 限制 voice 名为 [a-zA-Z0-9_-]{1,64};SQL 注入 + 路径穿越防护。
var voiceNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// voiceColumns 是 voices 的读取列(带渠道名 JOIN 供展示)。
const voiceColumns = `v.id, v.channel_id, COALESCE(c.name,''), v.name, v.speaker, v.resource_id,
	v.model, v.language, v.description, v.enabled, v.created_at, v.updated_at`

// VoiceList 列出所有音色;includeDisabled=false 时只返回 enabled=1。
// 按 id 升序,稳定顺序便于前端展示。
//
// STAGE6:附带渠道名(LEFT JOIN),便于管理端按渠道分组展示。
func (s *Store) VoiceList(includeDisabled bool) ([]Voice, error) {
	q := `SELECT ` + voiceColumns + ` FROM voices v LEFT JOIN channels c ON c.id = v.channel_id`
	if !includeDisabled {
		q += ` WHERE v.enabled = 1`
	}
	q += ` ORDER BY v.id ASC`

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
	row := s.db.QueryRow(`SELECT `+voiceColumns+`
	                     FROM voices v LEFT JOIN channels c ON c.id = v.channel_id
	                     WHERE v.id = ?`, id)
	v, err := scanVoice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: voice get id=%d: %w", id, err)
	}
	return &v, nil
}

// VoiceResolve 在指定渠道内按 name 查音色(STAGE6 路由主入口)。
//
// 「渠道即模型」:请求 model=渠道名&voice=音色名 → 先按 name 命中渠道,
// 再用这个函数在该渠道内解析音色。**同名音色可以在不同渠道各存在一份**,
// 所以必须带 channelID,不能再用全局唯一的 VoiceGetByName。
//
// 未命中返回 ErrNotFound —— 调用方据此返回 400 unknown_voice。
func (s *Store) VoiceResolve(channelID int64, name string) (*Voice, error) {
	row := s.db.QueryRow(`SELECT `+voiceColumns+`
	                     FROM voices v LEFT JOIN channels c ON c.id = v.channel_id
	                     WHERE v.channel_id = ? AND v.name = ?`, channelID, strings.TrimSpace(name))
	v, err := scanVoice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: voice resolve channel=%d name=%q: %w", channelID, name, err)
	}
	return &v, nil
}

// VoiceGetByName 按 name 查;未命中返回 ErrNotFound。
//
// ⚠️ STAGE6 起 name **不再全局唯一**(同名可属不同渠道),本函数因此语义模糊:
// 它返回**任意一条**命中行。业务路由请用 VoiceResolve(channelID, name)。
//
// 保留它只为兼容尚未改造的调用方与测试;新代码不应使用。
func (s *Store) VoiceGetByName(name string) (*Voice, error) {
	row := s.db.QueryRow(`SELECT `+voiceColumns+`
	                     FROM voices v LEFT JOIN channels c ON c.id = v.channel_id
	                     WHERE v.name = ? LIMIT 1`, name)
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
// 客户端输入错误(name 格式 / 必填字段缺失)返回 wrap ErrInvalid 的 error;
// 服务端错误(DB 失败等)不被 wrap,controller 用 errors.Is 区分。
// 不依赖 SQLite 约束作为唯一校验。
func (s *Store) VoiceInsert(v Voice) (int64, error) {
	v.Name = strings.TrimSpace(v.Name)
	v.Speaker = strings.TrimSpace(v.Speaker)
	v.ResourceID = strings.TrimSpace(v.ResourceID)
	v.Model = strings.TrimSpace(v.Model)
	v.Language = strings.TrimSpace(v.Language)
	v.Description = strings.TrimSpace(v.Description)

	if err := validateVoiceName(v.Name); err != nil {
		// validateVoiceName 返纯文本;这里 wrap 进 ErrInvalid 让 controller 用 errors.Is 判定。
		return 0, fmt.Errorf("%w: %s", ErrInvalid, err.Error())
	}
	// STAGE6:音色必须归属渠道(G5)。channel_id<=0 → ErrInvalid(controller 翻 400 invalid_channel)。
	if v.ChannelID == nil || *v.ChannelID <= 0 {
		return 0, fmt.Errorf("%w: channel_id is required", ErrInvalid)
	}
	if _, err := s.ChannelGet(*v.ChannelID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return 0, fmt.Errorf("%w: channel_id %d does not exist", ErrInvalid, *v.ChannelID)
		}
		return 0, fmt.Errorf("store: voice insert check channel: %w", err)
	}
	if v.Speaker == "" {
		return 0, fmt.Errorf("%w: speaker is required", ErrInvalid)
	}
	// STAGE6(A4):resource_id 允许为空 —— OpenAI/Azure 渠道没有这个维度。
	// 旧版这里强制非空,会把非火山渠道逼成"塞个假 resource_id"。

	res, err := s.db.Exec(`
		INSERT INTO voices (channel_id, name, speaker, resource_id, model, language, description, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))`,
		*v.ChannelID, v.Name, v.Speaker, v.ResourceID, v.Model, v.Language, v.Description, boolToInt(v.Enabled))
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

// VoiceUpdate 整行替换(含归属渠道);渠道内 name 仍需唯一。
// 不允许把 name 改成空/不合法;不允许 channel_id 为空。
//
// STAGE6 的两处连带维护(G4)—— 渠道级默认音色 channels.default_voice:
//   - **改名**:若渠道的 default_voice 指向旧名 → 同事务改成新名
//   - **换渠道**:若旧渠道的 default_voice 指向它 → 同事务清空(它已不在该渠道)
//
// 为什么必须同事务:否则"音色改名了、默认音色还指着旧名"会变成一个悬空引用,
// 请求不带 voice 时直接 400 voice_required,而管理员完全看不出原因。
//
// 这对应 STAGE5 之前那段"同步 settings.default_speaker"的逻辑 —— 目标表从
// 全局 settings 换成了渠道级 channels.default_voice。
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
	if v.ChannelID == nil || *v.ChannelID <= 0 {
		return fmt.Errorf("%w: channel_id is required", ErrInvalid)
	}
	if v.Speaker == "" {
		return fmt.Errorf("%w: speaker is required", ErrInvalid)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: voice update begin: %w", err)
	}
	defer func() {
		// commit 成功时 Rollback 返回 sql.ErrTxDone,无害
		_ = tx.Rollback()
	}()

	// 1. 读旧值(同事务,避免并发改)
	var oldName string
	var oldChannel sql.NullInt64
	if err := tx.QueryRow(`SELECT name, channel_id FROM voices WHERE id = ?`, v.ID).
		Scan(&oldName, &oldChannel); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("store: voice update read old id=%d: %w", v.ID, err)
	}

	// 2. 维护 channels.default_voice(G4)
	if oldChannel.Valid {
		oldChID := oldChannel.Int64
		if oldChID == *v.ChannelID {
			// 同渠道改名:default_voice 跟着改
			if oldName != v.Name {
				if _, err := tx.Exec(`
					UPDATE channels SET default_voice = ?, updated_at = datetime('now')
					WHERE id = ? AND default_voice = ?`,
					v.Name, oldChID, oldName); err != nil {
					return fmt.Errorf("store: voice update sync default_voice: %w", err)
				}
			}
		} else {
			// 换渠道:旧渠道的默认音色指向它就清空(它已不属于旧渠道)
			if _, err := tx.Exec(`
				UPDATE channels SET default_voice = '', updated_at = datetime('now')
				WHERE id = ? AND default_voice = ?`,
				oldChID, oldName); err != nil {
				return fmt.Errorf("store: voice update clear old default_voice: %w", err)
			}
		}
	}

	// 3. UPDATE voice
	res, err := tx.Exec(`
		UPDATE voices SET channel_id=?, name=?, speaker=?, resource_id=?, model=?, language=?, description=?, enabled=?, updated_at=datetime('now')
		WHERE id = ?`,
		*v.ChannelID, v.Name, v.Speaker, v.ResourceID, v.Model, v.Language, v.Description,
		boolToInt(v.Enabled), v.ID)
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

	// 4. 提交
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: voice update commit: %w", err)
	}
	return nil
}

// VoiceDelete 按 id 删。
//
// STAGE6(G4):若该音色正是所属渠道的 default_voice,同事务**清空**它并打 WARN。
// 不再返回 ErrInUse —— 全局 settings.default_speaker 已废弃,不存在"被默认值引用
// 就拒绝删除"的场景;阻断管理操作换来的只是管理员无处释放的 409。
//
// 用事务而非两条独立语句:否则"删了音色但 default_voice 还指着它"会留下悬空引用。
func (s *Store) VoiceDelete(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: voice delete begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var name string
	var channelID sql.NullInt64
	if err := tx.QueryRow(`SELECT name, channel_id FROM voices WHERE id = ?`, id).
		Scan(&name, &channelID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("store: voice delete read id=%d: %w", id, err)
	}

	if channelID.Valid {
		res, err := tx.Exec(`
			UPDATE channels SET default_voice = '', updated_at = datetime('now')
			WHERE id = ? AND default_voice = ?`, channelID.Int64, name)
		if err != nil {
			return fmt.Errorf("store: voice delete clear default_voice: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("[store] WARN: 删除音色 %q 时同步清空了渠道 %d 的 default_voice"+
				"(该渠道后续请求必须自带 voice)", name, channelID.Int64)
		}
	}

	if _, err := tx.Exec(`DELETE FROM voices WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: voice delete id=%d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: voice delete commit: %w", err)
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
	var channelID sql.NullInt64
	var enabled int
	err := r.Scan(&v.ID, &channelID, &v.ChannelName, &v.Name, &v.Speaker, &v.ResourceID,
		&v.Model, &v.Language, &v.Description, &enabled, &v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		return v, err
	}
	if channelID.Valid {
		id := channelID.Int64
		v.ChannelID = &id
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
