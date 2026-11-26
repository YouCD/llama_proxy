package main

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"llama_proxy/internal/httpx"
	"llama_proxy/internal/model"
)



func (s *Server) apiStats(c *gin.Context) {
	hours := httpx.QueryInt(c.Request, "hours", 24)
	f := parseRequestFilter(c.Request)
	f.TimeFrom, f.TimeTo = s.store.EffectiveWindow(f, hours)
	stats, err := s.store.GetStats(f)
	if err != nil {
		httpx.WriteJSON(c.Writer, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 进程运行时长（秒），供面板页脚展示。
	stats["uptime_seconds"] = int(time.Since(s.startedAt).Seconds())
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
	s.enrichBackendMeta(items, f)
	httpx.WriteJSON(c.Writer, http.StatusOK, map[string]any{"items": items, "hours": hours})
}

// enrichBackendMeta 给统计条目附带配置中的部署模型与路由标签，
// 并补充时间窗口内实际承载的模型（按请求记录统计），
// 供面板在后端地址下方展示“部署了哪些模型、属于哪些路由池”。
// 配置未登记 model 的后端（如独立 llama.cpp 实例）也能显示实际承载的模型。
func (s *Server) enrichBackendMeta(items []map[string]any, f model.RequestFilter) {
	if len(items) == 0 {
		return
	}
	topModels, _ := s.store.GetBackendTopModels(f, 3)

	var meta map[string]map[string]any
	if _, yamlCfg := s.snapshot(); yamlCfg != nil {
		meta = make(map[string]map[string]any, len(yamlCfg.Backends.List)+1)
		for _, b := range yamlCfg.Backends.List {
			if b.URL == "" {
				continue
			}
			meta[b.URL] = map[string]any{"model": b.Model, "tags": sortedTagList(b.EffectiveTags())}
		}
		// 本地 background 组成员就绪时同样附带模型与标签（每个就绪成员各一条）。
		if s.scheduler != nil {
			for _, node := range s.scheduler.LocalNodes() {
				if node.BaseURL == "" {
					continue
				}
				if _, ok := meta[node.BaseURL]; ok {
					continue
				}
				tags := make(map[string]bool, len(node.Tags))
				for _, t := range node.Tags {
					if t = strings.TrimSpace(t); t != "" {
						tags[t] = true
					}
				}
				meta[node.BaseURL] = map[string]any{"model": node.ModelID, "tags": sortedTagList(tags)}
			}
		}
	}

	for _, it := range items {
		url, _ := it["backend_url"].(string)
		var models []string
		if m, ok := meta[url]; ok {
			if cm, _ := m["model"].(string); cm != "" {
				models = append(models, cm)
			}
			it["tags"] = m["tags"]
		}
		for _, m := range topModels[url] {
			if len(models) >= 3 {
				break
			}
			if !sliceContains(models, m) {
				models = append(models, m)
			}
		}
		if len(models) > 0 {
			it["model"] = models
		}
	}
}

func sliceContains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
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

func (s *Server) apiMinuteStats(c *gin.Context) {
	minutes := httpx.QueryInt(c.Request, "minutes", 60)
	f := parseRequestFilter(c.Request)
	items, err := s.store.GetMinuteStats(minutes, f)
	if err != nil {
		httpx.WriteJSON(c.Writer, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	httpx.WriteJSON(c.Writer, http.StatusOK, map[string]any{"items": items, "minutes": minutes})
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

func (s *Server) apiPaths(c *gin.Context) {
	items, err := s.store.GetPaths()
	if err != nil {
		httpx.WriteJSON(c.Writer, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	httpx.WriteJSON(c.Writer, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) apiClientIPs(c *gin.Context) {
	items, err := s.store.GetClientIPs()
	if err != nil {
		httpx.WriteJSON(c.Writer, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	httpx.WriteJSON(c.Writer, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) apiUserAgents(c *gin.Context) {
	items, err := s.store.GetUserAgents()
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

