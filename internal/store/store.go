// Package store 封装请求持久化、原始报文存储与统计查询。
package store

import (
	"net/http"
	"sync/atomic"
	"time"

	"llama_proxy/internal/db"
	"llama_proxy/internal/model"

)

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
func (st *Store) createdAtValue(t time.Time) any {
	if st.isPostgres {
		return t.UTC()
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// RecordBackendMetrics 将抓取自后端 /metrics 的指标写入 backend_metrics 表。
func (st *Store) DataDir() string { return st.dataDir }
