// Package store 封装请求持久化、原始报文存储与统计查询。
package store

import (
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"llama_proxy/internal/db"
	"llama_proxy/internal/meta"
	"llama_proxy/internal/model"

	"github.com/youcd/toolkit/log"
	"gorm.io/gorm"
)

// Store 封装 GORM 句柄与数据目录上的持久化/统计逻辑。
type Store struct {
	db      db.Database
	dataDir string
	// retentionDays 数据保留天数，atomic 支持配置热更新（SetRetentionDays）。
	retentionDays atomic.Int32
	isPostgres    bool

	// Active 返回当前在途请求数（用于统计的 active_connections）；nil 按 0 计。
	Active func() int64
}

// New 基于数据库句柄与数据目录创建 Store。
func New(database db.Database, dataDir string, retentionDays int) *Store {
	st := &Store{
		db:         database,
		dataDir:    dataDir,
		isPostgres: db.IsPostgresDB(database),
	}
	st.retentionDays.Store(int32(retentionDays))
	return st
}

// SetRetentionDays 热更新数据保留天数（Cleanup 下一次执行时生效）。
func (st *Store) SetRetentionDays(days int) {
	st.retentionDays.Store(int32(days))
}

// activeCount 返回在途请求数（Active 未注入时按 0 计）。
func (st *Store) activeCount() int64 {
	if st.Active != nil {
		return st.Active()
	}
	return 0
}

func (st *Store) SaveRawPayload(requestID string, kind string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	dateDir := time.Now().UTC().Format("2006-01-02")
	relDir := filepath.Join("raw", dateDir)
	fullDir := filepath.Join(st.dataDir, relDir)
	if err := os.MkdirAll(fullDir, 0o755); err != nil {
		return "", err
	}
	fileName := fmt.Sprintf("%s-%s.gz", requestID, kind)
	relPath := filepath.Join(relDir, fileName)
	fullPath := filepath.Join(st.dataDir, relPath)

	f, err := os.Create(fullPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	if _, err := gw.Write(data); err != nil {
		_ = gw.Close()
		return "", err
	}
	if err := gw.Close(); err != nil {
		return "", err
	}
	return relPath, nil
}

func ReadGzipFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gr.Close()
	return io.ReadAll(gr)
}
func (st *Store) Normalize() error {
	db := st.db
	return db.Exec(`UPDATE requests SET
		query = COALESCE(query, ''),
		client_ip = COALESCE(client_ip, ''),
		backend_url = COALESCE(backend_url, ''),
		model = COALESCE(model, ''),
		error_text = COALESCE(error_text, ''),
		request_raw_path = COALESCE(request_raw_path, ''),
		response_raw_path = COALESCE(response_raw_path, ''),
		user_agent = COALESCE(user_agent, '')
		WHERE
			query IS NULL OR
			client_ip IS NULL OR
			backend_url IS NULL OR
			model IS NULL OR
			error_text IS NULL OR
			request_raw_path IS NULL OR
			response_raw_path IS NULL OR
			user_agent IS NULL`).Error
}

