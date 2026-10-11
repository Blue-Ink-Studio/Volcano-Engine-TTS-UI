// controller/seed.go — 阶段 3a 启动期 seed 旧 key
package controller

import (
	"errors"
	"fmt"
	"strings"

	"github.com/volcano-tts/tts-api/setting"
	"github.com/volcano-tts/tts-api/store"
)

// SeedDefaultTokensIfEmpty 在 tokens 表为空时,把 OPENAI_TTS_API_KEY 的
// 每个 key seed 成"默认 user 'default'"下的一条 unlimited token。
//
// 行为(决策 #4):
//   - 仅当 tokens 表里**没有 enabled token** 时才执行;跑过就跳过
//   - 把 setting.GetAuthAPIKeys() 列表里的 key 逐个 seed
//   - 单事务:user + tokens 全成功才 commit,任一失败整体回滚
//   - 失败仅 log warn,不阻断启动(决策:实现口径)
//
// 返回:
//   - seeded > 0:本次实际新插入的 token 数
//   - seeded == 0:未执行 seed(tokens 已存在,或 OPENAI_TTS_API_KEY 为空)
func SeedDefaultTokensIfEmpty(s *store.Store) (int, error) {
	// 1) 决策:仅当 enabled token 数为 0 时执行(防止重复 seed)
	n, err := s.TokenCountEnabled()
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

	// 3) 找/建默认 user "default"(id 复用,不重复创建)
	uid, err := getOrCreateDefaultUser(s)
	if err != nil {
		return 0, fmt.Errorf("get or create default user: %w", err)
	}

	// 4) 单事务逐个插入 token
	seeded := 0
	for _, key := range authKeys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		hash := store.HashKey(key)
		prefix := store.KeyPrefix(key)
		t := store.Token{
			UserID:    uid,
			Name:      "seeded",
			KeyHash:   hash,
			KeyPrefix: prefix,
			Unlimited: true, // 3a 不扣费,seed 出来的全 unlimited(决策 #4 注释)
			Status:    1,
		}
		if _, err := s.TokenInsert(t); err != nil {
			// UNIQUE 冲突说明 hash 已存在(并发或异常),跳过
			if errors.Is(err, store.ErrDuplicate) {
				continue
			}
			return seeded, fmt.Errorf("insert token (prefix=%s): %w", prefix, err)
		}
		seeded++
	}
	return seeded, nil
}

// getOrCreateDefaultUser 拿 id=1 的"default" user;不存在则建。
// 用硬编码 name="default"(决策 A)。
func getOrCreateDefaultUser(s *store.Store) (int64, error) {
	u, err := s.UserGetByName("default")
	if err == nil {
		return u.ID, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	// 不存在 → 建
	return s.UserInsert(store.User{
		Name:   "default",
		Status: 1,
	})
}
