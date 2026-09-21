package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// SetupApply 原子提交:settings 批量写入 + voices 全部插入,任一失败整体回滚,
// 保证 db 不会留下半残状态(原 controller 直接调 SettingsSetBatch + 循环
// VoiceInsert 时,voice 第 3 条失败 → settings 已写、voice 1/2 已落、voice 4
// 没了,db 处于"装了一半"的脏状态,只能靠重启救)。
//
// 行为契约:
//   - settingsKV 全部写入(settings 白名单校验复用 SettingsSetBatch 逻辑);
//   - voices 逐条插入;ErrInvalid 校验错误 → 整体回滚,errors.Is(err, ErrInvalid) 仍可用;
//   - voices 中已存在的 name 命中 ErrDuplicate → 静默跳过(不计 inserted,事务继续),
//     兼容"重复 setup 同一组 voice"场景;
//   - 其它 voice 错误 → 整体回滚;
//   - 全部成功 → tx.Commit,返回 inserted count(不含被 ErrDuplicate 跳过的)。
//
// 不动 lock 文件:lock 由 controller 层(installer.CreateLock)管理,
// 失败/成功都不应影响 db 事务(事务外)。
func (s *Store) SetupApply(settingsKV map[string]string, voices []Voice) (inserted int, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: setup apply begin: %w", err)
	}
	// defer Rollback:Commit 成功时 Rollback 返 sql.ErrTxDone,无害。
	defer func() {
		_ = tx.Rollback()
	}()

	// 1) 写 settings(同事务)
	if len(settingsKV) > 0 {
		if err := settingsSetBatchTx(tx, settingsKV); err != nil {
			return 0, err
		}
	}

	// 2) 逐条插 voice;ErrDuplicate 跳过,ErrInvalid/其它整体回滚
	for i, v := range voices {
		// trim 各字段,跟 VoiceInsert 保持一致(防止 controller 已经 trim 过但
		// 未来调用方不 trim 时行为不一致)
		v.Name = strings.TrimSpace(v.Name)
		v.Speaker = strings.TrimSpace(v.Speaker)
		v.ResourceID = strings.TrimSpace(v.ResourceID)
		v.Model = strings.TrimSpace(v.Model)
		v.Language = strings.TrimSpace(v.Language)
		v.Description = strings.TrimSpace(v.Description)
		// Enabled 走 setup 语义:用户主动配置时保留(允许 admin 预设 disabled);
		// 但 controller.SetupSubmitHandler 走的是用户首次安装,统一 enabled=true。
		// 这里不强制覆盖,保持原值(等同 VoiceInsert 行为)。

		id, err := voiceInsertTx(tx, v)
		if err != nil {
			if errors.Is(err, ErrDuplicate) {
				// 已存在,跳过(不计 inserted)
				continue
			}
			// ErrInvalid / DB 错误等:整体回滚,把原始 error 透传(已 wrap ErrInvalid)
			return 0, fmt.Errorf("store: setup apply voice[%d] %q: %w", i, v.Name, err)
		}
		_ = id // id 当前用不到,后续如果 controller 需要可加返回值
		inserted++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: setup apply commit: %w", err)
	}
	return inserted, nil
}

// settingsSetBatchTx 在已有 tx 上写 settings;逻辑跟 SettingsSetBatch 一致
// 但用 tx 代替 s.db。失败时**不**回滚(交给 caller 决定);caller 拿 err 后
// defer Rollback 兜底。
func settingsSetBatchTx(tx *sql.Tx, kv map[string]string) error {
	if len(kv) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(`
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, datetime('now'))
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`)
	if err != nil {
		return fmt.Errorf("store: settings setbatch prepare: %w", err)
	}
	defer stmt.Close()

	for k, v := range kv {
		if k == "" {
			return fmt.Errorf("store: settings setbatch: empty key")
		}
		if !isAllowedSettingsKey(k) {
			return fmt.Errorf("store: settings setbatch: key %q not in whitelist", k)
		}
		if _, err := stmt.Exec(k, v); err != nil {
			return fmt.Errorf("store: settings setbatch exec %q: %w", k, err)
		}
	}
	return nil
}

// voiceInsertTx 在已有 tx 上插 voice;跟 VoiceInsert 逻辑一致。
// 校验(name 格式 / speaker / resource_id)用 ErrInvalid wrap;
// 唯一冲突返 ErrDuplicate;其它错误返 wrap 的 db error。
func voiceInsertTx(tx *sql.Tx, v Voice) (int64, error) {
	if err := validateVoiceName(v.Name); err != nil {
		return 0, fmt.Errorf("%w: %s", ErrInvalid, err.Error())
	}
	if v.Speaker == "" {
		return 0, fmt.Errorf("%w: speaker is required", ErrInvalid)
	}
	if v.ResourceID == "" {
		return 0, fmt.Errorf("%w: resource_id is required", ErrInvalid)
	}
	res, err := tx.Exec(`
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
