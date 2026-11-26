package main

import (
	"bufio"
	"bytes"
	"encoding/json"
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
// onProgress 在每个 data 块到达时回调（firstByteMs 未收到首字节前为 0），供调用方
// 回写在途请求进度；nil 表示不回调。
func streamCopySSE(w http.ResponseWriter, src io.Reader, capture *meta.LimitedBuffer, started time.Time, onProgress func(firstByteMs float64, tokens int64)) (copied int64, firstByteMs float64, chunks int64, err error) {
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(src)
	seenFirst := false
	var estTokens int64
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
					if onProgress != nil {
						// 优先用块内真实 usage，否则按 delta 文本长度估算
						if tokens := estStreamTokens(trimmed, estTokens); tokens >= 0 {
							estTokens = tokens
							onProgress(firstByteMs, tokens)
						}
					}
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

// estStreamTokens 解析单个 SSE data 块：含 usage.completion_tokens 时返回真实累计值；
// 否则按 delta 文本长度估算累计 token（CJK 1字≈1 token，其他 4字符≈1 token）。
// 解析失败返回 -1，调用方保留上一值。
func estStreamTokens(data string, prevEst int64) int64 {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning"`
			} `json:"delta"`
		} `json:"choices"`
		Usage *struct {
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return -1
	}
	if chunk.Usage != nil && chunk.Usage.CompletionTokens > 0 {
		return chunk.Usage.CompletionTokens
	}
	est := prevEst
	for _, c := range chunk.Choices {
		est += estTokensFromText(c.Delta.Content) + estTokensFromText(c.Delta.Reasoning)
	}
	return est
}

// estTokensFromText 粗略估算文本的 token 数。
func estTokensFromText(s string) int64 {
	cjk, other := 0, 0
	for _, r := range s {
		switch {
		case (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3400 && r <= 0x4DBF):
			cjk++
		case r != ' ':
			other++
		}
	}
	return int64(cjk) + int64((other+3)/4)
}
