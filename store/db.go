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
	"errors"
	"fmt"
	"log"

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
//	5 → STAGE4 自助门户 + 完整计费(决策 C:扣费主源 token → user.balance):
//	    users 补 4 列(password_hash/is_admin/balance/token_version)、
//	    tokens 补 limit_quota、usage.status 增 0=预扣中态、新增 charge_logs/redemptions 表
//	6 → STAGE5 Setup 改版 + 用户体系埋点:
//	    users 补 role/user_group、channels 补 channel_group、is_admin 冻结(role 成唯一真源)、
//	    owner 唯一性索引;并做一次**版本门控**的 is_admin → role 回填
const schemaVersion = 6

// stage5SchemaVersion 是引入「is_admin → role 一次性回填」的那个版本号。
//
// 门控必须钉在**引入时的那个版本**(6),而不是当前的 schemaVersion ——
// 否则将来 schemaVersion 升到 7 时,`curVersion(6) < schemaVersion(7)` 会再次成立,
// 回填被重跑。而重跑是破坏性的:is_admin 已冻结,`role='admin' WHERE is_admin=1`
// 会把"曾提升、后降级"的账号**重新提权**(权限静默回弹)。
const stage5SchemaVersion = 6

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

	// 读取"升级前"的版本号 —— 供 STAGE5 的一次性回填做门控(见 stage5SchemaVersion 的注释)。
	//
	// 必须在**任何** INSERT schema_version 之前读,否则读到的就是升级后的值,
	// 门控永远不成立、回填被跳过 ⇒ 所有历史 admin 静默降级成 user。
	curVersion := s.currentSchemaVersion()

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

	// ⚠️ schema_version 的写入**不在这个位置** —— 它被移到 migrate() 末尾(STAGE5 §4.2)。
	//
	// 原因:STAGE5 引入了一次「is_admin → role」的破坏性回填。若版本号在回填**之前**
	// 就记下,一旦回填中途失败或进程被杀,下次启动会因"版本已是 6"而跳过回填
	// ⇒ 所有历史 admin 静默降级成 user(且没有任何日志提示)。
	// 因此版本标记必须与回填**在同一事务内**提交,末尾的那次写入只作记账。

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

	// usage.upstream_req_id:上游(火山)返回的 ReqID,仅用于对账追溯。
	//
	// 【为什么不让 request_id 直接改成上游 id】request_id 是预扣/结算/退款
	// 三段式的**定位键**(idx_usage_request_pending 与退款幂等都依赖它),
	// 必须在整条生命周期内保持稳定;否则结算后再按原键找行会找不到,
	// 状态机的"单向前进"判定就退化成 ErrNotFound,无法区分"已结算"与"不存在"。
	if err := s.ensureColumn("usage", "upstream_req_id",
		`upstream_req_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}

	// ===== 阶段 4(STAGE4):普通用户自助门户 + 完整计费 =====
	//
	// 决策 C(new-api 式双扣):扣费主源从 tokens.remain_quota 迁到 users.balance;
	// tokens 退化为「认证凭证 + 可选准入限额」。
	// 契约见 docs/STAGE4_PORTAL_BILLING_PLAN.md(§5 Schema 变更)。

	// users:钱包 + WebUI 登录态
	if err := s.ensureColumn("users", "password_hash",
		`password_hash TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.ensureColumn("users", "is_admin",
		`is_admin INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := s.ensureColumn("users", "balance",
		`balance INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := s.ensureColumn("users", "token_version",
		`token_version INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}

	// tokens:可选准入限额(NULL = 不限额度,只准入不扣费)。
	// 该列可空,故 DDL 不带 NOT NULL DEFAULT。
	if err := s.ensureColumn("tokens", "limit_quota",
		`limit_quota INTEGER`); err != nil {
		return err
	}

	// charge_logs:资金流水/审计。
	// 只记余额来源(充值/兑换/手动调整/赠送);调用消费由 usage.actual_quota 覆盖,
	// **不双写**(决策 D25/D33)。
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS charge_logs (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id       INTEGER NOT NULL REFERENCES users(id),
			type          TEXT    NOT NULL,
			delta         INTEGER NOT NULL,
			balance_after INTEGER NOT NULL,
			source        TEXT    NOT NULL DEFAULT '',
			remark        TEXT    NOT NULL DEFAULT '',
			created_at    TEXT    NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
		return fmt.Errorf("create charge_logs: %w", err)
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_charge_logs_user_time ON charge_logs(user_id, created_at)`); err != nil {
		return fmt.Errorf("create idx_charge_logs_user_time: %w", err)
	}

	// redemptions:充值码(一次性;used_by/used_at 标记,原子兑换靠 UNIQUE + 条件 UPDATE)
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS redemptions (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			code       TEXT    NOT NULL UNIQUE,
			quota      INTEGER NOT NULL,
			used_by    INTEGER,
			used_at    TEXT,
			remark     TEXT    NOT NULL DEFAULT '',
			created_at TEXT    NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
		return fmt.Errorf("create redemptions: %w", err)
	}

	// usage 预扣中态的唯一性保护:同一 request_id 至多一条 status=0。
	// SQLite 支持部分索引;这是防「预扣重放 → 双重扣款」的落脚点(决策 X16)。
	if _, err := s.db.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_request_pending
		    ON usage(request_id) WHERE status = 0`); err != nil {
		return fmt.Errorf("create idx_usage_request_pending: %w", err)
	}

	// ===== 阶段 5(STAGE5):Setup 改版 + 用户体系埋点 =====
	//
	// 契约:docs/STAGE5_SETUP_ROLES_PLAN.md。三列全部走 ensureColumn(只增不改删)。
	// role 是**唯一真源**;is_admin 保留但冻结 —— 运行期一律不再读它(§6.1)。
	if err := s.ensureColumn("users", "role",
		`role TEXT NOT NULL DEFAULT 'user'`); err != nil {
		return err
	}
	if err := s.ensureColumn("users", "user_group",
		`user_group TEXT NOT NULL DEFAULT 'default'`); err != nil {
		return err
	}
	if err := s.ensureColumn("channels", "channel_group",
		`channel_group TEXT NOT NULL DEFAULT 'default'`); err != nil {
		return err
	}

	// 一次性列值回填:**版本门控**,只在本库从 <6 升上来时执行。
	//
	// 不做门控的后果见 stage5SchemaVersion 的注释(重启会把被降级的账号重新提权)。
	if curVersion < stage5SchemaVersion {
		if err := s.applyStage5Backfill(); err != nil {
			return err
		}
	}

	// owner 唯一性:DB 级兜底(逻辑侧还有三处,见契约 §6.2)。
	//
	//   - 谓词带 `status <> 3`(UserDeleteStatus):**软删的 owner 不占位**。
	//     否则一个被软删的 owner 会把系统永久锁死 —— 索引占位、账号登不进、
	//     又没有"取消软删"的接口。
	//   - 必须 `IF NOT EXISTS`:否则"建失败降 WARN"会变成每次启动刷一条 WARN。
	//   - 必须放在回填**之后**:回填期间可能出现"零个/一个 owner"的中间态。
	if _, err := s.db.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_users_owner
		    ON users(role) WHERE role = 'owner' AND status <> 3`); err != nil {
		log.Printf("[store] WARN: 创建 owner 唯一性索引失败,降级为仅逻辑保证(不阻断启动): %v", err)
	}

	// 版本记账(与回填的原子标记互为兜底;此处 INSERT OR IGNORE 幂等)。
	if _, err := s.db.Exec(`
		INSERT OR IGNORE INTO schema_version (version) VALUES (?)`, schemaVersion); err != nil {
		return fmt.Errorf("insert schema_version: %w", err)
	}

	return nil
}

