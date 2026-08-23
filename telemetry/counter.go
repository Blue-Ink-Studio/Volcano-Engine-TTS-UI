package telemetry

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
)

// Counter 单调递增的累计指标(整数语义,内部用 float64 位以 atomic 操作)。
type Counter struct {
	metricName string
	help       string
	labelNames []string

	mu     sync.RWMutex
	values map[string]*counterChild // key = labelKey(...)
}

type counterChild struct {
	labels Labels
	bits   atomic.Uint64 // float64
}

func newCounter(name, help string, labelNames []string) *Counter {
	return &Counter{
		metricName: name,
		help:       help,
		labelNames: append([]string(nil), labelNames...),
		values:     make(map[string]*counterChild),
	}
}

// Inc 计数 +1。
func (c *Counter) Inc(labels Labels) { c.Add(1, labels) }

// Add 累加 v(v 必须 >= 0)。
func (c *Counter) Add(v float64, labels Labels) {
	if v < 0 {
		return
	}
	child := c.getOrCreate(labels)
	for {
		bits := child.bits.Load()
		cur := float64frombits(bits)
		next := float64bits(cur + v)
		if child.bits.CompareAndSwap(bits, next) {
			return
		}
	}
}

func (c *Counter) getOrCreate(labels Labels) *counterChild {
	key := labelKey(c.labelNames, labels)
	c.mu.RLock()
	if child, ok := c.values[key]; ok {
		c.mu.RUnlock()
		return child
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if child, ok := c.values[key]; ok {
		return child
	}
	child := &counterChild{labels: copyLabels(labels, c.labelNames)}
	c.values[key] = child
	return child
}

func (c *Counter) collect(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n", c.metricName, c.help)
	fmt.Fprintf(w, "# TYPE %s counter\n", c.metricName)

	c.mu.RLock()
	keys := make([]string, 0, len(c.values))
	for k := range c.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	defer c.mu.RUnlock()

	for _, k := range keys {
		child := c.values[k]
		val := float64frombits(child.bits.Load())
		writeMetricLine(w, c.metricName, child.labels, val)
	}
}
