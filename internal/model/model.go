// Package model 定义请求记录、过滤条件与响应元数据等共享领域模型。
package model

import "time"

type RequestRecord struct {
	ID                 string    `json:"id"`
	CreatedAt          time.Time `json:"created_at"`
	Method             string    `json:"method"`
	Path               string    `json:"path"`
	Query              string    `json:"query"`
	ClientIP           string    `json:"client_ip"`
	BackendURL         string    `json:"backend_url"`
	Model              string    `json:"model"`
	IsStreaming        bool      `json:"is_streaming"`
	StatusCode         int       `json:"status_code"`
	ErrorText          string    `json:"error_text"`
	RequestBytes       int64     `json:"request_bytes"`
	ResponseBytes      int64     `json:"response_bytes"`
	PromptTokens       int64     `json:"prompt_tokens"`
	CachedPromptTokens int64     `json:"cached_prompt_tokens"`
	CacheHitPct        float64   `json:"cache_hit_pct"`
	CompletionTokens   int64     `json:"completion_tokens"`
	TotalTokens        int64     `json:"total_tokens"`
	PromptMs           float64   `json:"prompt_ms"`
	CompletionMs       float64   `json:"completion_ms"`
	TotalMs            float64   `json:"total_ms"`
	FirstByteMs        float64   `json:"first_byte_ms"`
	ChunksCount        int64     `json:"chunks_count"`
	RequestRawPath     string    `json:"request_raw_path"`
	ResponseRawPath    string    `json:"response_raw_path"`
	UserAgent          string    `json:"user_agent"`
	PromptTokPerSec    float64   `json:"prompt_tok_per_sec" gorm:"-"`
	DecodeTokPerSec    float64   `json:"decode_tok_per_sec" gorm:"-"`
	TotalTokPerSec     float64   `json:"total_tok_per_sec" gorm:"-"`
}

// TableName 固定 GORM 模型对应的表名（避免默认复数化）。
func (RequestRecord) TableName() string {
	return "requests"
}

type RequestFilter struct {
	Path       string
	Model      string
	Method     string
	Backend    string
	ClientIP   string
	UserAgent  string
	Search     string
	StatusCode int
	// StatusClass 状态码段过滤："4xx"（400–499）或 "5xx"（≥500），空串表示不限。
	// 与 StatusCode 互斥使用，二者同时非空时 StatusCode 生效。
	StatusClass         string
	TimeFrom            time.Time // 开始时间（含），零值表示不限
	TimeTo              time.Time // 结束时间（含），零值表示不限
	Streaming           *bool
	ErrorsOnly          bool
	WithTokens          bool
	ChatCompletionsOnly bool
}

// ResponseMeta 为从后端响应中解析出的元数据（token、模型、时延）。
type ResponseMeta struct {
	Model              string
	PromptTokens       int64
	CachedPromptTokens int64
	CompletionTok      int64
	TotalTokens        int64
	PromptMs           float64
	CompletionMs       float64
}
