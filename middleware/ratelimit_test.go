package middleware

import (
	"net/http/httptest"
	"testing"
)

// TestGetClientIP 覆盖 XFF 解析在两种模式下的关键场景。
// 表驱动测试,每个 case 独立设置 trustedProxyHops,验证 GetClientIP 输出。
func TestGetClientIP(t *testing.T) {
	tests := []struct {
		name       string
		mode       int    // 0=启发式, N>0=精确 N 跳
		remoteAddr string // 直连 IP:port
		xff        string // X-Forwarded-For 头(空则不设)
		xri        string // X-Real-IP 头(空则不设)
		want       string
	}{
		// === 直出部署(directIP 是公网,XFF 分支不进)===
		{
			name:       "直出_无XFF",
			mode:       0,
			remoteAddr: "1.2.3.4:5678",
			want:       "1.2.3.4",
		},
		{
			name:       "直出_XFF被忽略",
			mode:       0,
			remoteAddr: "1.2.3.4:5678",
			xff:        "fake",
			want:       "1.2.3.4", // 公网直连不走 XFF 分支
		},
		{
			name:       "直出_精确模式也不走XFF",
			mode:       2,
			remoteAddr: "1.2.3.4:5678",
			xff:        "fake, 5.6.7.8",
			want:       "1.2.3.4",
		},

		// === 单跳反代 ===
		{
			name:       "单跳_启发式",
			mode:       0,
			remoteAddr: "10.0.0.1:5678",
			xff:        "1.2.3.4",
			want:       "1.2.3.4",
		},
		{
			name:       "单跳_精确N1",
			mode:       1,
			remoteAddr: "10.0.0.1:5678",
			xff:        "1.2.3.4",
			want:       "1.2.3.4",
		},

		// === 攻击者伪造首值 ===
		{
			name:       "伪造_启发式跳过fake",
			mode:       0,
			remoteAddr: "10.0.0.1:5678",
			xff:        "fake, 1.2.3.4",
			want:       "1.2.3.4",
		},
		{
			name:       "伪造_精确N1也跳过fake",
			mode:       1,
			remoteAddr: "10.0.0.1:5678",
			xff:        "fake, 1.2.3.4",
			want:       "1.2.3.4", // target=1, 跳过 fake 取 real
		},
		{
			name:       "伪造_多个假值前缀",
			mode:       0,
			remoteAddr: "10.0.0.1:5678",
			xff:        "fake1, fake2, 1.2.3.4",
			want:       "1.2.3.4", // 从尾扫,只看最后一个
		},

		// === 多跳 CDN+nginx ===
		{
			name:       "多跳_启发式返回CDN边缘",
			mode:       0,
			remoteAddr: "10.0.0.1:5678",
			xff:        "1.2.3.4, 203.0.113.5",
			want:       "203.0.113.5", // 链尾公网=CDN 边缘
		},
		{
			name:       "多跳_精确N2返回真实client",
			mode:       2,
			remoteAddr: "10.0.0.1:5678",
			xff:        "1.2.3.4, 203.0.113.5",
			want:       "1.2.3.4", // 倒数第2=真实 client
		},
		{
			name:       "多跳_精确N1不够穿透",
			mode:       1,
			remoteAddr: "10.0.0.1:5678",
			xff:        "1.2.3.4, 203.0.113.5",
			want:       "203.0.113.5", // 数到 nginx,没穿透到 client
		},

		// === 链尾私有 IP ===
		{
			name:       "链尾私有_启发式跳过",
			mode:       0,
			remoteAddr: "10.0.0.1:5678",
			xff:        "1.2.3.4, 10.0.0.1",
			want:       "1.2.3.4", // 跳过私有取公网
		},
		{
			name:       "链尾私有_精确N1取末值",
			mode:       1,
			remoteAddr: "10.0.0.1:5678",
			xff:        "1.2.3.4, 10.0.0.1",
			want:       "10.0.0.1", // 精确模式不跳私有
		},

		// === X-Real-IP 兜底 ===
		{
			name:       "无XFF_走XRI",
			mode:       0,
			remoteAddr: "10.0.0.1:5678",
			xri:        "1.2.3.4",
			want:       "1.2.3.4",
		},
		{
			name:       "XFF全非法_走XRI",
			mode:       0,
			remoteAddr: "10.0.0.1:5678",
			xff:        "not_ip, also_not",
			xri:        "1.2.3.4",
			want:       "1.2.3.4",
		},
		{
			name:       "XRI被XFF优先_但XFF全非法",
			mode:       0,
			remoteAddr: "10.0.0.1:5678",
			xff:        "not_an_ip",
			xri:        "1.2.3.4",
			want:       "1.2.3.4",
		},

		// === 全部私有 IP(启发式无解)===
		{
			name:       "全私有_启发式回退directIP",
			mode:       0,
			remoteAddr: "10.0.0.1:5678",
			xff:        "192.168.1.1, 172.16.0.1",
			want:       "10.0.0.1", // 全跳私有,走 directIP
		},

		// === 畸形/空 XFF ===
		{
			name:       "畸形XFF_启发式跳过畸形",
			mode:       0,
			remoteAddr: "10.0.0.1:5678",
			xff:        "not_an_ip, 1.2.3.4",
			want:       "1.2.3.4",
		},
		{
			name:       "全空XFF_回退directIP",
			mode:       0,
			remoteAddr: "10.0.0.1:5678",
			xff:        "  ,  ,  ",
			want:       "10.0.0.1",
		},
		{
			name:       "XFF带前后空格",
			mode:       0,
			remoteAddr: "10.0.0.1:5678",
			xff:        "  1.2.3.4  ,  5.6.7.8  ",
			want:       "5.6.7.8", // TrimSpace 处理
		},

		// === 精确模式 N 超出链长 ===
		{
			name:       "精确N超出链长_回退到首值",
			mode:       5,
			remoteAddr: "10.0.0.1:5678",
			xff:        "1.2.3.4",
			want:       "1.2.3.4", // target<0 保护,取首个合法
		},
		{
			name:       "精确N等于链长_取首值",
			mode:       1,
			remoteAddr: "10.0.0.1:5678",
			xff:        "1.2.3.4",
			want:       "1.2.3.4", // target=0
		},
		{
			name:       "精确N大于链长_取首值",
			mode:       2,
			remoteAddr: "10.0.0.1:5678",
			xff:        "1.2.3.4",
			want:       "1.2.3.4", // target<0,fall back
		},

		// === 精确模式链中含畸形 ===
		{
			name:       "精确N1_链中畸形回退到首值",
			mode:       1,
			remoteAddr: "10.0.0.1:5678",
			xff:        "1.2.3.4, not_ip",
			want:       "1.2.3.4", // target=1(not_ip 失败)→ i=0(1.2.3.4 成功)
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trustedProxyHops = tt.mode
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			if tt.xri != "" {
				r.Header.Set("X-Real-IP", tt.xri)
			}
			got := GetClientIP(r)
			if got != tt.want {
				t.Errorf("GetClientIP() = %q, want %q", got, tt.want)
			}
		})
	}

	// 重置为默认,避免影响其他测试或运行时行为
	trustedProxyHops = 0
}
