package telemetry

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
)

// Gauge 可增可减的瞬时值。
type Gauge struct {
	metricName string
	help       string
	labelNames []string

	mu     sync.RWMutex
	values map[string]*gaugeChild
}

type gaugeChild struct {
	labels Labels
	bits   atomic.Uint64
}

func newGauge(name, help string, labelNames []string) *Gauge {
	return &Gauge{
		metricName: name,
		help:       help,
		labelNames: append([]string(nil), labelNames...),
		values:     make(map[string]*gaugeChild),
	}
}

// Set 直接设置当前值。
func (g *Gauge) Set(v float64, labels Labels) {
	child := g.getOrCreate(labels)
	child.bits.Store(float64bits(v))
}

// Inc +1。
func (g *Gauge) Inc(labels Labels) { g.Add(1, labels) }

// Dec -1。
func (g *Gauge) Dec(labels Labels) { g.Add(-1, labels) }

// Add 累加 v(可负)。
func (g *Gauge) Add(v float64, labels Labels) {
	child := g.getOrCreate(labels)
	for {
		bits := child.bits.Load()
		cur := float64frombits(bits)
		next := float64bits(cur + v)
		if child.bits.CompareAndSwap(bits, next) {
			return
		}
	}
}

func (g *Gauge) getOrCreate(labels Labels) *gaugeChild {
	key := labelKey(g.labelNames, labels)
	g.mu.RLock()
	if c, ok := g.values[key]; ok {
		g.mu.RUnlock()
		return c
	}
	g.mu.RUnlock()

	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.values[key]; ok {
		return c
	}
	c := &gaugeChild{labels: copyLabels(labels, g.labelNames)}
	g.values[key] = c
	return c
}

func (g *Gauge) collect(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n", g.metricName, g.help)
	fmt.Fprintf(w, "# TYPE %s gauge\n", g.metricName)

	g.mu.RLock()
	keys := make([]string, 0, len(g.values))
	for k := range g.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	defer g.mu.RUnlock()

	for _, k := range keys {
		child := g.values[k]
		val := float64frombits(child.bits.Load())
		writeMetricLine(w, g.metricName, child.labels, val)
	}
}
