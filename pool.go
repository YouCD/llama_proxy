package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"llama_proxy/internal/config"
	"llama_proxy/internal/meta"

	"github.com/youcd/toolkit/log"
)

// pool.go 汇集"后端池与调度"的胶水逻辑：
//
//	- 本地 background 组各成员的入池/出池同步（syncLocalBackendNodes）
//	- 各后端 /metrics 周期性抓取入库（backendMetricsLoop）
//	- 调度事件广播与 API Key 注入（pushSchedEvent / schedulerForwardBC）
//
// 核心实现在 internal/scheduler（进程调度）与 internal/balancer（负载均衡）；
// 这些胶水需要同时持有 scheduler、balancer、store、hub，Server 是唯一聚合者，
// 故留在根包，避免 internal 包之间互相依赖。

// pushSchedEvent 记录并广播一次调度相关事件（coding 请求、进程切换等）。
func (s *Server) pushSchedEvent(kind string, extra map[string]any) {
	ev := map[string]any{"kind": "scheduler_" + kind, "time": time.Now().UTC()}
	for k, v := range extra {
		ev[k] = v
	}
	s.schedMu.Lock()
	s.schedEvents = append(s.schedEvents, ev)
	if len(s.schedEvents) > 200 {
		s.schedEvents = s.schedEvents[len(s.schedEvents)-200:]
	}
	s.schedMu.Unlock()
	s.hub.BroadcastWithType("scheduler", ev)
}

// schedulerForwardBCFor 返回指定模型进程的后端 API Key 注入配置（模型进程就绪且
// 配置了自身 API Key 时生效），使客户端 key 与后端 key 隔离。
func (s *Server) schedulerForwardBCFor(modelID string) *config.BackendConfig {
	if key := s.scheduler.APIKeyFor(modelID); key != "" {
		return &config.BackendConfig{APIKey: key}
	}
	return nil
}

// localNodeNamePrefix 是本地 background 组成员进程在代理池中的节点名前缀
//（完整节点名为 local-<模型 ID>，与 backends.list 静态节点名区分）。
const localNodeNamePrefix = "local-"

// syncLocalBackendNodes 将本地 background 组成员的就绪状态同步到代理池：
// background 组态下每个就绪成员各作为一个后端节点入池（与 backends.list 一起
// 并行负载均衡）；coding 态（本地成员全部停止）或成员未就绪（启动中/崩溃）时出池。
// 仅就绪成员集合变化时重建池。
func (s *Server) syncLocalBackendNodes() {
	if s.balancer == nil || s.scheduler == nil {
		return
	}
	nodes := s.scheduler.LocalNodes()
	sig := make([]string, 0, len(nodes))
	for _, n := range nodes {
		sig = append(sig, n.ModelID)
	}
	if strings.Join(sig, ",") == s.localNodeSig {
		return
	}
	s.localNodeSig = strings.Join(sig, ",")
	merged := make([]config.BackendConfig, 0, len(s.staticBackends)+len(nodes))
	merged = append(merged, s.staticBackends...)
	for _, node := range nodes {
		if node.BaseURL == "" {
			continue
		}
		merged = append(merged, config.BackendConfig{
			Name:   localNodeNamePrefix + node.ModelID,
			URL:    node.BaseURL,
			Weight: node.Weight,
			Tags:   node.Tags,
			APIKey: node.APIKey,
			// 该成员的模型 ID：请求该 ID 时可经 GetBackendByModel 反查到本地节点直连。
			Model: node.ModelID,
		})
	}
	s.balancer.Update(merged)
}

// syncLocalBackendNodesLoop 周期性（每秒）同步本地 background 节点的入池状态，
// 并检测调度器状态变化后广播事件（前端无轮询，面板状态更新全靠此推送）。
func (s *Server) syncLocalBackendNodesLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		s.syncLocalBackendNodes()
		s.broadcastSchedulerStateChange()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// broadcastSchedulerStateChange 检测调度器运行时状态（模型进程起停、就绪、切换
// 目标等）的变化，变化时广播一个通知事件，前端收到后重新拉取完整状态。签名未
// 变则不推送，避免无谓刷面板。
func (s *Server) broadcastSchedulerStateChange() {
	if s.scheduler == nil {
		return
	}
	snap := s.scheduler.Snapshot()
	var b strings.Builder
	b.WriteString(snap.CodingModel)
	b.WriteString("|")
	b.WriteString(snap.SwitchTarget)
	b.WriteString("|")
	for i, id := range snap.Group {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(id)
	}
	for _, m := range snap.Models {
		fmt.Fprintf(&b, "|%s:%v:%v", m.ID, m.Running, m.Ready)
	}
	sig := b.String()
	if sig == s.schedStateSig {
		return
	}
	s.schedStateSig = sig
	s.pushSchedEvent("scheduler_state", nil)
}

// backendMetricsLoop 按配置间隔抓取各后端 /metrics 并入库。
func (s *Server) backendMetricsLoop(ctx context.Context) {
	cfg, _ := s.snapshot()
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pollBackendMetrics(ctx)
		}
	}
}

// pollBackendMetrics 抓取代理池中所有后端节点的一次 /metrics。
func (s *Server) pollBackendMetrics(ctx context.Context) {
	// Collect all enabled backends (from the balancer list) to poll.
	var urls []string
	if s.balancer != nil {
		for _, name := range s.balancer.Names() {
			if b := s.balancer.GetBackendByName(name); b != nil {
				urls = append(urls, strings.TrimRight(b.URL, "/"))
			}
		}
	}
	if len(urls) == 0 {
		return
	}

	for _, u := range urls {
		s.pollBackendMetricsURL(ctx, u)
	}
}

// pollBackendMetricsURL 抓取单个后端的 Prometheus 指标并入库。
func (s *Server) pollBackendMetricsURL(ctx context.Context, baseURL string) {
	u := baseURL + "/metrics"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := s.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return
	}
	metrics := meta.ParsePrometheusText(string(body))
	if err := s.store.RecordBackendMetrics(baseURL, metrics); err != nil {
		log.WithCtx(ctx).Infof("record backend metrics failed: %v", err)
	}
}
