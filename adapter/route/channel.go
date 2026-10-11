// Package route 实现多渠道路由分发层(阶段 1)。
//
// 思路参考 QuantumNous/new-api 的 Channel + 路由选择,但按本项目
// "一渠道 = 一上游账号" 简化:
//   - Channel: 一个上游账号(Provider 名 + 凭据 + 可选 voice 白名单)
//   - Router:  按优先级 + 加权随机从 channels 里挑一个给 Synthesize
//   - 不做 HTTP 管理接口(阶段 2 才上),channels 启动期从 store 加载一次
package route

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/volcano-tts/tts-api/adapter/provider"
	"github.com/volcano-tts/tts-api/store"
)

// Channel 是 router 用的渠道视图,从 store.Channel 反序列化凭据而来。
//
// 持有 store.Channel 字符串字段(VoicesCSV / CredentialsJSON)会让 router
// 变成"半 DB 层",所以这里转成 router 友好的强类型:
//   - Voices []string: 白名单;空 = 不限
//   - Credentials: provider.Credentials 强类型
type Channel struct {
	ID          int64
	Name        string
	Provider    string            // 对应 provider.Registry 里的 Provider.Name()
	Credentials provider.Credentials
	Voices      []string          // 对外 voice 名白名单;空 = 不限
	Priority    int
	Weight      int
	Status      int               // 1=enabled 2=disabled
	AutoBan     bool
}

// FromStoreChannel 把 store.Channel 转成 router.Channel。
// 失败原因:
//   - credentials_json 不是合法 JSON → 返 error(让启动期 / Reload 直接报错,不要静默继续)
func FromStoreChannel(s store.Channel) (Channel, error) {
	c := Channel{
		ID:       s.ID,
		Name:     s.Name,
		Provider: s.Provider,
		Priority: s.Priority,
		Weight:   s.Weight,
		Status:   s.Status,
		AutoBan:  s.AutoBan,
		Voices:   splitCSV(s.Voices),
	}
	if err := json.Unmarshal([]byte(s.CredentialsJSON), &c.Credentials); err != nil {
		return Channel{}, fmt.Errorf("channel %d (%s): credentials_json invalid: %w", s.ID, s.Name, err)
	}
	return c, nil
}

// BuildRequest 拿一个上游请求模板(零 Channel 兜底用的),填入本渠道的
// 凭据,作为 Synthesize 的入参。
//
// 不复制 Text / Format / Speed 等请求级字段(由调用方按本次请求填),
// 这里只换"账号身份"——保证 router 调 Synthesize 时拿到的是"用本渠道账号调" 的 Request。
func (c Channel) BuildRequest(template provider.Request) provider.Request {
	template.Credentials = c.Credentials
	return template
}

// AcceptsVoice 检查本渠道是否支持该 voice:
//   - Voices 为空 → 不限,接受一切
//   - 非空 → 必须在白名单里
func (c Channel) AcceptsVoice(voice string) bool {
	if len(c.Voices) == 0 {
		return true
	}
	for _, v := range c.Voices {
		if v == voice {
			return true
		}
	}
	return false
}

// Enabled 是不是启用状态(Status=1)。
func (c Channel) Enabled() bool { return c.Status == 1 }

// splitCSV 拆 "a,b,c" 为 []string;容忍空 / 重复 / 空白。
func splitCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
