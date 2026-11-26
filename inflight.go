// inflight.go 跟踪正在处理（已选中后端、尚未完成）的请求，
// 供监控面板实时展示“当前请求”的客户端 IP、User-Agent 等信息。
package main

import (
	"sort"
	"sync"
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
	// 实时进度：流式请求由转发循环经 setProgress 回写，供面板展示
	// “等待首 token / 流式输出”状态、首 token 耗时、已生成 token 数与目标 token 数。
	FirstByteMs      float64 `json:"first_byte_ms"` // 0 = 尚未收到首字节
	CompletionTokens int64   `json:"completion_tokens"` // 已生成 token 数（流式：块内 usage 优先，否则按文本长度估算）
	MaxTokens        int64   `json:"max_tokens"` // 请求体声明的目标 token 数（0 = 未声明）
	Status           string  `json:"status"` // waiting=等待首 token / streaming=流式输出 / running=处理中（非流式）

	mu sync.Mutex // 保护上述进度字段（写：转发循环；读：inflightList）
}

// setProgress 回写一条流式请求的实时进度（firstByteMs<=0 表示尚未收到首字节）。
func (info *inflightInfo) setProgress(firstByteMs float64, tokens int64) {
	info.mu.Lock()
	if firstByteMs > 0 {
		info.FirstByteMs = firstByteMs
		if info.Status == "waiting" {
			info.Status = "streaming"
		}
	}
	if tokens > info.CompletionTokens {
		info.CompletionTokens = tokens
	}
	info.mu.Unlock()
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
