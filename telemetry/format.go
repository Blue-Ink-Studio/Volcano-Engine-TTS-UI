package telemetry

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// copyLabels 返回只包含 labelNames 中声明的 key 的副本,缺失补空串。
// 这样序列化时输出顺序和数量固定。
func copyLabels(labels Labels, names []string) Labels {
	if len(names) == 0 {
		return Labels{}
	}
	out := make(Labels, len(names))
	for _, n := range names {
		out[n] = labels[n]
	}
	return out
}

func mergeLabels(a, b Labels) Labels {
	out := make(Labels, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// formatLabels 序列化为 `{k1="v1",k2="v2"}`;空集合返回空字符串。
// value 内的 `\`, `"`, 换行会按 Prometheus 规范转义。
func formatLabels(labels Labels) string {
	if len(labels) == 0 {
		return ""
	}
	keys := sortedKeys(labels)
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(k)
		sb.WriteString(`="`)
		sb.WriteString(escapeLabelValue(labels[k]))
		sb.WriteByte('"')
	}
	sb.WriteByte('}')
	return sb.String()
}

func escapeLabelValue(v string) string {
	if !strings.ContainsAny(v, "\\\"\n") {
		return v
	}
	var sb strings.Builder
	sb.Grow(len(v) + 2)
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '\\':
			sb.WriteString(`\\`)
		case '"':
			sb.WriteString(`\"`)
		case '\n':
			sb.WriteString(`\n`)
		default:
			sb.WriteByte(v[i])
		}
	}
	return sb.String()
}

func writeMetricLine(w io.Writer, name string, labels Labels, value float64) {
	fmt.Fprintf(w, "%s%s %s\n", name, formatLabels(labels), formatFloat(value))
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// float64 bits 互转,封装到独立文件避免重复。
func float64bits(f float64) uint64     { return math.Float64bits(f) }
func float64frombits(b uint64) float64 { return math.Float64frombits(b) }
