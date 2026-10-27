// Package store 封装请求持久化、原始报文存储与统计查询。
package store

import (
	"strings"
	"time"

	"llama_proxy/internal/db"
	"llama_proxy/internal/model"
)

func (st *Store) GetStats(f model.RequestFilter) (map[string]any, error) {
	from := f.TimeFrom.UTC()
	to := f.TimeTo.UTC()
	secs := to.Sub(from).Seconds()
	if secs <= 0 {
		secs = 1
	}

	var streamSumSQL = `COALESCE(SUM(is_streaming),0)`
	if st.isPostgres {
		streamSumSQL = `COALESCE(SUM(CASE WHEN is_streaming THEN 1 ELSE 0 END),0)`
	}

	var totalRequests int64
	var promptTokens int64
	var cachedTokens int64
	var completionTokens int64
	var totalTokens int64
	var lifetimeRequests int64
	var lifetimeTokens int64
	var lifetimePromptTokens int64
	var lifetimeCachedTokens int64
	var lifetimeCompletionTokens int64
	var reqBytes int64
	var respBytes int64
	var avgPromptMs float64
	var avgCompletionMs float64
	var avgTotalMs float64
	var avgFirstByteMs float64
	var errorsCount int64
	var streamCount int64

	query := `SELECT
		COUNT(*),
		COALESCE(SUM(prompt_tokens),0),
		COALESCE(SUM(cached_prompt_tokens),0),
		COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(total_tokens),0),
		COALESCE(SUM(request_bytes),0),
		COALESCE(SUM(response_bytes),0),
		COALESCE(AVG(prompt_ms),0),
		COALESCE(AVG(completion_ms),0),
		COALESCE(AVG(total_ms),0),
		COALESCE(AVG(first_byte_ms),0),
		COALESCE(SUM(CASE WHEN status_code >= 400 OR error_text != '' THEN 1 ELSE 0 END),0),
		` + streamSumSQL + `
		FROM requests WHERE 1=1`
	args := []any{}
	query, args = appendRequestFilterSQL(query, args, f, st.isPostgres)

	row := st.db.Raw(query, args...).Row()
	if err := row.Scan(
		&totalRequests,
		&promptTokens,
		&cachedTokens,
		&completionTokens,
		&totalTokens,
		&reqBytes,
		&respBytes,
		&avgPromptMs,
		&avgCompletionMs,
		&avgTotalMs,
		&avgFirstByteMs,
		&errorsCount,
		&streamCount,
	); err != nil {
		return nil, err
	}
	row = st.db.Raw(`SELECT
		COUNT(*),
		COALESCE(SUM(prompt_tokens),0),
		COALESCE(SUM(cached_prompt_tokens),0),
		COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(total_tokens),0)
		FROM requests`).Row()
	if err := row.Scan(&lifetimeRequests, &lifetimePromptTokens, &lifetimeCachedTokens,
		&lifetimeCompletionTokens, &lifetimeTokens); err != nil {
		return nil, err
	}

	cacheHitPct := 0.0
	if promptTokens > 0 {
		cacheHitPct = float64(cachedTokens) / float64(promptTokens) * 100
	}

	rpm := float64(totalRequests) / (secs / 60)
	promptTokensPerSec := float64(promptTokens) / secs
	decodeTokensPerSec := float64(completionTokens) / secs
	tokensPerSec := float64(totalTokens) / secs
	errorRate := 0.0
	if totalRequests > 0 {
		errorRate = float64(errorsCount) / float64(totalRequests)
	}

	return map[string]any{
		"hours":                      secs / 3600,
		"active_connections":         st.activeCount(),
		"total_requests":             totalRequests,
		"total_prompt_tokens":        promptTokens,
		"total_cached_tokens":        cachedTokens,
		"total_input_tokens":         promptTokens - cachedTokens,
		"total_completion_tokens":    completionTokens,
		"total_tokens":               totalTokens,
		"cache_hit_pct":              cacheHitPct,
		"matching_total_requests":    totalRequests,
		"matching_total_tokens":      totalTokens,
		"lifetime_total_requests":    lifetimeRequests,
		"lifetime_prompt_tokens":     lifetimePromptTokens,
		"lifetime_cached_tokens":     lifetimeCachedTokens,
		"lifetime_completion_tokens": lifetimeCompletionTokens,
		"lifetime_total_tokens":      lifetimeTokens,
		"lifetime_cache_hit_pct":     lifetimeCacheHitPct(lifetimePromptTokens, lifetimeCachedTokens),
		"token_by_model":             st.tokenBreakdownByModel(f),
		"total_request_bytes":        reqBytes,
		"total_response_bytes":       respBytes,
		"avg_prompt_ms":              avgPromptMs,
		"avg_completion_ms":          avgCompletionMs,
		"avg_total_ms":               avgTotalMs,
		"avg_first_byte_ms":          avgFirstByteMs,
		"requests_per_minute":        rpm,
		"prompt_tokens_per_second":   promptTokensPerSec,
		"decode_tokens_per_second":   decodeTokensPerSec,
		"tokens_per_second":          tokensPerSec,
		"errors_count":               errorsCount,
		"error_rate":                 errorRate,
		"streaming_requests":         streamCount,
	}, nil
}