// currentSchemaVersion 返回库中已记录的最高 schema 版本;全新库(无记录)返回 0。
// 读取失败也返回 0 —— 那会让门控成立、回填照跑,对空库是正确行为。
func (s *Store) currentSchemaVersion() int {
	var v int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v); err != nil {
		return 0
	}
	return v
}

// applyStage5Backfill 是 5→6 的**一次性**数据回填,单事务完成三件事:
//
//  1. is_admin → role 映射(此时 role 刚由 ensureColumn 建出,全部是默认 'user');
//  2. 保证至少一个 owner(规则与启动期共用 ensureOwnerExistsTx);
//  3. 写入版本号 —— **与 1/2 同事务**,这是本函数存在的核心理由:
//     版本不能先记后改数据,否则中途失败会导致下次启动跳过回填(§4.2)。
func (s *Store) applyStage5Backfill() error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: stage5 backfill begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		UPDATE users SET role = CASE WHEN is_admin = 1 THEN 'admin' ELSE 'user' END,
		                  updated_at = datetime('now')`); err != nil {
		return fmt.Errorf("store: stage5 map is_admin to role: %w", err)
	}

	ownerID, err := ensureOwnerExistsTx(tx)
	if err != nil {
		return err
	}
	if ownerID != 0 {
		log.Printf("[store] STAGE5 回填:已把 user_id=%d 提升为 owner", ownerID)
	}

	if _, err := tx.Exec(`INSERT OR IGNORE INTO schema_version (version) VALUES (?)`,
		stage5SchemaVersion); err != nil {
		return fmt.Errorf("store: stage5 version mark: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: stage5 backfill commit: %w", err)
	}
	return nil
}

// ensureOwnerExistsTx 是 owner 保障的**唯一实现**(STAGE5 §5.2 / 定案 A5)。
//
// migrate 的一次性回填与启动期的 EnforceInitialSecurityPolicy 都调它,
// 避免"两处各写一份提 owner 规则"造成漂移(那会提出两个 owner,再由唯一索引兜住 →
// 表现为启动 WARN,问题被掩盖而不是被解决)。
//
// 幂等:已有可用 owner(role='owner' 且未软删)则直接返回 0。
// 返回本次被提升的 user_id(0 = 未改动)。
//
// 候选顺序(均要求 status=1 启用 —— 把 owner 提给一个禁用/软删账号等于没提,
// 系统仍无人可管,所以这里比契约原文的"非软删"更严):
//  ① name='default' 且 is_admin=1(历史默认管理员,优先保持原状)
//  ② 最早的 is_admin=1
//  ③ 最早的启用用户(打 WARN,说明是兜底提升)
func ensureOwnerExistsTx(tx *sql.Tx) (int64, error) {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'owner' AND status <> ?`,
		UserDeleteStatus).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: ensure owner count: %w", err)
	}
	if n > 0 {
		return 0, nil
	}

	pick := func(query string, args ...any) (int64, bool, error) {
		var id int64
		err := tx.QueryRow(query, args...).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, fmt.Errorf("store: ensure owner pick: %w", err)
		}
		return id, true, nil
	}

	var (
		id    int64
		found bool
		warn  bool
		err   error
	)
	if id, found, err = pick(`SELECT id FROM users WHERE status = 1 AND is_admin = 1 AND name = 'default' ORDER BY id ASC LIMIT 1`); err != nil {
		return 0, err
	}
	if !found {
		if id, found, err = pick(`SELECT id FROM users WHERE status = 1 AND is_admin = 1 ORDER BY id ASC LIMIT 1`); err != nil {
			return 0, err
		}
	}
	if !found {
		warn = true
		if id, found, err = pick(`SELECT id FROM users WHERE status = 1 ORDER BY id ASC LIMIT 1`); err != nil {
			return 0, err
		}
	}
	if !found {
		// 库是空的(或没有任何启用用户):不造 owner,交给 setup 创建。
		return 0, nil
	}

	// 提升,带 NOT EXISTS 护卫(并发/重复调用都不会提出第二个 owner)。
	res, err := tx.Exec(`
		UPDATE users SET role = 'owner', updated_at = datetime('now')
		WHERE id = ? AND NOT EXISTS (SELECT 1 FROM users WHERE role = 'owner' AND status <> ?)`,
		id, UserDeleteStatus)
	if err != nil {
		return 0, fmt.Errorf("store: ensure owner promote id=%d: %w", id, err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return 0, nil
	}
	if warn {
		log.Printf("[store] WARN: 系统既无 owner 也无 is_admin 历史用户,已把最早的启用用户 user_id=%d 提升为 owner(STAGE5 §5.2 兜底)", id)
	}
	return id, nil
}

