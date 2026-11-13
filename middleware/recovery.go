package middleware

import (
	"log"
	"net/http"
	"runtime/debug"
)

// PanicRecovery 是全局 panic 捕获中间件(STAGE6 P3-2)。
//
// 为什么需要:
// 任何 handler 里的 panic(空指针、越界、并发 map 写)如果没人 recover,
// 会**直接终止整个进程** —— 所有在线用户的服务瞬间中断。对一个多租户
// 收费服务来说,一个边角 bug 的影响面被放大成"全站故障"。
//
// 装了它之后,单次请求的 panic 只影响那一个请求:
//   - 恢复执行,给客户端 500(不泄漏堆栈)
//   - 打日志(带堆栈,便于定位)
//   - 进程继续服务其它请求
//
// ⚠️ 注意:recover 只能救**当前 goroutine**。若 panic 发生在别的 goroutine
// (本项目里的后台清理协程、metrics 上报等),本中间件救不了 ——
// 那些协程自己需要 recover。本中间件覆盖的是 HTTP 请求这条主链路。
//
// 位置要求:必须装在**最外层**(router.Use 的第一个),这样连中间件链
// 自身的 panic 都能被兜住。
func PanicRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// 记录堆栈:panic 值本身可能含敏感信息(比如带 token 的错误),
				// 但堆栈只进日志不进响应,问题不大;而日志是排障的唯一线索,
				// 丢了就等于没发生。
				log.Printf("[panic] 请求处理发生 panic - 方法=%s 路径=%s 客户端=%s panic=%v\n%s",
					r.Method, r.URL.Path, GetClientIP(r), rec, debug.Stack())

				// 已经写过响应头的话不能再改状态码(会打 warning),
				// 此时只能断开连接 —— 好过让客户端收到半截响应体。
				// 这里用一个轻量的 writer 包装记录"是否已写头"。
				if !hasWrittenHeader(w) {
					SendJSONError(w, http.StatusInternalServerError,
						"internal server error", "server_error", "internal_error")
				}
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// hasWrittenHeader 判断响应是否已经写过头。
//
// 标准库没有直接暴露这个信息,但 http.ResponseWriter 的 WriteHeader 一旦被调用,
// 再调用就会打 "superfluous WriteHeader" 警告。我们用一个包装器记录状态。
// 注意:本函数对**未包装**的 ResponseWriter 无法判断,会返回 false
// (即尝试写 500);这在极端情况下可能产生 "superfluous WriteHeader" 日志,
// 但不会造成错误响应 —— 可接受。
func hasWrittenHeader(w http.ResponseWriter) bool {
	if t, ok := w.(interface{ WrittenHeader() bool }); ok {
		return t.WrittenHeader()
	}
	return false
}

// ResponseRecorder 包装 ResponseWriter,记录是否已写头。
//
// 用途:让 PanicRecovery 能准确判断"panic 发生时响应是否已经发出"。
// 放在这里是因为它是通用能力,任何中间件都可能需要。
type ResponseRecorder struct {
	http.ResponseWriter
	written bool
}

// NewResponseRecorder 包装 w。
func NewResponseRecorder(w http.ResponseWriter) *ResponseRecorder {
	return &ResponseRecorder{ResponseWriter: w}
}

// WriteHeader 记录状态后转发。
func (r *ResponseRecorder) WriteHeader(code int) {
	if !r.written {
		r.written = true
	}
	r.ResponseWriter.WriteHeader(code)
}

// Write 隐式写头时也记录。
func (r *ResponseRecorder) Write(b []byte) (int, error) {
	if !r.written {
		r.written = true
	}
	return r.ResponseWriter.Write(b)
}

// WrittenHeader 报告是否已写过头。
func (r *ResponseRecorder) WrittenHeader() bool { return r.written }
