package telemetry

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
)

// DefaultLatencyBuckets 适合 HTTP/TTS 场景的默认桶(秒)。
var DefaultLatencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// Histogram 累计分布型指标,记录观测值的分布。
//
// 内部为每个 child 维护:
//   - buckets[i] 累计计数(<= le_i 的观测数,不含 +Inf 桶)
//   - count 全部观测计数
//   - sum 全部观测值之和
type Histogram struct {
	metricName string
	help       string
	labelNames []string
	buckets    []float64 // 用户声明的上界,不含 +Inf

	mu     sync.RWMutex
	values map[string]*histChild
}

type histChild struct {
	labels  Labels
	buckets []atomic.Uint64 // 累计计数
	count   atomic.Uint64
	sumBits atomic.Uint64 // float64
}

func newHistogram(name, help string, buckets []float64, labelNames []string) *Histogram {
	bs := append([]float64(nil), buckets...)
	sort.Float64s(bs)
	return &Histogram{
		metricName: name,
		help:       help,
		labelNames: append([]string(nil), labelNames...),
		buckets:    bs,
		values:     make(map[string]*histChild),
	}
}

// Observe 记录一个观测值。
func (h *Histogram) Observe(v float64, labels Labels) {
	child := h.getOrCreate(labels)
	for {
		bits := child.sumBits.Load()
		cur := float64frombits(bits)
		next := float64bits(cur + v)
		if child.sumBits.CompareAndSwap(bits, next) {
			break
		}
	}
	child.count.Add(1)
	for i, le := range h.buckets {
		if v <= le {
			child.buckets[i].Add(1)
		}
	}
}

func (h *Histogram) getOrCreate(labels Labels) *histChild {
	key := labelKey(h.labelNames, labels)
	h.mu.RLock()
	if c, ok := h.values[key]; ok {
		h.mu.RUnlock()
		return c
	}
	h.mu.RUnlock()

	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.values[key]; ok {
		return c
	}
	c := &histChild{
		labels:  copyLabels(labels, h.labelNames),
		buckets: make([]atomic.Uint64, len(h.buckets)),
	}
	h.values[key] = c
	return c
}

func (h *Histogram) collect(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n", h.metricName, h.help)
	fmt.Fprintf(w, "# TYPE %s histogram\n", h.metricName)

	h.mu.RLock()
	keys := make([]string, 0, len(h.values))
	for k := range h.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	defer h.mu.RUnlock()

	for _, k := range keys {
		child := h.values[k]
		for i, le := range h.buckets {
			merged := mergeLabels(child.labels, Labels{"le": formatFloat(le)})
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.metricName, formatLabels(merged), child.buckets[i].Load())
		}
		merged := mergeLabels(child.labels, Labels{"le": "+Inf"})
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.metricName, formatLabels(merged), child.count.Load())
		sum := float64frombits(child.sumBits.Load())
		fmt.Fprintf(w, "%s_sum%s %s\n", h.metricName, formatLabels(child.labels), formatFloat(sum))
		fmt.Fprintf(w, "%s_count%s %d\n", h.metricName, formatLabels(child.labels), child.count.Load())
	}
}