// EnsureOwnerExists 是 ensureOwnerExistsTx 的自管事务版本(启动期调用)。
//
// 用途:owner 被删除/软删后自动补位,避免系统陷入"没人能管"的状态。
// 注意它会**静默改变权限**(有人变成 owner),因此补位时必然打日志。
func (s *Store) EnsureOwnerExists() (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: ensure owner begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	id, err := ensureOwnerExistsTx(tx)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: ensure owner commit: %w", err)
	}
	return id, nil
}

// EnforceInitialSecurityPolicy 应用启动期的初始安全策略,由 main 在**每次启动**调用一次。
//
// 做两件互相独立的事:
//
//	(a) 确保系统里始终有 owner —— 委托给 EnsureOwnerExists()(owner 保障的**唯一实现**,
//	    STAGE5 §5.2 / 定案 A5)。全新安装时它什么都不做(setup 会创建 owner);
//	    owner 被删除/软删后它会自动补位并打日志。
//	(b) 【STAGE4 的 D24 防线,**必须保留**】default 账号存在且尚未设密码时,
//	    强制 allow_register=0 并打 WARN —— 防止"空密码管理员 + 开放注册"这一攻击面组合。
//
// ⚠️ STAGE5 只允许改 (a):原先这里是「把 default 提升为 is_admin=1」,现在 is_admin 已冻结,
// 必须改为 role 化的 owner 保障。**(b) 不得因为重写本函数而丢失。**
//
// 幂等:可重复调用。(b) 只在 allow_register **从未被设置过**时才动作,绝不覆盖管理员的显式设置(D31(c))。
func (s *Store) EnforceInitialSecurityPolicy() error {
	// (a) owner 保障(唯一实现,规则见 ensureOwnerExistsTx)
	if _, err := s.EnsureOwnerExists(); err != nil {
		return err
	}

	// (b) D24:空密码的 default + 开放注册 = 危险组合,强制关闭注册
	var existing string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = 'allow_register'`).Scan(&existing)
	switch {
	case err == nil:
		return nil // 管理员已显式设置过,不覆盖
	case errors.Is(err, sql.ErrNoRows):
		// 未设置 → 继续评估
	default:
		return fmt.Errorf("store: enforce policy read allow_register: %w", err)
	}

	var hash string
	err = s.db.QueryRow(`SELECT password_hash FROM users WHERE name = 'default'`).Scan(&hash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil // 尚无 default 用户(全新库未 seed)→ 无需动作
	case err != nil:
		return fmt.Errorf("store: enforce policy read default user: %w", err)
	}
	if hash != "" {
		return nil // 已设密 → 无需强制关闭注册
	}

	if _, err := s.db.Exec(`
		INSERT INTO settings (key, value, updated_at) VALUES ('allow_register', '0', datetime('now'))
		ON CONFLICT(key) DO NOTHING`); err != nil {
		return fmt.Errorf("store: enforce policy set allow_register: %w", err)
	}
	log.Printf("[store] WARN: default 账号尚未设置密码,已强制关闭自由注册(allow_register=0)。" +
		"请从服务器本机或携带有效 admin_key 调用 POST /api/auth/setup-password 设置密码," +
		"之后可在管理后台按需重新开启注册。")
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
