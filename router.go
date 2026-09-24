package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"llama_proxy/internal/httpx"
	"llama_proxy/internal/model"
	"llama_proxy/internal/store"
)

// newRouter 构建 gin 路由引擎，路由表与旧版 handleMonitor 的 switch 一一对应：
//
//	/_proxy + 固定端点	→ 监控 JSON API / SSE / 调度状态
//	/_proxy/ui、/ui/*、/assets/* → 前端静态资源（handleUI）
//	/_proxy 下未命中	→ 404 {"error":"unknown monitor endpoint"}（经 NoRoute 前缀分派）
//	其余所有路径/方法	→ 代理转发（handleProxy，经 NoRoute）
//
// 注：不用组级 /*catchAll 通配（gin 的 radix 树中组级 catch-all 与同组静态路由冲突），
// 统一用引擎级 NoRoute（gin v1.12 中 NoRoute 不进路由树，仅在匹配失败时兜底）。
func (s *Server) newRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	// 旧实现按原始路径精确匹配（不做尾斜杠 301），保持一致。
	r.RedirectTrailingSlash = false

	pg := r.Group("/_proxy")
	pg.Any("/", s.apiRoot)
	pg.Any("/health", s.apiHealth)
	pg.Any("/live", s.apiLive)
	pg.Any("/stats", s.apiStats)
	pg.Any("/stats-by-backend", s.apiStatsByBackend)
	pg.Any("/daily-stats", s.apiDailyStats)
	pg.Any("/requests", s.apiRequests)
	pg.Any("/models", s.apiModels)
	pg.Any("/backends", s.apiBackends)
	pg.Any("/backend-metrics", s.apiBackendMetrics)
	pg.Any("/scheduler", s.apiScheduler)
	pg.Any("/request/:id", s.apiRequest)
	// 通配保留旧版 "/raw/{id}/{request|response}" 的两段式校验（段数不对返回 400）。
	pg.Any("/raw/*rest", s.apiRaw)
	pg.Any("/events", s.apiEvents)

	ui := func(c *gin.Context) { s.handleUI(c.Writer, c.Request) }
	pg.Any("/ui", ui)
	pg.Any("/ui/*path", ui)
	pg.Any("/assets/*path", ui)

	// 裸 /_proxy（无尾斜杠）也返回根信息，与旧版 p=="/" 行为一致。
	r.Any("/_proxy", s.apiRoot)

	// NoRoute 兜底（与旧版 ServeHTTP 的前缀分派一致）：
	// /_proxy 下未命中任何路由 → 404 JSON；其余路径 → 代理转发。
	r.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/_proxy") {
			httpx.WriteJSON(c.Writer, http.StatusNotFound, map[string]any{"error": "unknown monitor endpoint"})
			return
		}
		s.handleProxy(c.Writer, c.Request)
	})

	return r
}

// getRouter 惰性构建并缓存 gin 路由引擎。
func (s *Server) getRouter() http.Handler {
	s.routerOnce.Do(func() { s.router = s.newRouter() })
	return s.router
}

// ServeHTTP 实现 http.Handler，委托给 gin 路由引擎（兼容 httptest.NewServer(svc) 等旧用法）。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.getRouter().ServeHTTP(w, r)
}

// ---- 监控端点 handler（由原 handleMonitor switch 分支迁移而来）----

func (s *Server) apiRoot(c *gin.Context) {
	httpx.WriteJSON(c.Writer, http.StatusOK, map[string]any{
		"name":    "llama_proxy",
		"version": "1.0.0",
		"endpoints": []string{
			"/_proxy/health",
			"/_proxy/live",
			"/_proxy/stats?hours=24",
			"/_proxy/requests?limit=100&offset=0",
			"/_proxy/request/{id}",
			"/_proxy/raw/{id}/{request|response}",
			"/_proxy/events",
			"/_proxy/backend-metrics?limit=200",
			"/_proxy/daily-stats?days=30",
			"/_proxy/ui",
			"/_proxy/scheduler",
		},
	})
}

func (s *Server) apiHealth(c *gin.Context) {
	httpx.WriteJSON(c.Writer, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC(), "active_connections": s.active.Load()})
}

func (s *Server) apiLive(c *gin.Context) {
	httpx.WriteJSON(c.Writer, http.StatusOK, map[string]any{"active_connections": s.active.Load(), "time": time.Now().UTC()})
}

