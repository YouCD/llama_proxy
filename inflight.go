// inflight.go 跟踪正在处理（已选中后端、尚未完成）的请求，
// 供监控面板实时展示“当前请求”的客户端 IP、User-Agent 等信息。
package main

import (
	"sort"
	"time"
)

// inflightInfo 描述一条在途（进行中）请求，字段与落库记录的前半段一致。
type inflightInfo struct {
	ID         string    `json:"id"`
	ClientIP   string    `json:"client_ip"`
	UserAgent  string    `json:"user_agent"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Model      string    `json:"model"`
	BackendURL string    `json:"backend_url"`
	Streaming  bool      `json:"streaming"`
	StartedAt  time.Time `json:"started_at"`
}

// inflightAdd 登记一条在途请求，并向 SSE 订阅者广播最新列表。
// inflight 允许为 nil（测试中直接零值构造 Server 的场景），首次写入时惰性初始化。
func (s *Server) inflightAdd(info *inflightInfo) {
	s.inflightMu.Lock()
	if s.inflight == nil {
		s.inflight = make(map[string]*inflightInfo)
	}
	s.inflight[info.ID] = info
	s.inflightMu.Unlock()
	s.broadcastInflight()
}

// inflightRemove 移除一条在途请求（请求完成时调用），并向 SSE 订阅者广播最新列表。
func (s *Server) inflightRemove(id string) {
	s.inflightMu.Lock()
	_, ok := s.inflight[id]
	delete(s.inflight, id)
	s.inflightMu.Unlock()
	if ok {
		s.broadcastInflight()
	}
}

// inflightList 返回按开始时间排序的在途请求快照。
func (s *Server) inflightList() []inflightInfo {
	s.inflightMu.RLock()
	out := make([]inflightInfo, 0, len(s.inflight))
	for _, info := range s.inflight {
		out = append(out, *info)
	}
	s.inflightMu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// broadcastInflight 广播当前在途请求列表（SSE 事件 "inflight"）。
// 事件携带完整列表：面板据此整体重建状态，无需处理增量差量。
func (s *Server) broadcastInflight() {
	s.hub.BroadcastWithType("inflight", map[string]any{
		"items": s.inflightList(),
		"time":  time.Now().UTC(),
	})
}
