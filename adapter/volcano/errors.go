package volcano

import "github.com/volcano-tts/tts-api/adapter/provider"

// UpstreamError 是 provider.UpstreamError 的别名。
//
// 通用错误类型已上移到 provider 包,这样主干(controller)只需 import provider 即可
// 判断上游错误,不必依赖具体适配器。火山包内部沿用旧名,构造处零改动。
type UpstreamError = provider.UpstreamError
