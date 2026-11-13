package store

import (
	"path/filepath"
	"testing"
)

// 本文件覆盖 STAGE6 的迁移与音色归属(契约 docs/STAGE6_MULTI_PROVIDER_PLAN.md)。
//
// 注意:*_test.go 被 .gitignore 忽略(项目约定,测试不提交),
// 所以这些断言的价值在于**本地验收**,而不是 CI 门禁。

// ---------------------------------------------------------------------------
// §4.2 的核心回归:voices 重建必须真的解除"全局唯一",且必须被版本门控
// ---------------------------------------------------------------------------

// TestStage6VoicesRebuildRemovesGlobalUnique 是本阶段最重要的一条测试。
//
// 它锁死一个会让核心目标落空的缺陷:
//
//	旧 voices 表的 name 是 **表级** `TEXT NOT NULL UNIQUE`(生成
//	sqlite_autoindex_voices_1)。`DROP INDEX idx_voices_name` 只能删掉那个
//	冗余的命名索引,**表级约束纹丝不动** —— 于是"同名音色分属不同渠道"
//	永远失败(实测:UNIQUE constraint failed: voices.name)。
//
// 测试做法:把库退回 v6 的老 voices 结构(表级 UNIQUE + 一条历史音色)
// → 重开触发重建 → 断言:历史音色被清空、同名跨渠道可插入、同渠道同名被拒。
func TestStage6VoicesRebuildRemovesGlobalUnique(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stage6.db")

	// 1) 正常开一次(此刻 schema 已是 7,voices 已是新结构)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// 2) 把它改造成 "v6 老库":重建出带表级 UNIQUE 的旧 voices + 一条历史音色
	if _, err := s.DB().Exec(`DROP TABLE voices`); err != nil {
		t.Fatalf("drop voices: %v", err)
	}
	if _, err := s.DB().Exec(`CREATE TABLE voices (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		name        TEXT NOT NULL UNIQUE,   -- ← 表级 UNIQUE,这是被重建的理由
		speaker     TEXT NOT NULL,
		resource_id TEXT NOT NULL,
		model       TEXT DEFAULT '',
		language    TEXT DEFAULT '',
		description TEXT DEFAULT '',
		enabled     INTEGER NOT NULL DEFAULT 1,
		created_at  TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		t.Fatalf("recreate legacy voices: %v", err)
	}
	if _, err := s.DB().Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_voices_name ON voices(name)`); err != nil {
		t.Fatalf("create legacy idx: %v", err)
	}
	if _, err := s.DB().Exec(
		`INSERT INTO voices (name, speaker, resource_id) VALUES ('chun','S_legacy','seed-icl-2.0')`); err != nil {
		t.Fatalf("insert legacy voice: %v", err)
	}
	// 退版本 + 删掉 STAGE6 的新索引/新列痕迹
	if _, err := s.DB().Exec(`DELETE FROM schema_version WHERE version = ?`, stage6SchemaVersion); err != nil {
		t.Fatalf("rewind version: %v", err)
	}
	if _, err := s.DB().Exec(`DROP INDEX IF EXISTS idx_voices_channel_name`); err != nil {
		t.Fatalf("drop new idx: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 3) 重开 —— MAX(version)=6 < 7,重建应当执行
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen for rebuild: %v", err)
	}
	defer func() { _ = s2.Close() }()

	// 3a) C12:历史音色应被清空
	count, err := s2.VoiceCount()
	if err != nil {
		t.Fatalf("voice count: %v", err)
	}
	if count != 0 {
		t.Fatalf("历史音色应被清空(C12),实际仍有 %d 行", count)
	}

	// 3b) 版本应记为 7
	if got := s2.currentSchemaVersion(); got != stage6SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", got, stage6SchemaVersion)
	}

	// 3c) 建两个渠道,同名单个音色 —— 必须都能建(表级 UNIQUE 已解除)
	ch1, err := s2.ChannelInsert(Channel{
		Name: "huoshan", Provider: "volcano",
		CredentialsJSON: `{"api_key":"k1"}`, Status: 1, Weight: 1,
	})
	if err != nil {
		t.Fatalf("insert channel1: %v", err)
	}
	ch2, err := s2.ChannelInsert(Channel{
		Name: "openai-main", Provider: "openai",
		CredentialsJSON: `{"api_key":"k2"}`, Status: 1, Weight: 1,
	})
	if err != nil {
		t.Fatalf("insert channel2: %v", err)
	}
	if _, err := s2.DB().Exec(
		`INSERT INTO voices (channel_id,name,speaker) VALUES (?, 'alloy','a1')`, ch1); err != nil {
		t.Fatalf("insert voice into channel1: %v", err)
	}
	if _, err := s2.DB().Exec(
		`INSERT INTO voices (channel_id,name,speaker) VALUES (?, 'alloy','a2')`, ch2); err != nil {
		t.Fatalf("同名音色应能跨渠道共存,却被拒: %v", err)
	}

	// 3d) 同渠道同名仍必须被拒
	if _, err := s2.DB().Exec(
		`INSERT INTO voices (channel_id,name,speaker) VALUES (?, 'alloy','a3')`, ch1); err == nil {
		t.Fatal("同渠道同名音色应被唯一索引拒绝,却插入成功了")
	}
}

// TestStage6MigrationIsVersionGated 锁死"重建只能跑一次"。
//
// 若门控用 schemaVersion 而非 stage6SchemaVersion,将来升到 8 时重建会再次
// 执行并把管理员重建好的音色**再清空一次**。这里通过"重建后再重开一次,
// 音色仍在"来断言门控生效。
func TestStage6MigrationIsVersionGated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stage6-gate.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	chID, err := s.ChannelInsert(Channel{
		Name: "huoshan", Provider: "volcano",
		CredentialsJSON: `{"api_key":"k"}`, Status: 1, Weight: 1,
	})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	if _, err := s.DB().Exec(
		`INSERT INTO voices (channel_id,name,speaker) VALUES (?, 'chun','S1')`, chID); err != nil {
		t.Fatalf("insert voice: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 重开:版本已是 7 → 重建必须**不再执行**,音色仍在
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	count, err := s2.VoiceCount()
	if err != nil {
		t.Fatalf("voice count: %v", err)
	}
	if count != 1 {
		t.Fatalf("门控失效:重开后又清空了音色(count=%d, want 1)", count)
	}
}

// TestStage6ChannelNameRejectsModelUnsafeChars 验证 S8:
// channel.name 现在是对外 model 名,必须能被客户端/SDK 请求出来。
func TestStage6ChannelNameRejectsModelUnsafeChars(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "stage6-name.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	bad := []string{"my channel", "a/b", "火山渠道", "with.dot", ""}
	for _, name := range bad {
		if _, err := s.ChannelInsert(Channel{
			Name: name, Provider: "volcano",
			CredentialsJSON: `{"api_key":"k"}`, Status: 1, Weight: 1,
		}); err == nil {
			t.Errorf("渠道名 %q 应被拒绝(不是合法 model 名),却建成功了", name)
		}
	}
	good := []string{"tts-1", "openai-main", "huoshan", "A_b-9"}
	for _, name := range good {
		if _, err := s.ChannelInsert(Channel{
			Name: name, Provider: "volcano",
			CredentialsJSON: `{"api_key":"k"}`, Status: 1, Weight: 1,
		}); err != nil {
			t.Errorf("渠道名 %q 应被接受,却被拒: %v", name, err)
		}
	}
}
