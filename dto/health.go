package dto

// HealthResponse 是 /health 端点的 JSON 响应。
// 数值类信息(请求统计、错误)迁移到 /metrics 端点,
// 这里只保留运行期最关键的状态。
//
// STAGE6 删除字段 `config_status`(G9):
// 它只承载"全局 TTS 必填项(api_key / default_resource_id / default_speaker)
// 是否齐备",而 STAGE5 起 setup 不再写这三项(凭证由渠道自带),
// 渠道化之后"全局 TTS 配置"这个概念本身就不存在了。
// 已确认前端 11 个页面**零引用** config_status / config_error,故直接移除。
type HealthResponse struct {
	Status    string                 `json:"status"`
	Service   string                 `json:"service"`
	Version   string                 `json:"version"`
	Commit    string                 `json:"commit"`
	Uptime    string                 `json:"uptime"`
	StartTime string                 `json:"start_time"`
	Memory    map[string]interface{} `json:"memory"`
	// M1 新增:反映 installer 模式,便于部署探针/运维识别未初始化状态
	Installed bool   `json:"installed"`
	Mode      string `json:"mode"`
}
