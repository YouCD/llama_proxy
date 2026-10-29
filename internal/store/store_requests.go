// Package store 封装请求持久化、原始报文存储与统计查询。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"llama_proxy/internal/db"
	"llama_proxy/internal/meta"
	"llama_proxy/internal/model"

	"gorm.io/gorm"
)

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
		respBytes, err := ReadCompressedFile(respAbs)
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
