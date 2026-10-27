// Package store 封装请求持久化、原始报文存储与统计查询。
package store

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"llama_proxy/internal/db"

	"github.com/youcd/toolkit/log"
)

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
