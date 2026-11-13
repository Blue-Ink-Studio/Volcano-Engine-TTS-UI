package common

import (
	"net/http"
	"time"
)

// NewUpstreamHTTPClient 构造发往上游的 http.Client(STAGE6 P2-7)。
//
// 为什么要有这个函数:之前只有 adapter/volcano 配了完整的 Transport
// (连接池 + TLS 握手超时),而 adapter/openai 与 adapter/azure 是裸
// `&http.Client{}` —— 用的是 http.DefaultTransport,既没有连接池参数,
// 也没有 TLS 握手超时。后果:
//   - 高频请求时每个请求都要新建 TCP+TLS 连接(无 keep-alive 复用);
//   - 对端不响应 TLS 握手时会一直挂着,直到 ctx 超时(默认 30s)才释放连接。
//
// 抽到 common 是为了让三个 adapter 用**同一份**参数,避免以后再漂移。
//
// ⚠️ Timeout 刻意**不设**:整体超时由调用方的 ctx 控制
// (controller 用 setting.GetTTSTimeout()),这样"上游慢"能按业务语义
// 统一处理;这里只管连接层(握手、空闲连接)。
func NewUpstreamHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			// Expect: 100-continue 会多一次往返,对 TTS 这种一次性请求不划算。
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}
