package telemetry

import "net/http"

// NoopMeter 是一个不采集、不输出的 Meter,用于单元测试或禁用观测的场景。
// 返回的 Counter / Gauge / Histogram 实例不会被注册到任何 Registry,
// 它们的 Inc/Add/Observe 调用在本进程内没有可见效果(每次返回新的空实例)。
//
// 实现 Meter 接口。
type NoopMeter struct{}

func (NoopMeter) NewCounter(string, string, ...string) *Counter {
	return newCounter("", "", nil)
}
func (NoopMeter) NewGauge(string, string, ...string) *Gauge {
	return newGauge("", "", nil)
}
func (NoopMeter) NewHistogram(string, string, []float64, ...string) *Histogram {
	return newHistogram("", "", nil, nil)
}
func (NoopMeter) Handler() http.Handler { return http.NotFoundHandler() }
func (NoopMeter) Registry() *Registry   { return nil }
