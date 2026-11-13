// Package common 提供跨包共享的小工具(常量、WAV 封装、出站 URL 校验等)。
//
// 本文件实现**出站请求的目标地址安全校验**(STAGE6 P1-5,SSRF 防护)。
//
// 为什么需要它:
// 渠道的 `credentials.scope.base_url` 由管理员填写,adapter 会直接拿它发 HTTP
// 请求。若不校验,管理员(或有 admin_key 的人)可以把 base_url 指向
// 127.0.0.1 / 10.x / 169.254.169.254(云元数据)等内网地址,
// 让服务器替他去访问内网资源 —— 这就是 SSRF。
//
// 威胁模型说明:这条**需要管理员权限**才能触发,所以风险等级低于"未认证可利用",
// 但它仍值得修 —— admin_key 是应急后门,持有者不应因此获得内网探测能力。
//
// 校验分两层(都在这里做):
//  1. **字面检查**:scheme 必须是 http/https;host 不能是回环/私网/链路本地
//     的**字面 IP**(如 127.0.0.1、10.0.0.1、169.254.169.254、[::1])。
//  2. **解析后检查**:host 是域名时,**解析 DNS** 得到 IP 再判断。
//     这一层是关键 —— `http://evil.example.com` 可以解析到 127.0.0.1
//     (DNS rebinding),只看字面 IP 会漏。
//
// ⚠️ 已知局限(DNS rebinding 的时间窗):
// 校验时解析一次、真正请求时又解析一次,两次之间 DNS 可能变化
// (攻击者控制 TTL 很短的域名)。彻底解决需要"把校验时的 IP 钉进连接"
// (自定义 DialContext),本期不做 —— 那是更大的改动,且对本项目
// (私有部署、管理员配渠道)的威胁模型收益有限。
// 校验能挡住的是绝大多数真实场景:内网字面地址、元数据地址、
// 以及指向内网的常规域名。
package common

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ErrUnsafeURL 表示目标地址未通过安全校验。
type ErrUnsafeURL struct {
	URL    string
	Reason string
}

func (e *ErrUnsafeURL) Error() string {
	return fmt.Sprintf("outbound: unsafe url %q: %s", e.URL, e.Reason)
}

// ValidateBaseURL 校验一个出站 base_url 是否安全。
//
// 规则:
//   - 必须能解析成绝对 URL;
//   - scheme 只能是 http 或 https;
//   - host 不能为空;
//   - host 是 IP 字面量 → 直接判定(内网/回环/元数据一律拒绝);
//   - host 是域名 → 解析 DNS,若任一条 A/AAAA 落在内网/回环/元数据 → 拒绝;
//     DNS 解析失败**不**直接拒绝(可能是内网 DNS 或临时抖动),
//     但会要求调用方显式放行(见 AllowUnresolvedDNS)。
//
// 返回值:通过则返回规范化后的 URL(去掉末尾斜杠),否则返回 *ErrUnsafeURL。
func ValidateBaseURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, &ErrUnsafeURL{URL: raw, Reason: "empty url"}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, &ErrUnsafeURL{URL: raw, Reason: "parse failed: " + err.Error()}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, &ErrUnsafeURL{URL: raw, Reason: "scheme must be http or https"}
	}
	host := u.Hostname()
	if host == "" {
		return nil, &ErrUnsafeURL{URL: raw, Reason: "missing host"}
	}

	// IP 字面量:直接判定
	if ip := net.ParseIP(host); ip != nil {
		if IsBlockedIP(ip) {
			return nil, &ErrUnsafeURL{URL: raw, Reason: "target is a private/loopback/link-local address"}
		}
		return normalize(u), nil
	}

	// 域名:解析后判定(防 DNS 指向内网)
	ips, err := net.LookupIP(host)
	if err != nil {
		// 解析失败:交由调用方决定是否放行(见 AllowUnresolvedDNS 的说明)
		return normalize(u), &ErrUnsafeURL{URL: raw, Reason: "dns resolve failed: " + err.Error()}
	}
	for _, ip := range ips {
		if IsBlockedIP(ip) {
			return nil, &ErrUnsafeURL{URL: raw, Reason: "host resolves to a private/loopback/link-local address"}
		}
	}
	return normalize(u), nil
}

// AllowUnresolvedDNS 说明"域名解析失败"时的处理口径:
//
// ValidateBaseURL 在 DNS 解析失败时返回的 error 里带 ErrUnsafeURL,
// 但**调用方可以选择放行** —— 因为:
//   - 内网自建上游常用内网 DNS,解析失败不代表恶意;
//   - 真正该拦的是"解析到内网 IP",那一步已经拦住了。
//
// 因此约定:调用方收到 ErrUnsafeURL 时,若 reason 以 "dns resolve failed" 开头,
// 可按"未解析"放行(仍保留 scheme 与字面 IP 的检查);
// 其余 reason 一律拒绝。
func IsDNSFailure(err error) bool {
	var e *ErrUnsafeURL
	if !asErrUnsafeURL(err, &e) {
		return false
	}
	return strings.HasPrefix(e.Reason, "dns resolve failed")
}

// asErrUnsafeURL 是 errors.As 的手写版(避免为此引入 errors 包的小函数)。
func asErrUnsafeURL(err error, target **ErrUnsafeURL) bool {
	for err != nil {
		if e, ok := err.(*ErrUnsafeURL); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// normalize 去掉末尾斜杠并返回 URL。
func normalize(u *url.URL) *url.URL {
	u.Path = strings.TrimRight(u.Path, "/")
	return u
}

// IsBlockedIP 判定一个 IP 是否属于"不应由服务器主动访问"的地址段。
//
// 覆盖:
//   - 回环(127.0.0.0/8、::1)
//   - 私有网段(10/8、172.16/12、192.168/16、fc00::/7)
//   - 链路本地(169.254/16、fe80::/10)—— **含云元数据 169.254.169.254**
//   - 组播与保留(224/4、240/4、255.255.255.255)
//   - 未指定地址(0.0.0.0、::)
//   - IPv4-mapped IPv6(::ffff:127.0.0.1)—— 转成 IPv4 再判
func IsBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// IPv4-mapped / IPv4-in-IPv6:统一转成 4 字节再判,否则 ::ffff:127.0.0.1 会漏网
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	// 240.0.0.0/4(含 255.255.255.255)
	if v4 := ip.To4(); v4 != nil && v4[0] >= 240 {
		return true
	}
	return false
}
