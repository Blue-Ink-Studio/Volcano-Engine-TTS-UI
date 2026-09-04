// Package telemetry 提供进程内可观测能力:Counter / Gauge / Histogram,
// 以及 Prometheus 文本格式导出。
//
// 设计原则:
//   - 零外部依赖,只使用标准库;
//   - label key 在指标注册时锁定,运行期不可新增(避免 cardinality 爆炸);
//   - 所有并发安全由实现保证,调用方无需加锁;
//   - Meter 是高层入口,NoopMeter 用于测试。
package telemetry

import (
	"crypto/sha1"
	"encoding/hex"
	"sort"
	"strings"
)

// Labels 是指标附加的标签集合。Value 在序列化时会按 Prometheus 规范转义。
type Labels map[string]string

// SpeakerLabel 把 speaker ID 转成不可逆的稳定短哈希,作为指标 label。
// 目的:保护火山复刻音色资产(speaker ID 是用户付费 / 隐私敏感);
// 同时仍能按 voice 聚合观测(同 speaker → 同 label)。
//
// 算法: sha1(s)[:8] = 32 bits 空间;典型 <100 个 voice 场景无碰撞风险。
// 空串返回 "unknown",避免 /metrics label 出现空值 (Prometheus 禁止空 label)。
//
// 注意: 这是**不可逆**哈希,不是加密;不可用于需要还原原始 speaker 的场景。
// Admin UI 想要看原名时,通过 /api/voices 拿 name 字段对照。
func SpeakerLabel(s string) string {
	if s == "" {
		return "unknown"
	}
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// MaskSpeaker 把 speaker ID 部分打码用于日志输出。
//   - 空 → "***"
//   - 长度 ≤ 4 → 全打码
//   - 其它 → 前 4 + **** + 后 4 (保留前缀便于肉眼区分 "S_xx 开头" vs "BV001_...")
// 例子: "S_G8tEKnaJ1" → "S_G8****naJ1"
func MaskSpeaker(s string) string {
	if s == "" {
		return "(未设置)"
	}
	if len(s) <= 4 {
		return strings.Repeat("*", len(s))
	}
	// 找前 4 字符中第一个非 [A-Za-z0-9_] 字符做截断,避免截到奇怪位置
	// (虽然火山 ID 实际都是 S_xxx 字母数字组合,这里保险)
	prefix := s[:4]
	suffix := s[len(s)-4:]
	return prefix + "****" + suffix
}

// labelKey 计算一组标签的稳定 key,用于在内部 map 中唯一定位 child。
// 缺失或多余的 label 一律视为空串,以保证 child 数量与 label 名集合一致。
func labelKey(names []string, labels Labels) string {
	if len(names) == 0 {
		return ""
	}
	parts := make([]string, 0, len(names)*2)
	for _, n := range names {
		parts = append(parts, n, labels[n])
	}
	return joinLabelParts(parts)
}

func joinLabelParts(parts []string) string {
	out := make([]byte, 0, 16*len(parts))
	for i, p := range parts {
		if i > 0 {
			out = append(out, 0)
		}
		out = append(out, p...)
	}
	return string(out)
}

// sortedKeys 返回按字典序排列的 key,用于导出时输出稳定顺序。
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
