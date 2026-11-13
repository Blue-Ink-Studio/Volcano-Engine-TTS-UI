package volcano

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	"github.com/volcano-tts/tts-api/common"
)

// HTTPClient 持有共享的 http.Client 以便复用连接(v3 keep-alive 1 分钟)。
type HTTPClient struct {
	client *http.Client
}

// NewHTTPClient 构造默认配置的 HTTPClient。
//
// STAGE6(P2-7):Transport 参数已下沉到 common.NewUpstreamHTTPClient(),
// 三个 adapter 共用同一份,避免以后再漂移。
func NewHTTPClient() *HTTPClient {
	return &HTTPClient{
		client: common.NewUpstreamHTTPClient(),
	}
}

// PostStream 发送一次流式请求,返回带上下文的 *http.Response。
// 调用方负责关闭 resp.Body。
//
// 空接收者(nil *HTTPClient)安全:返回错误而不是 panic。未初始化的 client 属于
// 装配错误,应当让该次请求以"上游不可用"失败(controller 归一成 5xx 并记日志),
// 而不是让整个进程崩掉。
func (h *HTTPClient) PostStream(ctx context.Context, url string, headers map[string]string, body []byte) (*http.Response, error) {
	if h == nil || h.client == nil {
		return nil, fmt.Errorf("volcano: HTTPClient 未初始化,无法发起上游请求")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return h.client.Do(req)
}
