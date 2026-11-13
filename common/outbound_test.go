package common

import (
	"net"
	"testing"
)

// 本文件覆盖 STAGE6 的出站 URL 安全校验(审计 P1-5,SSRF 防护)。
//
// 威胁:管理员(或持有 admin_key 的人)把渠道的 base_url 指向内网地址,
// 服务器就会替他去访问内网资源 —— 云元数据(169.254.169.254)、
// 内网服务、数据库等。

// TestValidateBaseURLRejectsInternalTargets 是这张表的核心断言。
func TestValidateBaseURLRejectsInternalTargets(t *testing.T) {
	bad := []struct {
		name string
		url  string
	}{
		{"回环 IP", "http://127.0.0.1:8080"},
		{"回环 IP(别名)", "http://127.0.0.1/"},
		{"回环 IPv6", "http://[::1]:9000"},
		{"私有 10/8", "http://10.0.0.5/admin"},
		{"私有 172.16/12", "http://172.16.3.4/"},
		{"私有 192.168/16", "http://192.168.1.1/"},
		{"链路本地(元数据)", "http://169.254.169.254/latest/meta-data/"},
		{"IPv4-mapped 回环", "http://[::ffff:127.0.0.1]/"},
		{"未指定地址", "http://0.0.0.0:8080/"},
		{"非 http scheme", "file:///etc/passwd"},
		{"gopher scheme", "gopher://127.0.0.1:11211/"},
		{"缺 host", "http://"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ValidateBaseURL(c.url); err == nil {
				t.Fatalf("URL %q 应被拒绝(指向内网/非法 scheme),却通过了", c.url)
			}
		})
	}
}

// TestValidateBaseURLAcceptsPublicTargets 确认正常地址不被误伤。
func TestValidateBaseURLAcceptsPublicTargets(t *testing.T) {
	good := []string{
		"https://api.openai.com",
		"https://api.openai.com/v1",
		"http://example.com:8080/base",
		"https://8.8.8.8/x",
	}
	for _, u := range good {
		if _, err := ValidateBaseURL(u); err != nil {
			t.Errorf("URL %q 应被接受,却被拒: %v", u, err)
		}
	}
}

// TestIsBlockedIP 单独覆盖 IP 判定(含 IPv4-mapped 这个易漏的形态)。
func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "172.31.255.255",
		"192.168.0.1", "169.254.169.254", "fe80::1", "fc00::1",
		"0.0.0.0", "255.255.255.255", "224.0.0.1", "::ffff:127.0.0.1",
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("测试数据 %q 不是合法 IP", s)
		}
		if !IsBlockedIP(ip) {
			t.Errorf("IP %s 应被判定为禁止访问", s)
		}
	}

	allowed := []string{"8.8.8.8", "1.1.1.1", "172.32.0.1", "192.169.0.1", "2606:4700::1111"}
	for _, s := range allowed {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("测试数据 %q 不是合法 IP", s)
		}
		if IsBlockedIP(ip) {
			t.Errorf("IP %s 不应被判定为禁止访问", s)
		}
	}
}

// TestIsDNSFailure 验证"DNS 解析失败"能被识别出来(调用方据此放行内网自建上游)。
func TestIsDNSFailure(t *testing.T) {
	// 这个域名几乎不可能解析成功(且不在我们的白名单里)
	_, err := ValidateBaseURL("http://this-host-should-not-resolve-9d8f7a6b.invalid")
	if err == nil {
		t.Skip("该域名意外解析成功了,跳过")
	}
	if !IsDNSFailure(err) {
		t.Fatalf("DNS 解析失败应被识别为 DNS failure,实际 err=%v", err)
	}
}
