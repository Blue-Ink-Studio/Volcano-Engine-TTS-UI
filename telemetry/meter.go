package telemetry

import "net/http"

// Meter 是 telemetry 的高层入口。所有业务埋点都通过它创建指标;
// 测试时可替换为 NoopMeter 或带缓冲的自定义实现。
type Meter struct {
	reg *Registry
}

// NewMeter 创建默认实现。
func NewMeter() *Meter {
	return &Meter{reg: newRegistry()}
}

// Handler 返回 /metrics 端点的 http.Handler。
func (m *Meter) Handler() http.Handler { return m.reg.Handler() }

// Registry 暴露给特殊用例(如测试断言),生产代码不应使用。
func (m *Meter) Registry() *Registry { return m.reg }

// NewCounter 注册并返回一个 Counter。
//   - name 指标名(Prometheus 风格,如 "tts_request_total")
//   - help 帮助文本
//   - labelNames 注册时锁定的 label key 集合,运行期不可变
func (m *Meter) NewCounter(name, help string, labelNames ...string) *Counter {
	c := newCounter(name, help, labelNames)
	if err := m.reg.register(name, c); err != nil {
		// 注册重名是启动期 bug,直接 panic 让问题在启动时暴露。
		panic(err)
	}
	return c
}

// NewGauge 同 NewCounter。
func (m *Meter) NewGauge(name, help string, labelNames ...string) *Gauge {
	g := newGauge(name, help, labelNames)
	if err := m.reg.register(name, g); err != nil {
		panic(err)
	}
	return g
}

// NewHistogram 同 NewCounter,额外接受桶上界(不含 +Inf,+Inf 由实现自动追加)。
func (m *Meter) NewHistogram(name, help string, buckets []float64, labelNames ...string) *Histogram {
	h := newHistogram(name, help, buckets, labelNames)
	if err := m.reg.register(name, h); err != nil {
		panic(err)
	}
	return h
}
