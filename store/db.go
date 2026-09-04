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
const schemaVersion = 1

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
	return nil
}
