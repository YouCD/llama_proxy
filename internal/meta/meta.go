// Package meta 负责请求/响应元数据解析（OpenAI / llama.cpp / SSE / Prometheus）。
package meta

import (
	"bufio"
	"bytes"
	"encoding/json"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"llama_proxy/internal/model"
)

type LimitedBuffer struct {
	max int64
	buf bytes.Buffer
}

func NewLimitedBuffer(max int64) *LimitedBuffer {
	return &LimitedBuffer{max: max}
}

func (lb *LimitedBuffer) Write(p []byte) (int, error) {
	if lb.max <= 0 {
		return lb.buf.Write(p)
	}
	remaining := lb.max - int64(lb.buf.Len())
	if remaining <= 0 {
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		_, _ = lb.buf.Write(p[:remaining])
		return len(p), nil
	}
	_, _ = lb.buf.Write(p)
	return len(p), nil
}

func (lb *LimitedBuffer) Bytes() []byte {
	return lb.buf.Bytes()
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case float32:
		return int64(x)
	case int:
		return int64(x)
	case int64:
		return x
	case int32:
		return int64(x)
	case json.Number:
		i, _ := x.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(x, 10, 64)
		return i
	default:
		return 0
	}
}

func toFloat64(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case int32:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	default:
		return 0
	}
}

func ParseResponseMeta(headers http.Header, body []byte) model.ResponseMeta {
	var meta model.ResponseMeta
	ct := headers.Get("Content-Type")
	mediatype, _, _ := mime.ParseMediaType(ct)
	if strings.Contains(mediatype, "json") {
		return parseJSONResponseMeta(body)
	}
	if strings.Contains(mediatype, "text/event-stream") {
		parseSSEResponseMeta(body, &meta)
	}
	return meta
}

func parseJSONResponseMeta(body []byte) model.ResponseMeta {
	var meta model.ResponseMeta
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return meta
	}
	if v, ok := m["model"].(string); ok {
		meta.Model = v
	}

	if usage, ok := m["usage"].(map[string]any); ok {
		meta.PromptTokens = toInt64(usage["prompt_tokens"])
		meta.CompletionTok = toInt64(usage["completion_tokens"])
		meta.TotalTokens = toInt64(usage["total_tokens"])
		if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
			meta.CachedPromptTokens = toInt64(details["cached_tokens"])
		}
	}
	if timings, ok := m["timings"].(map[string]any); ok {
		if meta.PromptTokens == 0 {
			meta.PromptTokens = toInt64(timings["prompt_n"])
		}
		if meta.CompletionTok == 0 {
			meta.CompletionTok = toInt64(timings["predicted_n"])
		}
		meta.PromptMs = toFloat64(timings["prompt_ms"])
		meta.CompletionMs = toFloat64(timings["predicted_ms"])
	}
	if meta.PromptTokens == 0 {
		meta.PromptTokens = toInt64(m["tokens_evaluated"])
	}
	if meta.CompletionTok == 0 {
		meta.CompletionTok = toInt64(m["tokens_predicted"])
	}
	if meta.TotalTokens == 0 {
		meta.TotalTokens = meta.PromptTokens + meta.CompletionTok
	}
	if meta.PromptMs == 0 {
		meta.PromptMs = toFloat64(m["tokens_evaluated_ms"])
	}
	if meta.CompletionMs == 0 {
		meta.CompletionMs = toFloat64(m["tokens_predicted_ms"])
	}
	return meta
}

func mergeResponseMeta(dst *model.ResponseMeta, src model.ResponseMeta) {
	if src.Model != "" {
		dst.Model = src.Model
	}
	if src.PromptTokens > 0 {
		dst.PromptTokens = src.PromptTokens
	}
	if src.CachedPromptTokens > 0 {
		dst.CachedPromptTokens = src.CachedPromptTokens
	}
	if src.CompletionTok > 0 {
		dst.CompletionTok = src.CompletionTok
	}
	if src.TotalTokens > 0 {
		dst.TotalTokens = src.TotalTokens
	}
	if src.PromptMs > 0 {
		dst.PromptMs = src.PromptMs
	}
	if src.CompletionMs > 0 {
		dst.CompletionMs = src.CompletionMs
	}
}

func parseSSEResponseMeta(body []byte, meta *model.ResponseMeta) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		chunkMeta := parseJSONResponseMeta([]byte(payload))
		mergeResponseMeta(meta, chunkMeta)
	}
	if meta.TotalTokens == 0 {
		meta.TotalTokens = meta.PromptTokens + meta.CompletionTok
	}
}
func DetectRequestMeta(body []byte) (isStreaming bool, model string) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return false, ""
	}
	isStreaming, _ = m["stream"].(bool)
	if v, ok := m["model"].(string); ok {
		model = v
	}
	return
}

// DetectMaxTokens 提取请求体声明的目标 token 数（max_tokens，0 = 未声明），
// 供监控面板展示在途请求的“已生成/目标”进度。
func DetectMaxTokens(body []byte) int64 {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return 0
	}
	if v, ok := m["max_tokens"].(float64); ok && v > 0 {
		return int64(v)
	}
	return 0
}

// RewriteModel replaces the "model" field in a JSON request body with the
// backend's actual model ID. It returns the rewritten body and the new model
// name. If the body is not valid JSON (or has no model field), the original
// body is returned unchanged.
func RewriteModel(body []byte, model string) ([]byte, string, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body, "", err
	}
	m["model"] = model
	newBody, err := json.Marshal(m)
	if err != nil {
		return body, "", err
	}
	return newBody, model, nil
}

func ParsePrometheusText(text string) map[string]float64 {
	out := make(map[string]float64)
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := fields[0]
		value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			continue
		}
		out[name] = value
	}
	return out
}

// normalizeCacheHit 在历史数据未记录缓存命中率时按 token 数补齐。
func NormalizeCacheHit(rec *model.RequestRecord) {
	if rec.CacheHitPct == 0 && rec.PromptTokens > 0 && rec.CachedPromptTokens > 0 {
		rec.CacheHitPct = float64(rec.CachedPromptTokens) / float64(rec.PromptTokens) * 100
	}
}

func EnrichRequestRates(rec model.RequestRecord) model.RequestRecord {
	if rec.PromptMs > 0 {
		rec.PromptTokPerSec = float64(rec.PromptTokens) / (rec.PromptMs / 1000.0)
	}
	if rec.CompletionMs > 0 {
		rec.DecodeTokPerSec = float64(rec.CompletionTokens) / (rec.CompletionMs / 1000.0)
	}
	if rec.TotalMs > 0 {
		rec.TotalTokPerSec = float64(rec.TotalTokens) / (rec.TotalMs / 1000.0)
	}
	return rec
}
