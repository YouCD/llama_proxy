package main

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"llama_proxy/internal/httpx"
)



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
	pg.Any("/inflight", s.apiInflight)
	pg.Any("/models", s.apiModels)
	pg.Any("/backends", s.apiBackends)
	pg.Any("/backend-metrics", s.apiBackendMetrics)
	pg.Any("/scheduler", s.apiScheduler)
	pg.Any("/scheduler/switch", s.apiSchedulerSwitch)
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
			"/_proxy/inflight",
			"/_proxy/request/{id}",
			"/_proxy/raw/{id}/{request|response}",
			"/_proxy/events",
			"/_proxy/backend-metrics?limit=200",
			"/_proxy/daily-stats?days=30",
			"/_proxy/ui",
			"/_proxy/scheduler",
			"/_proxy/scheduler/switch",
		},
	})
}

func (s *Server) apiHealth(c *gin.Context) {
	httpx.WriteJSON(c.Writer, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC(), "active_connections": s.active.Load()})
}

func (s *Server) apiLive(c *gin.Context) {
	httpx.WriteJSON(c.Writer, http.StatusOK, map[string]any{"active_connections": s.active.Load(), "time": time.Now().UTC()})
}

