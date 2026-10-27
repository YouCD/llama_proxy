// Package store 封装请求持久化、原始报文存储与统计查询。
package store

import (
	"time"

	"llama_proxy/internal/db"

)

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
