package middleware

import (
	"log"
	"net/http"
	"strings"
	"time"
)

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (rec *statusRecorder) WriteHeader(code int) {
	rec.statusCode = code
	rec.ResponseWriter.WriteHeader(code)
}

func Logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rec, r)
		duration := time.Since(start)

		// r.RequestURI 是未经解析的原始请求行,攻击者可在 URL 中注入
		// \n / \r 伪造日志行。转义为可见字符后再记录。
		uri := strings.NewReplacer("\n", "\\n", "\r", "\\r").Replace(r.RequestURI)
		log.Printf("%s %s %s %d %v", r.Method, uri, r.RemoteAddr, rec.statusCode, duration)
	})
}