func findRawPayloadPath(dataDir, requestID, kind string) (string, bool) {
	rawRoot := filepath.Join(dataDir, "raw")
	entries, err := os.ReadDir(rawRoot)
	if err != nil {
		return "", false
	}
	fileName := fmt.Sprintf("%s-%s.gz", requestID, kind)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		fullPath := filepath.Join(rawRoot, entry.Name(), fileName)
		if _, err := os.Stat(fullPath); err == nil {
			return filepath.Join("raw", entry.Name(), fileName), true
		}
	}
	return "", false
}
func (st *Store) RepairStuckRequests() error {
	dataDir := st.dataDir

	var stuck []model.RequestRecord
	if err := st.db.Raw(`SELECT
		id, created_at, method, path, query, client_ip, backend_url, model,
		is_streaming, status_code, error_text, request_bytes, response_bytes,
		prompt_tokens, cached_prompt_tokens, cache_hit_pct, completion_tokens, total_tokens,
		prompt_ms, completion_ms, total_ms, first_byte_ms, chunks_count,
		request_raw_path, response_raw_path, user_agent
		FROM requests WHERE status_code = 0 AND (response_raw_path IS NULL OR response_raw_path = '')`).Scan(&stuck).Error; err != nil {
		return err
	}

	for i := range stuck {
		rec := stuck[i]
		respRel, ok := findRawPayloadPath(dataDir, rec.ID, "response")
		if !ok {
			continue
		}
		respAbs := filepath.Join(dataDir, respRel)
		respBytes, err := ReadGzipFile(respAbs)
		if err != nil {
			continue
		}
		parsed := meta.ParseResponseMeta(http.Header{"Content-Type": []string{"text/event-stream"}}, respBytes)
		if parsed.Model != "" {
			rec.Model = parsed.Model
		}
		rec.StatusCode = http.StatusOK
		rec.ResponseBytes = int64(len(respBytes))
		rec.PromptTokens = parsed.PromptTokens
		rec.CachedPromptTokens = parsed.CachedPromptTokens
		if parsed.PromptTokens > 0 && parsed.CachedPromptTokens > 0 {
			rec.CacheHitPct = float64(parsed.CachedPromptTokens) / float64(parsed.PromptTokens) * 100
		}
		rec.CompletionTokens = parsed.CompletionTok
		rec.TotalTokens = parsed.TotalTokens
		rec.PromptMs = parsed.PromptMs
		rec.CompletionMs = parsed.CompletionMs
		if stat, statErr := os.Stat(respAbs); statErr == nil {
			rec.TotalMs = float64(stat.ModTime().UTC().Sub(rec.CreatedAt.UTC()).Milliseconds())
		}
		rec.ResponseRawPath = respRel
		if err := db.RetryWrite(func() error {
			return st.db.Exec(`UPDATE requests SET
				model = ?,
				status_code = ?,
				error_text = ?,
				response_bytes = ?,
				prompt_tokens = ?,
				cached_prompt_tokens = ?,
				cache_hit_pct = ?,
				completion_tokens = ?,
				total_tokens = ?,
				prompt_ms = ?,
				completion_ms = ?,
				total_ms = ?,
				first_byte_ms = ?,
				chunks_count = ?,
				response_raw_path = ?
				WHERE id = ?`,
				rec.Model, rec.StatusCode, rec.ErrorText, rec.ResponseBytes,
				rec.PromptTokens, rec.CachedPromptTokens, rec.CacheHitPct, rec.CompletionTokens, rec.TotalTokens,
				rec.PromptMs, rec.CompletionMs, rec.TotalMs, rec.FirstByteMs,
				rec.ChunksCount, rec.ResponseRawPath, rec.ID,
			).Error
		}); err != nil {
			continue
		}
	}
	return nil
}
func (st *Store) InsertRequest(rec model.RequestRecord) error {
	return db.RetryWrite(func() error {
		return st.db.Exec(`INSERT INTO requests (
			id, created_at, method, path, query, client_ip, backend_url, model,
			is_streaming, request_bytes, request_raw_path, user_agent
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.ID, st.createdAtValue(rec.CreatedAt), rec.Method, rec.Path, rec.Query,
			rec.ClientIP, rec.BackendURL, rec.Model, rec.IsStreaming,
			rec.RequestBytes, rec.RequestRawPath, rec.UserAgent,
		).Error
	})
}

func (st *Store) FinishRequest(id string, rec model.RequestRecord) error {
	return db.RetryWrite(func() error {
		return st.db.Exec(`UPDATE requests SET
			model = ?,
			status_code = ?,
			error_text = ?,
			response_bytes = ?,
			prompt_tokens = ?,
			cached_prompt_tokens = ?,
			cache_hit_pct = ?,
			completion_tokens = ?,
			total_tokens = ?,
			prompt_ms = ?,
			completion_ms = ?,
			total_ms = ?,
			first_byte_ms = ?,
			chunks_count = ?,
			response_raw_path = ?
			WHERE id = ?`,
			rec.Model, rec.StatusCode, rec.ErrorText, rec.ResponseBytes,
			rec.PromptTokens, rec.CachedPromptTokens, rec.CacheHitPct, rec.CompletionTokens, rec.TotalTokens,
			rec.PromptMs, rec.CompletionMs, rec.TotalMs, rec.FirstByteMs,
			rec.ChunksCount, rec.ResponseRawPath, id,
		).Error
	})
}

func (st *Store) GetRequests(limit int, offset int, f model.RequestFilter) ([]model.RequestRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	query := `SELECT
		id, created_at, method, path, query, client_ip, backend_url, model,
		is_streaming, status_code, error_text, request_bytes, response_bytes,
		prompt_tokens, cached_prompt_tokens, cache_hit_pct, completion_tokens, total_tokens,
		prompt_ms, completion_ms, total_ms, first_byte_ms, chunks_count,
		request_raw_path, response_raw_path, user_agent
		FROM requests WHERE 1=1`
	args := make([]any, 0, 16)

	query, args = appendRequestFilterSQL(query, args, f, st.isPostgres)

	query += ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	out := make([]model.RequestRecord, 0, limit)
	if err := st.db.Raw(query, args...).Scan(&out).Error; err != nil {
		return nil, err
	}
	for i := range out {
		meta.NormalizeCacheHit(&out[i])
		out[i] = meta.EnrichRequestRates(out[i])
	}
	return out, nil
}

func (st *Store) GetRequestByID(id string) (model.RequestRecord, error) {
	var rec model.RequestRecord
	err := st.db.Where("id = ?", id).Take(&rec).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return rec, sql.ErrNoRows
		}
		return rec, err
	}
	meta.NormalizeCacheHit(&rec)
	return meta.EnrichRequestRates(rec), nil
}

func (st *Store) DeleteRequestByID(id string) error {
	rec, err := st.GetRequestByID(id)
	if err != nil {
		return err
	}
	for _, rel := range []string{rec.RequestRawPath, rec.ResponseRawPath} {
		if rel == "" {
			continue
		}
		fullPath := filepath.Clean(filepath.Join(st.dataDir, rel))
		relCheck, relErr := filepath.Rel(st.dataDir, fullPath)
		if relErr != nil || strings.HasPrefix(relCheck, "..") {
			return fmt.Errorf("refusing to delete path outside data dir")
		}
		if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	var affected int64
	err = db.RetryWrite(func() error {
		res := st.db.Exec(`DELETE FROM requests WHERE id = ?`, id)
		affected = res.RowsAffected
		return res.Error
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}
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
	var completionTokens int64
	var totalTokens int64
	var lifetimeRequests int64
	var lifetimeTokens int64
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
		COALESCE(SUM(total_tokens),0)
		FROM requests`).Row()
	if err := row.Scan(&lifetimeRequests, &lifetimeTokens); err != nil {
		return nil, err
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
		"hours":                    secs / 3600,
		"active_connections":       st.activeCount(),
		"total_requests":           totalRequests,
		"total_prompt_tokens":      promptTokens,
		"total_completion_tokens":  completionTokens,
		"total_tokens":             totalTokens,
		"matching_total_requests":  totalRequests,
		"matching_total_tokens":    totalTokens,
		"lifetime_total_requests":  lifetimeRequests,
		"lifetime_total_tokens":    lifetimeTokens,
		"total_request_bytes":      reqBytes,
		"total_response_bytes":     respBytes,
		"avg_prompt_ms":            avgPromptMs,
		"avg_completion_ms":        avgCompletionMs,
		"avg_total_ms":             avgTotalMs,
		"avg_first_byte_ms":        avgFirstByteMs,
		"requests_per_minute":      rpm,
		"prompt_tokens_per_second": promptTokensPerSec,
		"decode_tokens_per_second": decodeTokensPerSec,
		"tokens_per_second":        tokensPerSec,
		"errors_count":             errorsCount,
		"error_rate":               errorRate,
		"streaming_requests":       streamCount,
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

func (st *Store) GetBackends() ([]string, error) {
	return scanDistinctStrings(st.db, `SELECT DISTINCT backend_url
		FROM requests
		WHERE backend_url IS NOT NULL AND TRIM(backend_url) != ''
		ORDER BY backend_url ASC`, 16)
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

func (st *Store) GetBackendMetrics(limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 5000 {
		limit = 200
	}
	rows, err := st.db.Raw(`SELECT created_at, backend_url, metric_name, metric_value
		FROM backend_metrics ORDER BY created_at DESC, id DESC LIMIT ?`, limit).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]map[string]any, 0, limit)
	for rows.Next() {
		var createdAt string
		var backendURL string
		var metricName string
		var metricValue float64
		if err := rows.Scan(&createdAt, &backendURL, &metricName, &metricValue); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"created_at":   createdAt,
			"backend_url":  backendURL,
			"metric_name":  metricName,
			"metric_value": metricValue,
		})
	}
	return out, rows.Err()
}
func (st *Store) CleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	st.Cleanup()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st.Cleanup()
		}
	}
}

