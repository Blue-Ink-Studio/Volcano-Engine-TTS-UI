// controller/seed.go — 阶段 3a 启动期 seed 旧 key
package controller

import (
	"fmt"

	"github.com/volcano-tts/tts-api/setting"
	"github.com/volcano-tts/tts-api/store"
)

// SeedDefaultTokensIfEmpty 在 tokens 表为空时,把 OPENAI_TTS_API_KEY 的
// 每个 key seed 成"默认 user 'default'"下的一条 unlimited token。
//
// 行为(决策 #3 对齐):
//   - 仅当 tokens 表**全空**(SELECT COUNT(*)=0,不限 status)时才执行;跑过就跳过
//   - 把 setting.GetAuthAPIKeys() 列表里的 key 逐个 seed
//   - **单事务由 store.SeedDefaultTokens 原子完成**(default user + 全部 tokens
//     成功才 commit,任一失败整体回滚)——不再在 controller 层循环 autocommit 插入
//   - 失败仅 log warn,不阻断启动(决策:实现口径)
//
// 返回:
//   - seeded > 0:本次实际新插入的 token 数
//   - seeded == 0:未执行 seed(tokens 表非空,或 OPENAI_TTS_API_KEY 为空)
func SeedDefaultTokensIfEmpty(s *store.Store) (int, error) {
	// 1) 决策:仅当 tokens 表全空时执行(防止重复 seed)
	n, err := s.TokenCount()
	if err != nil {
		return 0, fmt.Errorf("count tokens: %w", err)
	}
	if n > 0 {
		return 0, nil
	}

	// 2) 拿 setting 里的 auth key 列表;空就跳过(seed 没东西可做)
	authKeys := setting.GetAuthAPIKeys()
	if len(authKeys) == 0 {
		return 0, nil
	}

	// 3) 单事务下沉 store:user + tokens 原子提交,失败回滚(修复 P1)
	return s.SeedDefaultTokens(authKeys, "default")
}
