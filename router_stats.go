package main

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"llama_proxy/internal/httpx"
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