func (st *Store) Cleanup() {
	days := st.retentionDays.Load()
	if days <= 0 {
		return
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -int(days))
	cutoffVal := st.createdAtValue(cutoff)
	if err := db.RetryWrite(func() error {
		return st.db.Exec(`DELETE FROM requests WHERE created_at < ?`, cutoffVal).Error
	}); err != nil {
		log.WithCtx(context.Background()).Infof("cleanup requests failed: %v", err)
	}
	if err := db.RetryWrite(func() error {
		return st.db.Exec(`DELETE FROM backend_metrics WHERE created_at < ?`, cutoffVal).Error
	}); err != nil {
		log.WithCtx(context.Background()).Infof("cleanup backend_metrics failed: %v", err)
	}

	rawRoot := filepath.Join(st.dataDir, "raw")
	entries, err := os.ReadDir(rawRoot)
	if err != nil && !os.IsNotExist(err) {
		log.WithCtx(context.Background()).Infof("cleanup raw read failed: %v", err)
		return
	}
	cutoffDate := time.Now().UTC().AddDate(0, 0, -int(days))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		d, err := time.Parse("2006-01-02", entry.Name())
		if err != nil {
			continue
		}
		if d.Before(cutoffDate) {
			_ = os.RemoveAll(filepath.Join(rawRoot, entry.Name()))
		}
	}
}

