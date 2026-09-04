package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// SettingsKey 是 settings 表的合法键白名单;防止上游拼写错误静默落库。
// 留空 hash 表示允许任意键;严格模式时把允许的键填进来。
//
// 本期(M0)使用宽松模式:任何非空字符串键都可以写入。
// 收紧时把对应键填入 allowedSettingsKeys 即可。
var allowedSettingsKeys = map[string]struct{}{}

// SettingsAccess 返回单条配置;键不存在返回 ("", false, nil)。
// 第二返回值表示键是否存在,便于上层区分"未设置"和"值为空串"。
func (s *Store) SettingsGet(key string) (string, bool, error) {
	if key == "" {
		return "", false, fmt.Errorf("store: settings key is empty")
	}
	row := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key)
	var v string
	err := row.Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: settings get %q: %w", key, err)
	}
	return v, true, nil
}

// SettingsSet 写入单条配置;空值会删除该键(SQLite 没 NULL 写法更直观)。
func (s *Store) SettingsSet(key, value string) error {
	if key == "" {
		return fmt.Errorf("store: settings key is empty")
	}
	if !isAllowedSettingsKey(key) {
		return fmt.Errorf("store: settings key %q not in whitelist", key)
	}
	_, err := s.db.Exec(`
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, datetime('now'))
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value)
	if err != nil {
		return fmt.Errorf("store: settings set %q: %w", key, err)
	}
	return nil
}

// SettingsDelete 显式删除单条键;键不存在不报错。
func (s *Store) SettingsDelete(key string) error {
	if key == "" {
		return fmt.Errorf("store: settings key is empty")
	}
	_, err := s.db.Exec(`DELETE FROM settings WHERE key = ?`, key)
	if err != nil {
		return fmt.Errorf("store: settings delete %q: %w", key, err)
	}
	return nil
}

// SettingsGetAll 返回所有配置;按 key 升序。
func (s *Store) SettingsGetAll() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("store: settings getall: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("store: settings getall scan: %w", err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: settings getall rows: %w", err)
	}
	return out, nil
}

// SettingsSetBatch 一次性写入多对;保留单事务原子性,失败整体回滚。
// 适合 /api/setup 一次性写入全局配置。
func (s *Store) SettingsSetBatch(kv map[string]string) error {
	if len(kv) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: settings setbatch begin: %w", err)
	}
	stmt, err := tx.Prepare(`
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, datetime('now'))
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: settings setbatch prepare: %w", err)
	}
	for k, v := range kv {
		if k == "" {
			_ = tx.Rollback()
			return fmt.Errorf("store: settings setbatch: empty key")
		}
		if !isAllowedSettingsKey(k) {
			_ = tx.Rollback()
			return fmt.Errorf("store: settings setbatch: key %q not in whitelist", k)
		}
		if _, err := stmt.Exec(k, v); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return fmt.Errorf("store: settings setbatch exec %q: %w", k, err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: settings setbatch close stmt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: settings setbatch commit: %w", err)
	}
	return nil
}

// SettingsGetInt 返回整型配置,带默认值;键不存在或解析失败时回退到 def。
func (s *Store) SettingsGetInt(key string, def int) (int, error) {
	v, ok, err := s.SettingsGet(key)
	if err != nil || !ok {
		return def, err
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def, nil // 解析失败静默回退,不污染调用方
	}
	return n, nil
}

// SettingsGetBool 返回 bool 配置,接受 "1"/"true"/"t"/"TRUE" 等;
func (s *Store) SettingsGetBool(key string, def bool) (bool, error) {
	v, ok, err := s.SettingsGet(key)
	if err != nil || !ok {
		return def, err
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, nil
	}
	return b, nil
}

// SettingsGetDuration 返回 duration 配置;支持 "30s" "5m" "1h" 等。
func (s *Store) SettingsGetDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok, err := s.SettingsGet(key)
	if err != nil || !ok {
		return def, err
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def, nil
	}
	return d, nil
}

// isAllowedSettingsKey 检查 key 是否在白名单;白名单空时全放行。
func isAllowedSettingsKey(key string) bool {
	if len(allowedSettingsKeys) == 0 {
		return true
	}
	_, ok := allowedSettingsKeys[key]
	return ok
}
