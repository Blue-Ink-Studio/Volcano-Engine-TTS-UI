package telemetry

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
)

// collector 是 Counter / Gauge / Histogram 共同实现的内部接口。
type collector interface {
	collect(w io.Writer)
}

// Registry 持有已注册的全部指标,提供 Prometheus 文本格式导出。
type Registry struct {
	mu      sync.RWMutex
	entries map[string]collector
	order   []string // 保留注册顺序,使输出可预测
}

func newRegistry() *Registry {
	return &Registry{
		entries: make(map[string]collector),
	}
}

func (r *Registry) register(name string, c collector) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.entries[name]; exists {
		return fmt.Errorf("metric %q already registered", name)
	}
	r.entries[name] = c
	r.order = append(r.order, name)
	return nil
}

// Gather 把所有指标按注册顺序写入 w,文本格式遵循 Prometheus 0.0.4。
func (r *Registry) Gather(w io.Writer) error {
	r.mu.RLock()
	order := append([]string(nil), r.order...)
	defer r.mu.RUnlock()
	for _, name := range order {
		r.entries[name].collect(w)
	}
	return nil
}

// Handler 返回标准 Prometheus 抓取端点。
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = r.Gather(w)
	})
}

// 注册顺序的辅助,用于测试断言。
func (r *Registry) names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := append([]string(nil), r.order...)
	sort.Strings(out)
	return out
}
