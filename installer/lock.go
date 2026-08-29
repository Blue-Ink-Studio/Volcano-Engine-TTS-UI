// Package installer 负责安装状态判定:lock 文件检测/创建/删除、
// 启动期 Detect 流程、损坏自愈回退。
//
// 设计要点:
//   - lock 文件路径: <dbDir>/installed.lock,与 tts.db 同目录
//   - lock 不存在 = 未安装(进入安装模式)
//   - lock 存在 + 库 OK = 已安装(正常模式)
//   - lock 存在 + 库损坏 = 自动备份 + 删 lock + 回到安装模式
//   - 写顺序: 先写库,后写 lock(避免 lock 在、库是半成品)
//   - 写锁用临时文件 + 原子 rename,避免崩溃中途留半成品 lock
package installer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// lockFileName 是 lock 文件名;固定不变,所有部署共用。
const lockFileName = "installed.lock"

// schemaVersion 是 lock 内容里的 schema 版本号;留作未来版本兼容判断。
// 未来若有破坏性升级,可读这个值决定是否要重装/迁移。
const schemaVersion = "1"

// LockPath 返回给定 db 路径下 lock 文件的绝对路径。
// dbPath 通常是 .db 文件路径(不是目录);若传入目录则直接拼 lockFileName。
func LockPath(dbPath string) string {
	if dbPath == "" {
		return lockFileName
	}
	// 如果 dbPath 是已存在的目录,直接拼文件名
	if info, err := os.Stat(dbPath); err == nil && info.IsDir() {
		return filepath.Join(dbPath, lockFileName)
	}
	dir := filepath.Dir(dbPath)
	return filepath.Join(dir, lockFileName)
}

// ErrLockExists 表示 lock 已存在;CreateLock 会返回这个,提醒上层别覆盖。
var ErrLockExists = errors.New("installer: lock already exists")

// LockExists 检测 lock 文件是否存在;不存在不算错误(常见的"未安装"状态)。
func LockExists(dbPath string) (bool, error) {
	p := LockPath(dbPath)
	_, err := os.Stat(p)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("installer: stat lock %q: %w", p, err)
}

// CreateLock 原子写入 lock 文件;lock 已存在返回 ErrLockExists。
// 内容: "version <schemaVersion> <RFC3339 时间戳>"
func CreateLock(dbPath string) error {
	exists, err := LockExists(dbPath)
	if err != nil {
		return err
	}
	if exists {
		return ErrLockExists
	}

	p := LockPath(dbPath)
	content := fmt.Sprintf("version %s %s\n", schemaVersion, time.Now().UTC().Format(time.RFC3339))

	// 原子写入:先写临时文件,再 rename
	dir := filepath.Dir(p)
	tmp, err := os.CreateTemp(dir, ".installed.lock.*.tmp")
	if err != nil {
		return fmt.Errorf("installer: create lock tmp: %w", err)
	}
	tmpName := tmp.Name()
	// 确保临时文件最终被清理(出错时)
	defer func() {
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("installer: write lock tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("installer: close lock tmp: %w", err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		return fmt.Errorf("installer: rename lock tmp: %w", err)
	}
	return nil
}

// DeleteLock 删 lock;不存在不报错。
// 主要用于损坏自愈流程和测试清理。
func DeleteLock(dbPath string) error {
	p := LockPath(dbPath)
	err := os.Remove(p)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("installer: remove lock %q: %w", p, err)
}

// ReadLock 读 lock 内容;主要用于诊断日志和未来版本兼容判断。
// 不存在返回 ("", nil)。
func ReadLock(dbPath string) (string, error) {
	p := LockPath(dbPath)
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("installer: read lock %q: %w", p, err)
	}
	return strings.TrimSpace(string(b)), nil
}
