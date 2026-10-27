package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"llama_proxy/internal/meta"

)

func readLimited(body io.Reader, cap int) ([]byte, bool) {
	raw, _ := io.ReadAll(io.LimitReader(body, int64(cap)+1))
	truncated := len(raw) > cap
	if truncated {
		raw = raw[:cap]
	}
	return raw, truncated
}

// recordRequest 保存请求原文并插入一条请求记录，指向实际转发（或最后尝试）的后端。

func isStreamingResponse(resp *http.Response, reqStreaming bool) bool {
	if reqStreaming {
		return true
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	return strings.Contains(ct, "text/event-stream")
}

// streamCopy 普通响应的流式转发：边读边写、边刷盘，同时限量采样到 capture。
func streamCopy(w http.ResponseWriter, src io.Reader, capture *meta.LimitedBuffer, started time.Time) (copied int64, firstByteMs float64, err error) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	seenFirst := false
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if !seenFirst {
				seenFirst = true
				firstByteMs = float64(time.Since(started).Milliseconds())
			}
			chunk := buf[:n]
			wn, werr := w.Write(chunk)
			if wn > 0 {
				copied += int64(wn)
				_, _ = capture.Write(chunk[:wn])
				if flusher != nil {
					flusher.Flush()
				}
			}
			if werr != nil {
				return copied, firstByteMs, werr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return copied, firstByteMs, nil
			}
			return copied, firstByteMs, rerr
		}
	}
}

// streamCopySSE 流式响应的逐行转发：额外统计 SSE data 块数量（排除 [DONE]）。
func streamCopySSE(w http.ResponseWriter, src io.Reader, capture *meta.LimitedBuffer, started time.Time) (copied int64, firstByteMs float64, chunks int64, err error) {
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(src)
	seenFirst := false
	for {
		line, rerr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if !seenFirst {
				seenFirst = true
				firstByteMs = float64(time.Since(started).Milliseconds())
			}
			if bytes.HasPrefix(line, []byte("data:")) {
				trimmed := strings.TrimSpace(strings.TrimPrefix(string(line), "data:"))
				if trimmed != "" && trimmed != "[DONE]" {
					chunks++
				}
			}
			wn, werr := w.Write(line)
			if wn > 0 {
				copied += int64(wn)
				_, _ = capture.Write(line[:wn])
				if flusher != nil {
					flusher.Flush()
				}
			}
			if werr != nil {
				return copied, firstByteMs, chunks, werr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return copied, firstByteMs, chunks, nil
			}
			return copied, firstByteMs, chunks, rerr
		}
	}
}
