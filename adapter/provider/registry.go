package provider

import "sort"

// registry 保存所有已注册的上游适配器。
// 适配器在自己的 init() 里调用 Register;主干只通过 Get/Names 访问。
var registry = map[string]Provider{}

// Register 注册一个上游适配器。通常在适配器包的 init() 里调用。
// 重名注册会 panic —— 说明有重名 bug,应立即暴露。
func Register(p Provider) {
	name := p.Name()
	if _, ok := registry[name]; ok {
		panic("provider: duplicate registration: " + name)
	}
	registry[name] = p
}

// Get 按名字取已注册的上游适配器;不存在返回 false。
func Get(name string) (Provider, bool) {
	p, ok := registry[name]
	return p, ok
}

// Names 返回所有已注册适配器的名字(排序稳定),供 admin UI 下拉框使用。
func Names() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
