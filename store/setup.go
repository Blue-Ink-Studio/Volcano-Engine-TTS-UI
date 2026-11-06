package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// SetupCreateOwner 是安装落库:**单事务**创建超管 + 写基础配置。
//
// 行为契约(docs/STAGE5_SETUP_ROLES_PLAN.md §7 / D5 = **DB 写入单事务**):
//   - owner 强制 `role='owner'`、`user_group='default'`、`balance=0`,且**必须**带
//     bcrypt 口令(调用方先算好;空口令直接 ErrInvalid —— 空密码的超管等于没有超管);
//   - `settingsKV` 可空;白名单校验与 SettingsSetBatch 一致;
//   - 任一失败整体回滚 —— 不允许出现"有账号但没配置"或反之的半残状态;
//   - 名字重名 → ErrDuplicate;其它校验错 → ErrInvalid。
//
// **不动 lock 文件**:lock 由 controller 层(installer.CreateLock)管理,处于事务之外;
// 这是刻意的 —— 文件写与 DB 事务无法原子,顺序由调用方保证(先提交 DB,再建 lock)。
func (s *Store) SetupCreateOwner(admin User, settingsKV map[string]string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: setup create owner begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if strings.TrimSpace(admin.PasswordHash) == "" {
		return 0, fmt.Errorf("%w: owner must have a password", ErrInvalid)
	}
	admin.Role = RoleOwner
	admin.UserGroup = "default"
	admin.Balance = 0
	if admin.Status == 0 {
		admin.Status = 1
	}

	id, err := insertUser(tx, admin)
	if err != nil {
		return 0, err
	}
	if err := settingsSetBatchTx(tx, settingsKV); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: setup create owner commit: %w", err)
	}
	return id, nil
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

// ── STAGE6 删除的历史实现(留作备注,便于回溯) ──────────────────────────
//
// `SetupApply(settingsKV, voices)` 与 `voiceInsertTx(tx, v)` 在 STAGE6 被删除。
//
// 删除理由:
//  1. STAGE5 起 setup 只建超管(SetupCreateOwner),这两个函数**已无调用方**;
//  2. 它们的 voice 插入路径**不带 channel_id** —— STAGE6 音色必须归属渠道,
//     留着它们只会造出永远路由不到的孤儿行(契约 docs/STAGE6_MULTI_PROVIDER_PLAN.md §9 G2)。
