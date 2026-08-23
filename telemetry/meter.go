package telemetry

import "net/http"

// Meter 是 telemetry 的高层入口,提供 Counter / Gauge / Histogram 的构造方法。
// 启动时调用 NewMeter() 得到默认实现,测试时可换成 NoopMeter。
//
// 设计:抽象成 interface 是为了在测试或禁用观测时能无侵入替换实现;
// 真正的注册逻辑全部委托给内部 *Registry。
type Meter interface {
	Handler() http.Handler
	Registry() *Registry
	NewCounter(name, help string, labelNames ...string) *Counter
	NewGauge(name, help string, labelNames ...string) *Gauge
	NewHistogram(name, help string, buckets []float64, labelNames ...string) *Histogram
}

// RealMeter 是 Meter 的默认实现,内部维护一个 *Registry。
type RealMeter struct {
	reg *Registry
}

// NewMeter 构造默认 Meter 实现。
func NewMeter() Meter {
	return &RealMeter{reg: newRegistry()}
}

func (m *RealMeter) Handler() http.Handler { return m.reg.Handler() }

// Registry 暴露给特殊用例(如测试断言),生产代码不应使用。
func (m *RealMeter) Registry() *Registry { return m.reg }

// NewCounter 注册并返回一个 Counter。
//   - name 指标名(Prometheus 风格,如 "tts_request_total")
//   - help 帮助文本
//   - labelNames 注册时锁定的 label key 集合,运行期不可变
func (m *RealMeter) NewCounter(name, help string, labelNames ...string) *Counter {
	c := newCounter(name, help, labelNames)
	if err := m.reg.register(name, c); err != nil {
		// 注册重名是启动期 bug,直接 panic 让问题在启动时暴露。
		panic(err)
	}
	return c
}

func (m *RealMeter) NewGauge(name, help string, labelNames ...string) *Gauge {
	g := newGauge(name, help, labelNames)
	if err := m.reg.register(name, g); err != nil {
		panic(err)
	}
	return g
}

func (m *RealMeter) NewHistogram(name, help string, buckets []float64, labelNames ...string) *Histogram {
	h := newHistogram(name, help, buckets, labelNames)
	if err := m.reg.register(name, h); err != nil {
		panic(err)
	}
	return h
}
