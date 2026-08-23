// Package telemetry 提供进程内可观测能力:Counter / Gauge / Histogram,
// 以及 Prometheus 文本格式导出。
//
// 设计原则:
//   - 零外部依赖,只使用标准库;
//   - label key 在指标注册时锁定,运行期不可新增(避免 cardinality 爆炸);
//   - 所有并发安全由实现保证,调用方无需加锁;
//   - Meter 是高层入口,NoopMeter 用于测试。
package telemetry

import "sort"

// Labels 是指标附加的标签集合。Value 在序列化时会按 Prometheus 规范转义。
type Labels map[string]string

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
