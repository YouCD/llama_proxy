package main

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"llama_proxy/internal/gpustats"
	"llama_proxy/internal/httpx"
)

// apiGpu 返回最新 GPU 性能快照（nvidia-smi，5s 轮询）。
// 无 GPU / nvidia-smi 不可用时 available=false，前端据此隐藏面板。
func (s *Server) apiGpu(c *gin.Context) {
	snap := s.gpuLatest()
	httpx.WriteJSON(c.Writer, http.StatusOK, snap)
}

// gpuLatest 返回最新 GPU 快照；未启用或尚未完成首次轮询时返回不可用快照。
func (s *Server) gpuLatest() *gpustats.Snapshot {
	if s.gpu == nil {
		return &gpustats.Snapshot{Available: false, Gpus: []gpustats.GPU{}}
	}
	if snap := s.gpu.Latest(); snap != nil {
		return snap
	}
	return &gpustats.Snapshot{Available: false, Gpus: []gpustats.GPU{}}
}

// pushGpuEvent 广播最新 GPU 快照（数据变化时由 Poller 回调触发）。
func (s *Server) pushGpuEvent(snap *gpustats.Snapshot) {
	s.hub.BroadcastWithType("gpu", snap)
}
