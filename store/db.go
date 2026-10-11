// Package store 负责 SQLite 访问层:打开/建表/迁移、settings/voices CRUD、
// 完整性校验。所有运行时可变配置(全局参数 + 音色库)统一落 SQLite,
// 环境变量仅作引导参数(TTS_DB_PATH 等)。
//
// 设计约束:
//   - 单二进制分发,纯 Go SQLite(modernc.org/sqlite),CGO_ENABLED=0
//   - 单用户自用,SetMaxOpenConns(1) 避免并发写竞争
//   - 所有 SQL 参数化,严禁字符串拼接
//   - schema_version 表保留未来升级钩子
package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // 注册 sqlite driver
)

// schemaVersion 是当前 schema 版本号;每次结构性变更 +1。
// migrate.go 负责在 Open 时按版本号增量应用。
// schemaVersion 是当前 schema 版本号;每次结构性变更 +1。
//
// 版本历史:
//
//	1 → 基础表(settings/voices)
//	2 → channels(阶段 1 多渠道)
//	3 → users/tokens/usage(阶段 3a 识别 + 计量)
//	4 → usage 补 3 列(阶段 3b 计费:pre_consumed/actual_quota/status)
const schemaVersion = 4

// Store 是 SQLite 访问层的统一入口;所有 settings/voices 操作都通过它。
type Store struct {
	db *sql.DB
}

// Open 打开或创建 SQLite 数据库,自动应用建表与迁移。
// path 推荐为绝对路径;空字符串会落到临时目录,不应用于生产。
func Open(path string) (*Store, error) {
	// modernc.org/sqlite 注册名:"sqlite" + "sqlite3" 两种
	// _dsn 参数控制 journal_mode 等;这里先打开,再用 PRAGMA 调整
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %q failed: %w", path, err)
	}

	// 单用户自用场景,避免并发写竞争
	db.SetMaxOpenConns(1)

	// PRAGMA 需要连接,触发一次 Ping 拿连接
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: enable WAL failed: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: enable foreign_keys failed: %w", err)
	}
	if _, err := db.Exec(`PRAGMA synchronous = NORMAL`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: enable synchronous=NORMAL failed: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate failed: %w", err)
	}
	return s, nil
}

// Close 关闭底层连接;调用方应保证只 Close 一次。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DB 返回底层 *sql.DB,仅供 store 包内或集成测试使用;
// 业务代码不应直接拿连接,所有操作走 Store 暴露的方法。
func (s *Store) DB() *sql.DB { return s.db }

// IntegrityCheck 执行 PRAGMA integrity_check;返回 "ok" 即视为库健康。
// installer 包据此判定是否触发损坏回退。
func (s *Store) IntegrityCheck() (string, error) {
	row := s.db.QueryRow(`PRAGMA integrity_check`)
	var result string
	if err := row.Scan(&result); err != nil {
		return "", fmt.Errorf("store: integrity_check scan failed: %w", err)
	}
	return result, nil
}

