package dto

type HealthResponse struct {
	Status       string                 `json:"status"`
	Service      string                 `json:"service"`
	Version      string                 `json:"version"`
	Uptime       string                 `json:"uptime"`
	StartTime    string                 `json:"start_time"`
	Memory       map[string]interface{} `json:"memory"`
	APIStats     APIStatsResponse       `json:"api_stats"`
	Errors       ErrorResponse          `json:"errors"`
	ConfigStatus ConfigStatusResponse   `json:"config_status"`
}

type APIStatsResponse struct {
	TotalRequests         int       `json:"total_requests"`
	SuccessfulRequests    int64     `json:"successful_requests"`
	FailedRequests        int64     `json:"failed_requests"`
	ErrorRatePercent      string    `json:"error_rate_percent"`
	AvgResponseTimeMs     string    `json:"avg_response_time_ms"`
	RecentResponseTimesMs []float64 `json:"recent_response_times_ms"`
}

type ErrorResponse struct {
	RecentErrorsCount int `json:"recent_errors_count"`
}

type ConfigStatusResponse struct {
	AllRequiredVarsSet bool `json:"all_required_vars_set"`
	ConfigError        bool `json:"config_error"`
}
