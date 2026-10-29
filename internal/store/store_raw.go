// Package store 封装请求持久化、原始报文存储与统计查询。
package store

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// 原始报文压缩格式：zstd（最高压缩级别）。早期 gzip 存量已全部转码为 .zst。
var (
	zstdEncoderOnce sync.Once
	zstdEncoder     *zstd.Encoder
	zstdDecoderOnce sync.Once
	zstdDecoder     *zstd.Decoder
)

// zstdEnc 返回包级 zstd 编码器：最高压缩级别（SpeedBestCompression，level 22）。
// 原始报文每请求只写一次、量小，压缩耗时可忽略；换取更小的磁盘占用。
func zstdEnc() *zstd.Encoder {
	zstdEncoderOnce.Do(func() {
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
		if err != nil {
			panic(err)
		}
		zstdEncoder = enc
	})
	return zstdEncoder
}

func zstdDec() *zstd.Decoder {
	zstdDecoderOnce.Do(func() {
		dec, err := zstd.NewReader(nil)
		if err != nil {
			panic(err)
		}
		zstdDecoder = dec
	})
	return zstdDecoder
}

var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

// ReadCompressedFile 读取 zstd 压缩的原始报文文件（按 magic 校验格式）。
func ReadCompressedFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(raw, zstdMagic) {
		return nil, fmt.Errorf("not a zstd file: %s", filepath.Base(path))
	}
	return zstdDec().DecodeAll(raw, nil)
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
	fileName := fmt.Sprintf("%s-%s.zst", requestID, kind)
	relPath := filepath.Join(relDir, fileName)
	fullPath := filepath.Join(st.dataDir, relPath)

	compressed := zstdEnc().EncodeAll(data, nil)
	if err := os.WriteFile(fullPath, compressed, 0o644); err != nil {
		return "", err
	}
	return relPath, nil
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

// findRawPayloadPath 在 raw/日期 目录下查找原始报文文件（.zst）。
func findRawPayloadPath(dataDir, requestID, kind string) (string, bool) {
	rawRoot := filepath.Join(dataDir, "raw")
	entries, err := os.ReadDir(rawRoot)
	if err != nil {
		return "", false
	}
	fileName := fmt.Sprintf("%s-%s.zst", requestID, kind)
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