func (st *Store) GetModels() ([]string, error) {
	var query string
	if st.isPostgres {
		query = `SELECT DISTINCT model FROM (
			SELECT model FROM requests WHERE model IS NOT NULL AND TRIM(model) != ''
			ORDER BY lower(model) ASC
		) t`
	} else {
		query = `SELECT DISTINCT model
		FROM requests
		WHERE model IS NOT NULL AND TRIM(model) != ''
		ORDER BY model COLLATE NOCASE ASC`
	}
	return scanDistinctStrings(st.db, query, 64)
}

// scanDistinctStrings 执行单列查询并返回去除空白后的非空字符串列表。
func scanDistinctStrings(database db.Database, query string, hint int) ([]string, error) {
	var raw []string
	if err := database.Raw(query).Scan(&raw).Error; err != nil {
		return nil, err
	}
	items := make([]string, 0, hint)
	for _, v := range raw {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		items = append(items, v)
	}
	return items, nil
}

func (st *Store) GetStatsByBackend(f model.RequestFilter) ([]map[string]any, error) {
	from := f.TimeFrom.UTC()
	to := f.TimeTo.UTC()

	var streamSumSQL = `COALESCE(SUM(is_streaming),0)`
	if st.isPostgres {
		streamSumSQL = `COALESCE(SUM(CASE WHEN is_streaming THEN 1 ELSE 0 END),0)`
	}

	query := `SELECT
		backend_url,
		COUNT(*),
		COALESCE(SUM(prompt_tokens),0),
		COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(total_tokens),0),
		COALESCE(AVG(total_ms),0),
		COALESCE(AVG(first_byte_ms),0),
		COALESCE(SUM(CASE WHEN status_code >= 400 OR error_text != '' THEN 1 ELSE 0 END),0),
		` + streamSumSQL + `
		FROM requests
		WHERE created_at >= ? AND created_at <= ? AND backend_url IS NOT NULL AND TRIM(backend_url) != ''
		GROUP BY backend_url
		ORDER BY COUNT(*) DESC`

	rows, err := st.db.Raw(query, filterTimeArg(from, st.isPostgres), filterTimeArg(to, st.isPostgres)).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]map[string]any, 0, 16)
	for rows.Next() {
		var backend string
		var count int64
		var promptTokens int64
		var completionTokens int64
		var totalTokens int64
		var avgTotalMs float64
		var avgFirstByteMs float64
		var errorsCount int64
		var streamCount int64
		if err := rows.Scan(&backend, &count, &promptTokens, &completionTokens, &totalTokens,
			&avgTotalMs, &avgFirstByteMs, &errorsCount, &streamCount); err != nil {
			return nil, err
		}
		errorRate := 0.0
		if count > 0 {
			errorRate = float64(errorsCount) / float64(count)
		}
		out = append(out, map[string]any{
			"backend_url":        backend,
			"requests":           count,
			"prompt_tokens":      promptTokens,
			"completion_tokens":  completionTokens,
			"total_tokens":       totalTokens,
			"avg_total_ms":       avgTotalMs,
			"avg_first_byte_ms":  avgFirstByteMs,
			"errors_count":       errorsCount,
			"error_rate":         errorRate,
			"streaming_requests": streamCount,
		})
	}
	return out, rows.Err()
}