func (s *Server) apiStats(c *gin.Context) {
	hours := httpx.QueryInt(c.Request, "hours", 24)
	f := parseRequestFilter(c.Request)
	f.TimeFrom, f.TimeTo = s.store.EffectiveWindow(f, hours)
	stats, err := s.store.GetStats(f)
	if err != nil {
		httpx.WriteJSON(c.Writer, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	httpx.WriteJSON(c.Writer, http.StatusOK, stats)
}

func (s *Server) apiStatsByBackend(c *gin.Context) {
	hours := httpx.QueryInt(c.Request, "hours", 24)
	f := parseRequestFilter(c.Request)
	f.TimeFrom, f.TimeTo = s.store.EffectiveWindow(f, hours)
	items, err := s.store.GetStatsByBackend(f)
	if err != nil {
		httpx.WriteJSON(c.Writer, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.enrichBackendMeta(items)
	httpx.WriteJSON(c.Writer, http.StatusOK, map[string]any{"items": items, "hours": hours})
}

// enrichBackendMeta 给统计条目附带配置中的部署模型与路由标签，
// 供面板在后端地址下方展示“部署了哪些模型、属于哪些路由池”。
func (s *Server) enrichBackendMeta(items []map[string]any) {
	_, yamlCfg := s.snapshot()
	if yamlCfg == nil || len(items) == 0 {
		return
	}
	meta := make(map[string]map[string]any, len(yamlCfg.Backends.List)+1)
	for _, b := range yamlCfg.Backends.List {
		if b.URL == "" {
			continue
		}
		meta[b.URL] = map[string]any{"model": b.Model, "tags": sortedTagList(b.EffectiveTags())}
	}
	// 本地 background 节点就绪时同样附带标签（模型名不在配置里，置空）。
	if yamlCfg.Scheduling != nil && s.scheduler != nil {
		if base, ready := s.scheduler.BackgroundReady(); ready && base != "" {
			if _, ok := meta[base]; !ok {
				meta[base] = map[string]any{"model": "", "tags": sortedTagList(yamlCfg.Scheduling.Background.EffectiveTags())}
			}
		}
	}
	for _, it := range items {
		url, _ := it["backend_url"].(string)
		if m, ok := meta[url]; ok {
			it["model"] = m["model"]
			it["tags"] = m["tags"]
		}
	}
}

// sortedTagList 返回按字典序排序的标签列表，保证展示顺序稳定。
func sortedTagList(tags map[string]bool) []string {
	out := make([]string, 0, len(tags))
	for t := range tags {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func (s *Server) apiDailyStats(c *gin.Context) {
	days := httpx.QueryInt(c.Request, "days", 30)
	f := parseRequestFilter(c.Request)
	items, err := s.store.GetDailyStats(days, f)
	if err != nil {
		httpx.WriteJSON(c.Writer, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	httpx.WriteJSON(c.Writer, http.StatusOK, map[string]any{"items": items, "days": days})
}

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

func (s *Server) apiModels(c *gin.Context) {
	items, err := s.store.GetModels()
	if err != nil {
		httpx.WriteJSON(c.Writer, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	httpx.WriteJSON(c.Writer, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) apiBackends(c *gin.Context) {
	items, err := s.store.GetBackends()
	if err != nil {
		httpx.WriteJSON(c.Writer, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	httpx.WriteJSON(c.Writer, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) apiBackendMetrics(c *gin.Context) {
	limit := httpx.QueryInt(c.Request, "limit", 200)
	items, err := s.store.GetBackendMetrics(limit)
	if err != nil {
		httpx.WriteJSON(c.Writer, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	httpx.WriteJSON(c.Writer, http.StatusOK, map[string]any{"items": items, "limit": limit})
}

func (s *Server) apiScheduler(c *gin.Context) {
	if s.scheduler == nil {
		httpx.WriteJSON(c.Writer, http.StatusNotFound, map[string]any{"error": "scheduler disabled"})
		return
	}
	httpx.WriteJSON(c.Writer, http.StatusOK, s.schedulerStatus())
}

// apiRequest 处理 GET（详情）/ DELETE（删除）/_proxy/request/{id}，方法分派与旧版一致。
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
func (s *Server) apiEvents(c *gin.Context) {
	s.handleEvents(c.Writer, c.Request)
}

// ---- 底层 handler 实现（原始报文 / SSE / 调度状态 / /v1/models 占位）----

// parseRequestFilter 从查询参数解析请求列表筛选条件。
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
func (s *Server) schedulerStatus() map[string]any {
	if s.scheduler == nil {
		return map[string]any{"enabled": false}
	}
	snap := s.scheduler.Snapshot()
	activeBase := s.scheduler.ActiveBaseURL()

	status := map[string]any{
		"enabled":         true,
		"mode":            snap.Mode,
		"ready":           snap.Ready,
		"coding_active":   s.scheduler.IsCodingActive(),
		"last_coding_at":  nil,
		"pid":             nil,
		"active_base_url": activeBase,
	}
	if snap.PID != 0 {
		status["pid"] = snap.PID
	}
	if !snap.LastCodingAt.IsZero() {
		status["last_coding_at"] = snap.LastCodingAt.UTC()
		status["lease_seconds"] = int(snap.Lease.Seconds())
		idleSeconds := int(time.Since(snap.LastCodingAt).Seconds())
		status["idle_seconds"] = idleSeconds
		remaining := snap.Lease - time.Since(snap.LastCodingAt)
		if remaining > 0 {
			status["lease_remaining_seconds"] = int(remaining.Seconds())
		} else {
			status["lease_remaining_seconds"] = 0
		}
	}

	s.schedMu.RLock()
	events := append([]map[string]any(nil), s.schedEvents...)
	s.schedMu.RUnlock()
	status["events"] = events
	return status
}

// handleModels 让路由器自身应答 OpenAI 兼容的 GET /v1/models。
// 返回占位模型 llm_prox（轮询模型列表）与所有已配置的具体模型 ID（backends.list
// 与本地调度进程）：请求 llm_prox 走负载均衡，请求具体模型 ID 直连对应后端/进程。
// 不转发到后端，也不记录。
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
			add(sc.Coding.Model)
			add(sc.Background.Model)
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
