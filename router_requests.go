package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"llama_proxy/internal/httpx"
	"llama_proxy/internal/model"
	"llama_proxy/internal/store"
)



func (s *Server) apiRequests(c *gin.Context) {
	limit := httpx.QueryInt(c.Request, "limit", 100)
	offset := httpx.QueryInt(c.Request, "offset", 0)
	f := parseRequestFilter(c.Request)

	recs, err := s.store.GetRequests(limit, offset, f)
	if err != nil {
		httpx.WriteJSON(c.Writer, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	httpx.WriteJSON(c.Writer, http.StatusOK, map[string]any{"items": recs, "limit": limit, "offset": offset, "filters": f})
}

// apiInflight 返回当前正在处理（已选中后端、尚未完成）的请求列表，
// 供面板实时展示当前请求的客户端 IP / User-Agent。
func (s *Server) apiInflight(c *gin.Context) {
	httpx.WriteJSON(c.Writer, http.StatusOK, map[string]any{"items": s.inflightList()})
}

func (s *Server) apiRequest(c *gin.Context) {
	id := c.Param("id")
	if c.Request.Method == http.MethodDelete {
		if err := s.store.DeleteRequestByID(id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				httpx.WriteJSON(c.Writer, http.StatusNotFound, map[string]any{"error": "not found"})
				return
			}
			httpx.WriteJSON(c.Writer, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		s.hub.BroadcastWithType("request", map[string]any{"deleted": true, "id": id, "time": time.Now().UTC()})
		httpx.WriteJSON(c.Writer, http.StatusOK, map[string]any{"status": "deleted", "id": id})
		return
	}
	rec, err := s.store.GetRequestByID(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpx.WriteJSON(c.Writer, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		httpx.WriteJSON(c.Writer, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	httpx.WriteJSON(c.Writer, http.StatusOK, rec)
}

// apiRaw 处理 /_proxy/raw/{id}/{request|response}。
// 路由用通配 /*rest 捕获，交由 handleRaw 保留旧版两段式路径校验。
func (s *Server) apiRaw(c *gin.Context) {
	s.handleRaw(c.Writer, "/raw"+c.Param("rest"))
}

// apiEvents 是 SSE 事件流的 gin 包装。
func parseRequestFilter(r *http.Request) model.RequestFilter {
	f := model.RequestFilter{
		Path:                strings.TrimSpace(r.URL.Query().Get("path")),
		Model:               strings.TrimSpace(r.URL.Query().Get("model")),
		Method:              strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("method"))),
		Backend:             strings.TrimSpace(r.URL.Query().Get("backend")),
		ClientIP:            strings.TrimSpace(r.URL.Query().Get("client_ip")),
		UserAgent:           strings.TrimSpace(r.URL.Query().Get("user_agent")),
		Search:              strings.TrimSpace(r.URL.Query().Get("q")),
		StatusCode:          httpx.QueryInt(r, "status", 0),
		ErrorsOnly:          httpx.QueryBool(r, "errors_only", false),
		WithTokens:          httpx.QueryBool(r, "with_tokens", false),
		ChatCompletionsOnly: httpx.QueryBool(r, "chat_completions_only", false),
	}
	if t, ok := httpx.ParseTime(r.URL.Query().Get("time_from")); ok {
		f.TimeFrom = t
	}
	if t, ok := httpx.ParseTime(r.URL.Query().Get("time_to")); ok {
		f.TimeTo = t
	}
	if stream, ok := httpx.OptionalQueryBool(r, "stream"); ok {
		f.Streaming = &stream
	}
	return f
}

// handleRaw 返回已落盘的原始请求/响应报文（gzip 解压后按内容类型输出）。
func (s *Server) handleRaw(w http.ResponseWriter, p string) {
	parts := strings.Split(strings.TrimPrefix(p, "/raw/"), "/")
	if len(parts) != 2 {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": "use /_proxy/raw/{request_id}/{request|response}"})
		return
	}

	id := parts[0]
	kind := parts[1]
	rec, err := s.store.GetRequestByID(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpx.WriteJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		httpx.WriteJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	var rel string
	if kind == "request" {
		rel = rec.RequestRawPath
	} else if kind == "response" {
		rel = rec.ResponseRawPath
	} else {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": "part must be request or response"})
		return
	}
	if rel == "" {
		httpx.WriteJSON(w, http.StatusNotFound, map[string]any{"error": "raw payload not available"})
		return
	}

	fullPath := filepath.Clean(filepath.Join(s.store.DataDir(), rel))
	data, err := store.ReadGzipFile(fullPath)
	if err != nil {
		httpx.WriteJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	if json.Valid(data) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Header().Set("X-Request-ID", id)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleEvents 以 SSE 长连接广播请求/调度/在途连接等实时事件。