func (st *Store) GetDailyStats(days int, f model.RequestFilter) ([]map[string]any, error) {
	if days <= 0 {
		days = 30
	}
	if days > 365 {
		days = 365
	}

	// 存储层 created_at 为 UTC（Postgres 为 TIMESTAMPTZ，SQLite 为 UTC 墙壁时间）。
	// 按 +8 时区分组日期，使 08:00 UTC 之前的请求归属到本地日期，避免跨天错位。
	dateExpr := `date(created_at, '+8 hours')`
	if st.isPostgres {
		dateExpr = `to_char(created_at AT TIME ZONE 'Asia/Shanghai', 'YYYY-MM-DD')`
	}

	// 时间窗口：优先使用 filter 中的绝对时间，否则回退到"本地时区（+8）(days-1) 天前的零点"。
	loc := time.FixedZone("CST", 8*3600)
	localNow := time.Now().In(loc)
	if f.TimeFrom.IsZero() {
		localStart := time.Date(localNow.Year(), localNow.Month(), localNow.Day()-(days-1), 0, 0, 0, 0, loc)
		f.TimeFrom = localStart.UTC()
	}
	if f.TimeTo.IsZero() {
		f.TimeTo = time.Now().UTC()
	}

	query := `SELECT
		` + dateExpr + `,
		COUNT(*),
		COALESCE(SUM(prompt_tokens),0),
		COALESCE(SUM(cached_prompt_tokens),0),
		COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(total_tokens),0),
		COALESCE(SUM(CASE WHEN status_code = 200 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status_code >= 400 AND status_code < 500 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status_code >= 500 THEN 1 ELSE 0 END),0)
		FROM requests
		WHERE 1=1`
	args := []any{}
	query, args = appendRequestFilterSQL(query, args, f, st.isPostgres)
	query += `
		GROUP BY ` + dateExpr + `
		ORDER BY ` + dateExpr + ` ASC`

	rows, err := st.db.Raw(query, args...).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]map[string]any, 0, days)
	for rows.Next() {
		var date string
		var count, promptTok, cachedTok, completionTok, totalTok, okCount, err4xx, err5xx int64
		if err := rows.Scan(&date, &count, &promptTok, &cachedTok, &completionTok, &totalTok,
			&okCount, &err4xx, &err5xx); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"date":                 date,
			"total_requests":       count,
			"prompt_tokens":        promptTok,
			"cached_prompt_tokens": cachedTok,
			"completion_tokens":    completionTok,
			"total_tokens":         totalTok,
			"ok_requests":          okCount,
			"err4xx":               err4xx,
			"err5xx":               err5xx,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// lifetimeCacheHitPct 计算全生命周期的缓存命中率（百分比）。
func lifetimeCacheHitPct(prompt, cached int64) float64 {
	if prompt <= 0 {
		return 0
	}
	return float64(cached) / float64(prompt) * 100
}

// tokenBreakdownByModel 按模型聚合 token 构成（输入/缓存/输出/命中率）。
// 查询失败时返回空列表，不影响主统计返回。
func (st *Store) tokenBreakdownByModel(f model.RequestFilter) []map[string]any {
	query := `SELECT
		model,
		COALESCE(SUM(prompt_tokens),0),
		COALESCE(SUM(cached_prompt_tokens),0),
		COALESCE(SUM(completion_tokens),0)
		FROM requests
		WHERE model IS NOT NULL AND TRIM(model) != ''`
	args := []any{}
	query, args = appendRequestFilterSQL(query, args, f, st.isPostgres)
	query += ` GROUP BY model ORDER BY SUM(prompt_tokens) + SUM(completion_tokens) DESC`

	rows, err := st.db.Raw(query, args...).Rows()
	if err != nil {
		return []map[string]any{}
	}
	defer rows.Close()

	out := make([]map[string]any, 0, 8)
	for rows.Next() {
		var modelName string
		var promptTok, cachedTok, completionTok int64
		if err := rows.Scan(&modelName, &promptTok, &cachedTok, &completionTok); err != nil {
			return []map[string]any{}
		}
		hitPct := 0.0
		if promptTok > 0 {
			hitPct = float64(cachedTok) / float64(promptTok) * 100
		}
		out = append(out, map[string]any{
			"model":             modelName,
			"prompt_tokens":     promptTok,
			"cached_tokens":     cachedTok,
			"input_tokens":      promptTok - cachedTok,
			"completion_tokens": completionTok,
			"total_tokens":      promptTok + completionTok,
			"cache_hit_pct":     hitPct,
		})
	}
	return out
}

func (st *Store) GetBackends() ([]string, error) {
	return scanDistinctStrings(st.db, `SELECT DISTINCT backend_url
		FROM requests
		WHERE backend_url IS NOT NULL AND TRIM(backend_url) != ''
		ORDER BY backend_url ASC`, 16)
}
