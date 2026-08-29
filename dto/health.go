package dto

// HealthResponse 是 /health 端点的 JSON 响应。
// 数值类信息(请求统计、错误)迁移到 /metrics 端点,
// 这里只保留运行期最关键的状态。
type HealthResponse struct {
	Status       string                 `json:"status"`
	Service      string                 `json:"service"`
	Version      string                 `json:"version"`
	Commit       string                 `json:"commit"`
	Uptime       string                 `json:"uptime"`
	StartTime    string                 `json:"start_time"`
	Memory       map[string]interface{} `json:"memory"`
	ConfigStatus ConfigStatusResponse   `json:"config_status"`
	// M1 新增:反映 installer 模式,便于部署探针/运维识别未初始化状态
	Installed bool   `json:"installed"`
	Mode      string `json:"mode"`
}

type ConfigStatusResponse struct {
	AllRequiredVarsSet bool `json:"all_required_vars_set"`
	ConfigError        bool `json:"config_error"`
}
