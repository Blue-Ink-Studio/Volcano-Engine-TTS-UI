package store

// schema 版本号演进与迁移函数注册表。
//
// 用法:每次结构性变更时 +1 schemaVersion 常量,并在 migrations 切片中追加 applyV<n>。
// migrate.go 会在 Open 时按版本号顺序应用。
//
// 注意:本文件留作未来扩展,本期 M0 阶段 schemaVersion=1,migrate() 在 db.go
// 内做基础建表,未触发 migrations 调度。切到 v2 时再启用。

// Migration 是从 version N-1 升级到 N 的迁移函数。
type Migration struct {
	From int
	To   int
	Fn   func(tx interface{ Exec(query string, args ...any) (any, error) }) error
}

// migrations 是按 From 升序排列的迁移列表;首条 From 必须等于 1。
// 留作占位,本期为空。
var migrations = []Migration{}

// schemaVersionRequested 是期望的 schema 版本号;db.go 里直接写常量。
// 这里留个常量引用便于未来从 db.go 解耦。
const schemaVersionRequested = 1

// CurrentVersion 返回当前代码期望的 schema 版本。
func CurrentVersion() int { return schemaVersionRequested }
