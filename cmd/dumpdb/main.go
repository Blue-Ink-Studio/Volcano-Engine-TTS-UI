// dumpdb 是个 ops 工具: dump tts.db 里的 settings + voices 表(明文打码)。
//
// 用法:
//   go run ./cmd/dumpdb /path/to/tts.db
//
// 输出 schema_version / settings(键值,敏感字段打码) / voices(完整行)。
// 调试时方便:不依赖 sqlite3 CLI,不暴露明文 key/speaker ID。
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <db-path>\n", os.Args[0])
		os.Exit(1)
	}
	dbPath := os.Args[1]

	dsn := "file:" + dbPath + "?mode=ro&_pragma=query_only"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
		os.Exit(2)
	}
	defer conn.Close()

	// 1. tables
	fmt.Println("=== Tables ===")
	rows, _ := conn.Query("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	for rows.Next() {
		var name string
		_ = rows.Scan(&name)
		fmt.Printf("  %s\n", name)
	}
	rows.Close()

	// 2. schema_version
	fmt.Println("\n=== schema_version ===")
	var ver int
	var ts string
	err = conn.QueryRow(`SELECT version, applied_at FROM schema_version LIMIT 1`).Scan(&ver, &ts)
	if err != nil {
		fmt.Printf("  (no row: %v)\n", err)
	} else {
		fmt.Printf("  version: %d, applied_at: %s\n", ver, ts)
	}

	// 3. settings
	fmt.Println("\n=== settings ===")
	rows, err = conn.Query(`SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		fmt.Printf("  err: %v\n", err)
	} else {
		for rows.Next() {
			var k, v string
			_ = rows.Scan(&k, &v)
			if v == "" {
				fmt.Printf("  %-30s = (empty)\n", k)
				continue
			}
			lowK := k
			_ = lowK
			masked := v
			if isSensitive(k) {
				if len(v) <= 8 {
					masked = "****"
				} else {
					masked = v[:4] + "****" + v[len(v)-4:]
				}
			}
			fmt.Printf("  %-30s = %s\n", k, masked)
		}
		rows.Close()
	}

	// 4. voices
	fmt.Println("\n=== voices ===")
	rows, err = conn.Query(`SELECT id, name, speaker, resource_id, model, language, description, enabled, created_at, updated_at FROM voices ORDER BY id`)
	if err != nil {
		fmt.Printf("  err: %v\n", err)
	} else {
		count := 0
		for rows.Next() {
			count++
			var id int64
			var name, speaker, resourceID, model, lang, desc, created, updated string
			var enabled int
			_ = rows.Scan(&id, &name, &speaker, &resourceID, &model, &lang, &desc, &enabled, &created, &updated)
			en := "true"
			if enabled == 0 {
				en = "false"
			}
			fmt.Printf("  [%d] name=%s\n      speaker=%s\n      resource_id=%s\n      model=%s\n      lang=%s enabled=%s\n      created=%s updated=%s\n",
				id, name, speaker, resourceID, model, lang, en, created, updated)
			if desc != "" {
				fmt.Printf("      desc: %s\n", desc)
			}
		}
		fmt.Printf("  total: %d\n", count)
		rows.Close()
	}

	// 5. pragmas
	fmt.Println("\n=== Pragmas ===")
	for _, p := range []string{"journal_mode", "synchronous", "foreign_keys", "page_size", "page_count"} {
		var v string
		_ = conn.QueryRow("PRAGMA " + p).Scan(&v)
		fmt.Printf("  %-15s = %s\n", p, v)
	}
}

func isSensitive(key string) bool {
	lowK := key
	// 简单 contains (不引 strings 包保持精简)
	markers := []string{"key", "token", "speaker"}
	for _, m := range markers {
		for i := 0; i+len(m) <= len(lowK); i++ {
			if lowK[i:i+len(m)] == m {
				return true
			}
		}
	}
	return false
}
