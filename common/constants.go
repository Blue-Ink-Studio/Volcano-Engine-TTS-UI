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
	MaxResponseTimes      = 100
	MaxErrors             = 10
	MaxConcurrentRequests = 10
	CleanupInterval       = time.Hour
	MaxModelNameLength    = 64
	MaxRateLimiterEntries = 100000
)
