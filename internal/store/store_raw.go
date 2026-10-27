// Package store 封装请求持久化、原始报文存储与统计查询。
package store

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"


)

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
