package volcano

import "fmt"

// UpstreamError 表示火山 v3 返回的 业务错误(code != 0 且 != 20000000)或传输错误。
// 包含上游错误码,便于 telemetry 把它作为 label。
type UpstreamError struct {
	Code    int
	Message string
	Stage   string // "request"/"stream"/"http" - 出错阶段
	Wrapped error
}

func (e *UpstreamError) Error() string {
	if e.Wrapped != nil {
		return fmt.Sprintf("volcano %s: code=%d %s: %v", e.Stage, e.Code, e.Message, e.Wrapped)
	}
	return fmt.Sprintf("volcano %s: code=%d %s", e.Stage, e.Code, e.Message)
}

func (e *UpstreamError) Unwrap() error { return e.Wrapped }

// IsAuth 当上游返回认证/权限类错误时返回 true。
func (e *UpstreamError) IsAuth() bool {
	return e.Code == 45000000 || e.Code == 55000000 ||
		e.Code == 401 || e.Code == 403
}
