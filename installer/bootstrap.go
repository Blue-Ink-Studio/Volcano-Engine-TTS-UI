package installer

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/volcano-tts/tts-api/store"
)

// Mode 表示服务当前的运行模式。
// 启动期由 Detect 确定,运行期不变。
type Mode int

const (
	// ModeSetup 未安装,只放行 /setup + /api/setup/*。
	ModeSetup Mode = iota
	// ModeNormal 已安装,全部路由可用。
	ModeNormal
)

func (m Mode) String() string {
	switch m {
	case ModeSetup:
		return "setup"
	case ModeNormal:
		return "normal"
	}
	return "unknown"
}

// CurrentMode 是 Detect 确定的运行期模式;供 controller/middleware 双保险使用。
// 进程内只有一个二进制实例,所以全局变量是合适的;不必走 DI。
var CurrentMode Mode = ModeSetup // 默认 setup,Detect 后会被覆盖

// SetMode 在 Detect 完成后调用,设置进程级模式。
func SetMode(m Mode) { CurrentMode = m }

// GetMode 返回进程级模式;Controller 双保险用。
func GetMode() Mode { return CurrentMode }

// Result 是 Detect 的完整输出;调用方关心 Mode + 一些诊断信息。
type Result struct {
	Mode      Mode
	DBPath    string // 实际打开的 db 路径
	LockPath  string
	Corrupted bool   // 这次启动是否从损坏回退
	BackupTo  string // 损坏回退时备份文件路径
}

// ErrInUse 标识在 Detect 期间发现 db 正在被另一个进程占用;
// 这种情况下不应该自动 rename,会破坏另一个进程的运行。
// 上层应记录日志并按"装模式"启动,等下次重启再处理。
var ErrInUse = errors.New("installer: database is locked by another process")

// Detect 是启动期的总入口:打开/创建 db、判定 lock、检测损坏并自愈。
//
// 流程:
//  1. Open db(可能新建)
//  2. 检查 lock:
//     - 不存在 → ModeSetup
//     - 存在 → 跑 IntegrityCheck
//     - 通过 → ModeNormal
//     - 不通过 → 备份 db.corrupt-<ts> + 删 lock + ModeSetup(并标记 Corrupted=true)
//
// 返回的 *store.Store 必须由调用方在进程退出时 Close。
func Detect(dbPath string) (*store.Store, Result, error) {
	if dbPath == "" {
		return nil, Result{}, fmt.Errorf("installer: db path is empty")
	}

	res := Result{
		DBPath:   dbPath,
		LockPath: LockPath(dbPath),
	}

	s, err := store.Open(dbPath)
	if err != nil {
		// 打开失败通常意味着文件损坏;走自愈回退。
		// 重要:不要区分"不存在"和"损坏"——SQLite 第一次 Open 会自动建空库,
		// 如果"不存在"能走到这里说明更严重的系统错误,也不该贸然启动。
		if backup, ok := tryBackupCorrupt(dbPath, err); ok {
			res.Corrupted = true
			res.BackupTo = backup
			log.Printf("[installer] 检测到损坏 db,已备份到 %q,删除 lock,回退到安装模式", backup)
		} else {
			return nil, res, fmt.Errorf("installer: open db %q failed: %w", dbPath, err)
		}
	}

	// lock 状态判定
	exists, err := LockExists(dbPath)
	if err != nil {
		if s != nil {
			_ = s.Close()
		}
		return nil, res, fmt.Errorf("installer: lock check failed: %w", err)
	}
	if !exists {
		res.Mode = ModeSetup
		SetMode(ModeSetup)
		if !res.Corrupted {
			log.Printf("[installer] 启动模式: 安装模式(无 lock 文件)")
		}
		return s, res, nil
	}

	// lock 在,跑完整性检查
	if s == nil {
		// 自愈回退已经走完,应该删了 lock;但保险起见再删一次
		if err := DeleteLock(dbPath); err != nil {
			return nil, res, fmt.Errorf("installer: delete lock after fallback: %w", err)
		}
		res.Mode = ModeSetup
		SetMode(ModeSetup)
		return nil, res, nil
	}

	check, err := s.IntegrityCheck()
	if err != nil {
		_ = s.Close()
		// integrity_check 自身报错,等同损坏,走自愈
		backup, ok := tryBackupCorrupt(dbPath, err)
		if !ok {
			return nil, res, fmt.Errorf("installer: integrity_check failed: %w", err)
		}
		_ = DeleteLock(dbPath)
		res.Corrupted = true
		res.BackupTo = backup
		res.Mode = ModeSetup
		SetMode(ModeSetup)
		log.Printf("[installer] integrity_check 错误,已备份到 %q,删除 lock,回退到安装模式", backup)
		return nil, res, nil
	}
	if check != "ok" {
		_ = s.Close()
		backup, ok := tryBackupCorrupt(dbPath, fmt.Errorf("integrity_check returned: %s", check))
		if !ok {
			return nil, res, fmt.Errorf("installer: integrity_check = %q (not ok)", check)
		}
		_ = DeleteLock(dbPath)
		res.Corrupted = true
		res.BackupTo = backup
		res.Mode = ModeSetup
		SetMode(ModeSetup)
		log.Printf("[installer] 库不完整(integrity_check=%q),已备份到 %q,删除 lock,回退到安装模式", check, backup)
		return nil, res, nil
	}

	res.Mode = ModeNormal
	SetMode(ModeNormal)
	log.Printf("[installer] 启动模式: 正常模式(lock=%s)", res.LockPath)
	return s, res, nil
}

// tryBackupCorrupt 尝试把损坏的 db 文件 rename 为 .corrupt-<unix-ms>;
// 成功返回 (新路径, true),失败 (任何原因) 返回 ("", false)。
// 注意:这里不返回 error,因为 "无法备份" 不应阻止回退(可以后续人工排查)。
func tryBackupCorrupt(dbPath string, reason error) (string, bool) {
	if dbPath == "" {
		return "", false
	}
	// 不存在的话没法 rename(也没必要)
	if _, err := os.Stat(dbPath); err != nil {
		return "", false
	}
	ts := time.Now().UnixMilli()
	backup := fmt.Sprintf("%s.corrupt-%d", dbPath, ts)
	if err := os.Rename(dbPath, backup); err != nil {
		log.Printf("[installer] 备份损坏 db 失败: %v(将直接重建空库)", err)
		return "", false
	}
	log.Printf("[installer] 损坏原因: %v", reason)
	return backup, true
}

// EnsureDBDir 确保 dbPath 所在目录存在(对首次安装很有用;
// 当 dbDir 是新目录时 store.Open 之前需要先 mkdir)。
func EnsureDBDir(dbPath string) error {
	if dbPath == "" {
		return nil
	}
	dir := filepath.Dir(dbPath)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}
