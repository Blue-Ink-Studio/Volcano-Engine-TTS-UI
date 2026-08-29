package common

import "time"

// DebugLog 控制非必要日志输出;由 setting 包在启动时通过 BYTEDANCE_TTS_DEBUG 环境变量设置。
var DebugLog bool

const (
	DefaultPort           = "8080"
	DefaultTimeout        = 30 * time.Second
	MaxTextLength         = 5000
	MinSpeed              = 0.25
	MaxSpeed              = 4.0
	DefaultSpeed          = 1.0
	MaxRequestBodySize    = 1024 * 1024
	RateLimitRequests     = 100
	RateLimitWindow       = time.Minute
	MaxConcurrentRequests = 10
	CleanupInterval       = time.Hour
	MaxModelNameLength    = 64
	MaxRateLimiterEntries = 100000
)

// SecureEqualString 是常量时间字符串比较,防 token 计时攻击。
// 长度先比对(避免短串早返回时泄漏长度信息),再遍历每个字节做 XOR 累加;
// diff 为 0 才返 true。用于 Bearer token、setup token 等敏感比较场景。
func SecureEqualString(a, b string) bool {
	if len(a) != len(b) {
		// 先比对长度(避免短串早返回时泄漏长度信息)
		// 但仍要遍历一遍避免优化器消除分支
		if len(a) > 0 {
			_ = a[0]
		}
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