// filterTimeArg 将时间转换为对应数据库驱动可比较的参数值。
func filterTimeArg(t time.Time, isPostgres bool) any {
	if isPostgres {
		return t.UTC()
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// effectiveWindow 返回筛选生效的 [from, to] 时间窗口。
// 若 filter 指定了绝对时间（TimeFrom/TimeTo）则优先使用，否则回退到"最近 fallbackHours 小时"。
func (st *Store) EffectiveWindow(f model.RequestFilter, fallbackHours int) (time.Time, time.Time) {
	now := time.Now().UTC()
	from := now.Add(-time.Duration(fallbackHours) * time.Hour)
	to := now
	if !f.TimeFrom.IsZero() {
		from = f.TimeFrom.UTC()
	}
	if !f.TimeTo.IsZero() {
		to = f.TimeTo.UTC()
	}
	return from, to
}

func appendRequestFilterSQL(query string, args []any, f model.RequestFilter, isPostgres bool) (string, []any) {
	if f.ChatCompletionsOnly {
		query += ` AND method = ? AND path = ?`
		args = append(args, http.MethodPost, "/v1/chat/completions")
	}
	if f.Path != "" {
		query += ` AND path LIKE ?`
		args = append(args, "%"+f.Path+"%")
	}
	if f.Model != "" {
		query += ` AND model LIKE ?`
		args = append(args, "%"+f.Model+"%")
	}
	if f.Method != "" {
		query += ` AND method = ?`
		args = append(args, f.Method)
	}
	if f.Backend != "" {
		query += ` AND backend_url = ?`
		args = append(args, f.Backend)
	}
	if f.ClientIP != "" {
		query += ` AND client_ip LIKE ?`
		args = append(args, "%"+f.ClientIP+"%")
	}
	if f.UserAgent != "" {
		query += ` AND user_agent LIKE ?`
		args = append(args, "%"+f.UserAgent+"%")
	}
	if f.StatusCode > 0 {
		query += ` AND status_code = ?`
		args = append(args, f.StatusCode)
	}
	if !f.TimeFrom.IsZero() {
		query += ` AND created_at >= ?`
		args = append(args, filterTimeArg(f.TimeFrom, isPostgres))
	}
	if !f.TimeTo.IsZero() {
		query += ` AND created_at <= ?`
		args = append(args, filterTimeArg(f.TimeTo, isPostgres))
	}
	if f.Streaming != nil {
		query += ` AND is_streaming = ?`
		args = append(args, *f.Streaming)
	}
	if f.ErrorsOnly {
		query += ` AND (status_code >= 400 OR error_text != '')`
	}
	if f.WithTokens {
		query += ` AND total_tokens > 0`
	}
	if f.Search != "" {
		query += ` AND (
			id LIKE ? OR path LIKE ? OR query LIKE ? OR client_ip LIKE ? OR model LIKE ? OR error_text LIKE ?
		)`
		pattern := "%" + f.Search + "%"
		args = append(args, pattern, pattern, pattern, pattern, pattern, pattern)
	}
	return query, args
}

// getDailyStats 按天聚合请求数与 token 用量。
// 返回按日期升序排列的每日统计，覆盖最近 days 天。
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

// createdAtValue 按数据库类型生成 created_at 参数值。
func (st *Store) createdAtValue(t time.Time) any {
	if st.isPostgres {
		return t.UTC()
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// RecordBackendMetrics 将抓取自后端 /metrics 的指标写入 backend_metrics 表。
func (st *Store) RecordBackendMetrics(baseURL string, metrics map[string]float64) error {
	if len(metrics) == 0 {
		return nil
	}
	now := st.createdAtValue(time.Now().UTC())
	return db.RetryWrite(func() error {
		tx := st.db.Begin()
		if tx.Error != nil {
			return tx.Error
		}
		for name, value := range metrics {
			if err := tx.Exec(`INSERT INTO backend_metrics (created_at, backend_url, metric_name, metric_value) VALUES (?, ?, ?, ?)`, now, baseURL, name, value).Error; err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		return tx.Commit().Error
	})
}

// DataDir 返回原始报文与本地数据目录。
func (st *Store) DataDir() string { return st.dataDir }
