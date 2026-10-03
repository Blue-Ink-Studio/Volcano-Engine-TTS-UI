package middleware

import (
	"log"
	"net"
	"net/http"
	"os"
	"strings"
)

// metricsAllowList 是 METRICS_ALLOW_CIDR 解析出的内网白名单。
// 启动期由 InitMetricsAllowList 填充;运行期只读,无并发写。
var metricsAllowList []*net.IPNet

// metricsAllowConfigured 表示是否配置了非空的 METRICS_ALLOW_CIDR。
// 决定根路径 /metrics、/health 是否注册(未配置则完全不注册,访问 404)。
var metricsAllowConfigured bool

// InitMetricsAllowList 解析 METRICS_ALLOW_CIDR,启动期调用一次。
//
// 格式:逗号分隔的 CIDR 列表,例如 "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"。
// 也接受裸 IP(自动补 /32 或 /128),方便写 "127.0.0.1"。
//
// 为什么需要它:根路径 /metrics、/health 是给 Prometheus 抓取 / K8s 探针用的机器端点,
// 让它们带 Bearer 会强制改抓取配置;用内网白名单更省事,且默认不暴露。
//
// 【重要】Docker 环境不要填 127.0.0.1/32 —— 那是容器自身的回环,
// Prometheus 在宿主机或另一个容器里根本进不来。请填容器内网网段(如 172.16.0.0/12)。
func InitMetricsAllowList() {
	raw := strings.TrimSpace(os.Getenv("METRICS_ALLOW_CIDR"))
	metricsAllowList = nil
	metricsAllowConfigured = false
	if raw == "" {
		return
	}

	for _, part := range strings.Split(raw, ",") {
		entry := strings.TrimSpace(part)
		if entry == "" {
			continue
		}
		// 裸 IP 自动补全掩码
		if !strings.Contains(entry, "/") {
			if ip := net.ParseIP(entry); ip != nil {
				if ip.To4() != nil {
					entry += "/32"
				} else {
					entry += "/128"
				}
			}
		}
		_, ipnet, err := net.ParseCIDR(entry)
		if err != nil {
			log.Printf("[metrics-ip] 忽略非法 CIDR 条目 %q: %v", part, err)
			continue
		}
		metricsAllowList = append(metricsAllowList, ipnet)
	}
	metricsAllowConfigured = len(metricsAllowList) > 0
	if metricsAllowConfigured {
		log.Printf("[metrics-ip] METRICS_ALLOW_CIDR 已启用,根路径 /metrics 仅对 %d 个网段开放", len(metricsAllowList))
	} else {
		log.Printf("[metrics-ip] METRICS_ALLOW_CIDR 无有效条目,根路径 /metrics 将不注册")
	}
}

// MetricsAllowListConfigured 报告是否配置了有效的白名单网段。
// router 据此决定是否注册根路径 /metrics / /health。
func MetricsAllowListConfigured() bool { return metricsAllowConfigured }

// MetricsIPAllowList 只放行来源 IP 命中白名单的请求。
//
// 未命中返回 **404**(而不是 403):不向扫描者确认"这里存在一个只是你没权限的端点"。
// 客户端 IP 取自 GetClientIP,它已处理 X-Forwarded-For 与 TRUSTED_PROXY_HOPS,
// 所以反代后面的真实来源也能正确判定。
func MetricsIPAllowList(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ipAllowed(GetClientIP(r)) {
			// 不打印每个被拒请求,避免扫描流量刷爆日志;只按需在 debug 下输出
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ipAllowed 判定客户端 IP 是否命中任一白名单网段。
// 空 IP(解析失败)一律拒绝 —— 宁可拒绝也不能误放行。
func ipAllowed(clientIP string) bool {
	if clientIP == "" {
		return false
	}
	ip := net.ParseIP(strings.TrimSpace(clientIP))
	if ip == nil {
		return false
	}
	for _, n := range metricsAllowList {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
