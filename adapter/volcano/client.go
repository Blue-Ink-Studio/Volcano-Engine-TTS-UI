package volcano

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"time"
)

// HTTPClient 持有共享的 http.Client 以便复用连接(v3 keep-alive 1 分钟)。
type HTTPClient struct {
	client *http.Client
}

// NewHTTPClient 构造默认配置的 HTTPClient。
func NewHTTPClient() *HTTPClient {
	return &HTTPClient{
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		},
	}
}

// PostStream 发送一次流式请求,返回带上下文的 *http.Response。
// 调用方负责关闭 resp.Body。
func (h *HTTPClient) PostStream(ctx context.Context, url string, headers map[string]string, body []byte) (*http.Response, error) {
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
