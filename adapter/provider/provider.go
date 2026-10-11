// Package provider 定义上游 TTS 服务的统一抽象。
//
// 设计要点(与 docs/UPSTREAM_ADAPTER_GUIDE.md §3 对齐):
//   - 主干(controller/setting/metrics)只依赖本包,不依赖任何具体适配器;
//   - 具体适配器(如 adapter/volcano)实现 Provider 并在自己的 init() 里注册;
//   - 新增上游 = 写一个新适配器 + main.go 加一行空导入,主干零改动。
package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/volcano-tts/tts-api/dto"
)

// Provider 是一个上游 TTS 服务的统一抽象。
// 实现者只需要关心:把我的能力说清楚、把一次合成做完。
type Provider interface {
	// Name 稳定标识,用于配置键、库表字段、metrics label。
	// 约定全小写无空格:volcano / openai / azure / aliyun / gpt-sovits。
	Name() string

	// Capabilities 声明本适配器支持什么。
	// 主干据此做校验与降级,不靠 if-else 猜实现。
	Capabilities() Capabilities

	// Synthesize 执行一次合成,阻塞到完成。
	// 必须遵守:ctx 取消即返回;上游不支持 clientFormat 时按 Capabilities 降级,
	// 并在 Result.Format 里如实回报真实格式。
	Synthesize(ctx context.Context, req Request, mtr MetricsRecorder) (*dto.SynthesisResult, error)
}

// Capabilities 声明适配器支持的能力,主干据此做校验与降级。
type Capabilities struct {
	// 上游原生能产出的格式;客户端要的格式不在其中时,主干负责降级或拒绝。
	Formats []string
	// 是否支持用倍率控制语速。不支持时主干忽略 speed 而不是报错。
	Speed bool
	// 是否按字符计费并回传用量(决定 UpstreamUsage 是否有意义)。
	Usage bool
	// 是否支持多音色路由。不支持时该 provider 只能配一个默认音色。
	Voices bool
	// 是否需要资源/部署/项目 ID 这类额外维度(火山需要,OpenAI 不需要)。
	ExtraScopes bool
}

// Request 是主干交给适配器的、已归一化的合成请求。
// 注意:这里刻意不出现任何厂商专属字段。
type Request struct {
	Text         string
	VoiceKey     string            // voices 表的主键语义(对外 voice 名或上游音色 ID)
	Model        string
	Format       string            // 客户端期望格式(主干已归一化:mp3/wav/pcm/ogg_opus)
	SampleRate   int
	Speed        float64           // 1.0 = 原速
	Language     string
	Extra        map[string]string // 厂商专属配置,从 voices/settings 的 JSON 列读

	// Credentials 由 provider 自己解释:
	// 火山读 api_key + resource_id;OpenAI 只读 api_key。
	Credentials Credentials
}

// Credentials 承载上游鉴权与路由维度。
// Scope 是厂商额外凭证/路由维度:火山 = resource_id;Azure = region;自建 = base_url。
// 本包不解释 Scope 的内容,原样交给对应 provider。
type Credentials struct {
	APIKey string
	Scope  map[string]string
}

// MetricsRecorder 是适配器向上报告埋点的接口。
// 适配器本身不依赖 telemetry 包,controller 在 main 启动时把 Meter 适配成实现;
// 这样测试可以注入 mock,生产可以无侵入替换成 OTel。
type MetricsRecorder interface {
	UpstreamStarted(speaker, model, format string)
	UpstreamFinished(speaker, model, format, status string, duration, ttfb time.Duration, chunks, audioBytes, errCode int)
	UpstreamUsage(model string, textWords int)
}

// nopMetrics 是 MetricsRecorder 的 no-op 默认值。
type nopMetrics struct{}

func (nopMetrics) UpstreamStarted(string, string, string)        {}
func (nopMetrics) UpstreamFinished(string, string, string, string, time.Duration, time.Duration, int, int, int) {
}
func (nopMetrics) UpstreamUsage(string, int) {}

// NoopMetrics 返回一个 MetricsRecorder 的 no-op 实现。
// 适配器在未注入 recorder 时可把它作为默认值,保证埋点逻辑永远可运行。
func NoopMetrics() MetricsRecorder { return nopMetrics{} }

// UpstreamError 表示上游 TTS 返回的业务错误或传输错误。
// 各适配器用它报告失败,并带上错误发生的阶段(Stage),供主干归一错误与打 metrics label。
// 通用化到本包,是为了让主干(controller)只依赖 provider 包即可判断上游错误,
// 不必 import 具体适配器。
type UpstreamError struct {
	Code    int // 上游业务/HTTP 错误码;0 表示传输/本地错误
	Message string
	Stage   string // "request"/"http"/"stream"/"wrap" - 出错阶段
	Wrapped error
}

func (e *UpstreamError) Error() string {
	if e.Wrapped != nil {
		return fmt.Sprintf("upstream %s: code=%d %s: %v", e.Stage, e.Code, e.Message, e.Wrapped)
	}
	return fmt.Sprintf("upstream %s: code=%d %s", e.Stage, e.Code, e.Message)
}

func (e *UpstreamError) Unwrap() error { return e.Wrapped }

// IsAuth 当上游返回认证/权限类错误时返回 true(HTTP 401/403)。
func (e *UpstreamError) IsAuth() bool {
	return e.Code == 401 || e.Code == 403
}