// Path 返回当前 db 的 SQLite 报告路径(用于日志)。
// 通过 PRAGMA database_list 拿权威值,避免和入参 path 不一致时的混淆。
func (s *Store) Path() (string, error) {
	rows, err := s.db.Query(`PRAGMA database_list`)
	if err != nil {
		return "", fmt.Errorf("store: database_list query failed: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return "", fmt.Errorf("store: database_list returned no rows")
	}
	var seq int
	var name, file string
	if err := rows.Scan(&seq, &name, &file); err != nil {
		return "", fmt.Errorf("store: database_list scan failed: %w", err)
	}
	return file, nil
}

// migrate 应用 schema 迁移。当前 schema_version=1,只做基础建表。
// 未来升级:写 applyMigration(n) 函数,n 为目标版本号。
func (s *Store) migrate() error {
	// schema_version 表记录当前版本
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_version (
			version    INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}

	// settings 表(全局配置)
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS settings (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
		return fmt.Errorf("create settings: %w", err)
	}

	// voices 表(音色库)
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS voices (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			name        TEXT NOT NULL UNIQUE,
			speaker     TEXT NOT NULL,
			resource_id TEXT NOT NULL,
			model       TEXT DEFAULT '',
			language    TEXT DEFAULT '',
			description TEXT DEFAULT '',
			enabled     INTEGER NOT NULL DEFAULT 1,
			created_at  TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
		return fmt.Errorf("create voices: %w", err)
	}
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_voices_name ON voices(name)`); err != nil {
		return fmt.Errorf("create idx_voices_name: %w", err)
	}

	// 当前版本
	if _, err := s.db.Exec(`
		INSERT OR IGNORE INTO schema_version (version) VALUES (?)`, schemaVersion); err != nil {
		return fmt.Errorf("insert schema_version: %w", err)
	}

	// channels 表(多渠道路由,阶段 1 引入)。
	//   - name 唯一:管理侧标识,不允许重名
	//   - credentials_json: 序列化后的 provider.Credentials(见 store/channels.go 注释)
	//   - voices CSV: 限定该渠道支持的对外音色;空 = 不限
	//   - priority/weight: 大者优先 + 同档加权随机(由 adapter/route 实现)
	//   - status: 1=enabled 2=disabled(留扩展位,管理接口放阶段 2 上)
	//   - auto_ban: new-api 风格连续失败自动禁用标记,阶段 1 仅存,未接自动逻辑
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS channels (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			name            TEXT NOT NULL UNIQUE,
			provider        TEXT NOT NULL,
			credentials_json TEXT NOT NULL,
			voices          TEXT NOT NULL DEFAULT '',
			priority        INTEGER NOT NULL DEFAULT 0,
			weight          INTEGER NOT NULL DEFAULT 1,
			status          INTEGER NOT NULL DEFAULT 1,
			auto_ban        INTEGER NOT NULL DEFAULT 0,
			created_at      TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
		return fmt.Errorf("create channels: %w", err)
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_channels_status_priority ON channels(status, priority DESC)`); err != nil {
		return fmt.Errorf("create idx_channels_status_priority: %w", err)
	}

	// ===== 阶段 3a:多用户识别 + 用量计量 =====
	// 决策:3a 建全 users/tokens/usage 表(含 quota 列),但 3a 不实现 quota 校验;
	// 3b 在此基础上叠加"扣减 + 402",不改表结构。

	// users 表(调用方归属)
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			name       TEXT NOT NULL UNIQUE,
			status     INTEGER NOT NULL DEFAULT 1,    -- 1=enabled 2=disabled
			quota      INTEGER NOT NULL DEFAULT 0,    -- (3b 用)3a 只建列不校验
			used_quota INTEGER NOT NULL DEFAULT 0,    -- (3b 用)对账
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
		return fmt.Errorf("create users: %w", err)
	}

	// tokens 表(每个 API key 对应一个 token,归属一个 user)
	//   - key_hash: sha256(key) hex,不落明文
	//   - key_prefix: 明文前 8 字符,对账识别(不参与鉴权)
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS tokens (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id      INTEGER NOT NULL REFERENCES users(id),
			name         TEXT NOT NULL,
			key_hash     TEXT NOT NULL UNIQUE,
			key_prefix   TEXT NOT NULL DEFAULT '',
			remain_quota INTEGER NOT NULL DEFAULT 0,  -- (3b 用)3a 只建列不校验
			used_quota   INTEGER NOT NULL DEFAULT 0,
			unlimited    INTEGER NOT NULL DEFAULT 0,  -- 1=不限(测试/seed)
			status       INTEGER NOT NULL DEFAULT 1,  -- 1=enabled 2=disabled
			created_at   TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
		return fmt.Errorf("create tokens: %w", err)
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_tokens_user ON tokens(user_id)`); err != nil {
		return fmt.Errorf("create idx_tokens_user: %w", err)
	}

	// usage 表(计量流水,只增不改)
	//   - channel_id 可空(走兜底时无 channel)
	//   - request_id: Synthesize 的 ReqID,便于对账
	//   - characters: input rune 数(不是字节,贴合火山按字符计费)
	//   - pre_consumed/actual_quota/status: 阶段 3b 计费列(老库由下方 ensureColumn 补)
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS usage (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id      INTEGER NOT NULL,
			token_id     INTEGER NOT NULL,
			channel_id   INTEGER,
			request_id   TEXT NOT NULL,
			model        TEXT NOT NULL DEFAULT '',
			characters   INTEGER NOT NULL DEFAULT 0,
			audio_ms     INTEGER NOT NULL DEFAULT 0,
			audio_bytes  INTEGER NOT NULL DEFAULT 0,
			pre_consumed INTEGER NOT NULL DEFAULT 0,
			actual_quota INTEGER NOT NULL DEFAULT 0,
			status       INTEGER NOT NULL DEFAULT 1,
			created_at   TEXT NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
		return fmt.Errorf("create usage: %w", err)
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_usage_user_time ON usage(user_id, created_at)`); err != nil {
		return fmt.Errorf("create idx_usage_user_time: %w", err)
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_usage_token_time ON usage(token_id, created_at)`); err != nil {
		return fmt.Errorf("create idx_usage_token_time: %w", err)
	}
	// request_id 索引:退款幂等查询(UsageRefund 按 request_id + status 判重)
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_usage_request ON usage(request_id)`); err != nil {
		return fmt.Errorf("create idx_usage_request: %w", err)
	}

	// ===== 阶段 3b:usage 补计费三列 =====
	//
	// 【为什么 3b 必须改表】阶段 3a 建 usage 表时漏建了计费列,但 3b 的
	// API 契约(策划 §4.1)要求 usage 详单返回 pre_consumed/actual_quota/status,
	// §3 三段式计费也必须落这两列做对账。因此这里**打破 §2"3b 不改表"的表述**,
	// 用 ADD COLUMN 补齐(只增列,不改/不删旧列,老库老数据兼容)。
	//
	//   - pre_consumed: PreConsume 预扣额度(退款时按此全额回退)
	//   - actual_quota: Settle 结算后的实际额度(失败退款行为 0)
	//   - status: 1=settled 2=refunded(default 1:3a 老行只在合成成功时写入,
	//     语义上就是已结算,补列后按 1 读最准确)
	//
	// SQLite 无 "ADD COLUMN IF NOT EXISTS",故先 PRAGMA 自检再 ALTER(幂等)。
	if err := s.ensureColumn("usage", "pre_consumed",
		`pre_consumed INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := s.ensureColumn("usage", "actual_quota",
		`actual_quota INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := s.ensureColumn("usage", "status",
		`status INTEGER NOT NULL DEFAULT 1`); err != nil {
		return err
	}

	return nil
}

// ensureColumn 幂等地给 table 补一列:PRAGMA table_info 自检,缺失才 ALTER TABLE ADD COLUMN。
//
// 注意:本 Store 是 SetMaxOpenConns(1),持有 rows 时再 Exec 会自锁,
// 因此先把列名读进内存、显式 Close rows,再做 ALTER。
//
// table/column/columnDDL 全部是代码内常量,不接受外部输入,无注入面。
func (s *Store) ensureColumn(table, column, columnDDL string) error {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return fmt.Errorf("store: pragma table_info(%s): %w", table, err)
	}
	existing := make(map[string]struct{}, 16)
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notNull   int
			dfltValue any
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: pragma table_info(%s) scan: %w", table, err)
		}
		existing[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("store: pragma table_info(%s) rows: %w", table, err)
	}
	// 必须先 Close 再 ALTER(单连接)
	_ = rows.Close()

	if _, ok := existing[column]; ok {
		return nil // 已存在,幂等返回
	}
	if _, err := s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + columnDDL); err != nil {
		return fmt.Errorf("store: alter %s add %s: %w", table, column, err)
	}
	return nil
}
