package dto

// HealthResponse 是 /health 端点的 JSON 响应。
// 数值类信息(请求统计、错误)迁移到 /metrics 端点,
// 这里只保留运行期最关键的状态。
type HealthResponse struct {
	Status       string                 `json:"status"`
	Service      string                 `json:"service"`
	Version      string                 `json:"version"`
	Uptime       string                 `json:"uptime"`
	StartTime    string                 `json:"start_time"`
	Memory       map[string]interface{} `json:"memory"`
	ConfigStatus ConfigStatusResponse   `json:"config_status"`
}

type ConfigStatusResponse struct {
	AllRequiredVarsSet bool `json:"all_required_vars_set"`
	ConfigError        bool `json:"config_error"`
}
