package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"llama_proxy/internal/config"
	"llama_proxy/internal/httpx"
)



func (s *Server) apiScheduler(c *gin.Context) {
	if s.scheduler == nil {
		httpx.WriteJSON(c.Writer, http.StatusNotFound, map[string]any{"error": "scheduler disabled"})
		return
	}
	httpx.WriteJSON(c.Writer, http.StatusOK, s.schedulerStatus())
}

// apiSchedulerSwitch 处理 POST /_proxy/scheduler/switch：面板手动触发调度器切换到指定模型。
// 校验通过后在后台执行切换（模型加载最长 startup_timeout，不能阻塞 HTTP 响应），
// 立即返回 202；完成后广播调度事件，面板经 SSE 自动刷新状态。
//
//	body: {"model": "<scheduling 中配置的模型 ID>"}
func (s *Server) apiSchedulerSwitch(c *gin.Context) {
	if s.scheduler == nil {
		httpx.WriteJSON(c.Writer, http.StatusNotFound, map[string]any{"error": "scheduler disabled"})
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Model) == "" {
		httpx.WriteJSON(c.Writer, http.StatusBadRequest, map[string]any{"error": `body must be {"model": "<model id>"}`})
		return
	}
	modelID := strings.TrimSpace(req.Model)
	sc := s.schedulingConfig()
	entry := sc.Entry(modelID)
	if sc == nil || entry == nil {
		httpx.WriteJSON(c.Writer, http.StatusBadRequest,
			map[string]any{"error": fmt.Sprintf("model %q is not configured in scheduling", modelID)})
		return
	}
	// 目标为 background 且 coding 租约仍活跃：ensureModel 会保护正在使用的 coding
	// 进程跳过切换（与流量驱动的 background 请求行为一致），此处提前给出明确提示。
	if entry.Mode == config.ModeBackground && s.scheduler.IsCodingActive() {
		httpx.WriteJSON(c.Writer, http.StatusOK, map[string]any{
			"status":  "skipped",
			"model":   modelID,
			"message": "coding lease active, background switch skipped",
		})
		return
	}
	// coding 模式手动切换同样续期开发租约，与流量驱动切换行为一致。
	if entry.Mode == config.ModeCoding {
		s.scheduler.TouchCoding()
	}
	s.pushSchedEvent("manual_switch", map[string]any{"model": modelID})
	// 用 Background ctx：切换是进程级动作，与本次 HTTP 请求生命周期解耦，
	// 客户端断开（关浏览器）不中断模型加载。
	go func() {
		_, err := s.scheduler.EnsureModel(context.Background(), modelID)
		if err != nil {
			s.pushSchedEvent("manual_switch_failed", map[string]any{"model": modelID, "error": err.Error()})
			return
		}
		if entry.Mode == config.ModeCoding {
			s.scheduler.TouchCoding()
		}
		s.pushSchedEvent("manual_switch_ready", map[string]any{"model": modelID})
	}()
	httpx.WriteJSON(c.Writer, http.StatusAccepted, map[string]any{"status": "switching", "model": modelID})
}

// apiRequest 处理 GET（详情）/ DELETE（删除）/_proxy/request/{id}，方法分派与旧版一致。
func (s *Server) schedulerStatus() map[string]any {
	if s.scheduler == nil {
		return map[string]any{"enabled": false}
	}
	snap := s.scheduler.Snapshot()

	// 整体就绪：coding 态下要求当前 coding 进程就绪；background 组态下要求组内
	// 全部成员就绪。
	allReady := false
	mode := ""
	for _, st := range snap.Models {
		if st.Running && st.Mode == "coding" {
			mode = "coding"
			if st.ID == snap.CodingModel {
				allReady = st.Ready
			}
			break
		}
	}
	if mode != "coding" && len(snap.Group) > 0 {
		mode = "background"
		// 只要求常驻组成员就绪；coding 成员平时停着，不参与整体就绪判定。
		allReady = true
		for _, st := range snap.Models {
			if st.InGroup && !st.Ready {
				allReady = false
				break
			}
		}
	}

	status := map[string]any{
		"enabled":        true,
		"mode":           mode,
		"ready":          allReady,
		"coding_active":  s.scheduler.IsCodingActive(),
		"coding_model":   snap.CodingModel,
		"group":          snap.Group,
		"last_coding_at": nil,
	}
	if snap.SwitchTarget != "" {
		status["switch_target"] = snap.SwitchTarget
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

	// 面板模型列表：Snapshot 已按字典序列出全部 scheduling 条目及运行时状态，
	// active 表示当前已就绪且可直连（组态下的就绪组成员 / coding 态下的 coding 进程）。
	models := make([]map[string]any, 0, len(snap.Models))
	for _, st := range snap.Models {
		// active：当前已就绪且可直连——coding 进程须为当前 coding 模型；
		// background 成员须在组态下（coding 态本地成员全部停止）且属于默认组。
		active := st.Ready && ((st.Mode == "coding" && snap.CodingModel == st.ID) ||
			(st.Mode == "background" && st.InGroup && snap.CodingModel == ""))
		m := map[string]any{
			"id":       st.ID,
			"mode":     st.Mode,
			"running":  st.Running,
			"ready":    st.Ready,
			"in_group": st.InGroup,
			"pid":      nil,
			"active":   active,
		}
		if st.PID != 0 {
			m["pid"] = st.PID
		}
		models = append(models, m)
	}
	status["models"] = models

	s.schedMu.RLock()
	events := append([]map[string]any(nil), s.schedEvents...)
	s.schedMu.RUnlock()
	status["events"] = events
	return status
}

// handleModels 让路由器自身应答 OpenAI 兼容的 GET /v1/models。
// 返回占位模型 llm_proxy（轮询模型列表）与所有已配置的具体模型 ID（backends.list
// 与本地调度进程）：请求 llm_proxy 走负载均衡，请求具体模型 ID 直连对应后端/进程。
// 不转发到后端，也不记录。
