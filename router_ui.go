package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"llama_proxy/internal/httpx"
)



func (s *Server) apiEvents(c *gin.Context) {
	s.handleEvents(c.Writer, c.Request)
}

// ---- 底层 handler 实现（原始报文 / SSE / 调度状态 / /v1/models 占位）----

// parseRequestFilter 从查询参数解析请求列表筛选条件。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpx.WriteJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := s.hub.Subscribe()
	defer s.hub.Unsubscribe(ch)

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	ctx, cancelSSE := s.newSSECtx(r.Context())
	defer cancelSSE()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			_, _ = fmt.Fprintf(w, "%s\n", msg)
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = w.Write([]byte(": keepalive\n\n"))
			flusher.Flush()
		}
	}
}

// schedulerStatus 返回进程调度器的当前运行状态，供监控面板展示。
// 多进程组态：models 列出全部 scheduling 模型条目各自的运行状态（running/ready/pid），
// group 为常驻默认 background 组成员，coding_model 为 coding 态下运行的模型。
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	type obj struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	now := time.Now().Unix()
	ids := []string{ProxyModelID}
	seen := map[string]bool{ProxyModelID: true}
	_, yamlCfg := s.snapshot()
	if yamlCfg != nil {
		add := func(id string) {
			id = strings.TrimSpace(id)
			if id != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		for i := range yamlCfg.Backends.List {
			add(yamlCfg.Backends.List[i].Model)
		}
		if sc := yamlCfg.Scheduling; sc != nil {
			for id := range sc.Models {
				add(id)
			}
		}
	}
	data := make([]obj, 0, len(ids))
	for _, id := range ids {
		data = append(data, obj{ID: id, Object: "model", Created: now, OwnedBy: "llama_proxy"})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}
