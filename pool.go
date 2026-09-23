package main

import (
	"context"
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
//	- 本地 background 节点的入池/出池同步（syncLocalBackendNode）
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

// schedulerForwardBC 返回当前激活模式（coding/background）的后端 API Key 注入配置。
func (s *Server) schedulerForwardBC() *config.BackendConfig {
	if key := s.scheduler.ActiveAPIKey(); key != "" {
		return &config.BackendConfig{APIKey: key}
	}
	return nil
}

// localBackgroundBackendName 是本地 background 模型进程在代理池中的节点名。
const localBackgroundBackendName = "local-background"

// syncLocalBackendNode 将本地 background 模型进程的就绪状态同步到代理池：
// 就绪时作为后端节点入池（与 backends.list 一起参与负载均衡）；
// 未就绪（coding 进行中/启动中/崩溃）时出池。仅状态变化时重建池。
func (s *Server) syncLocalBackendNode() {
	if s.balancer == nil || s.scheduler == nil || s.yamlCfg == nil || s.yamlCfg.Scheduling == nil {
		return
	}
	base, ready := s.scheduler.BackgroundReady()
	if ready == s.localNodeInPool {
		return
	}
	s.localNodeInPool = ready
	merged := make([]config.BackendConfig, 0, len(s.staticBackends)+1)
	merged = append(merged, s.staticBackends...)
	if ready && base != "" {
		w := s.yamlCfg.Scheduling.Background.Weight
		if w <= 0 {
			w = 1
		}
		merged = append(merged, config.BackendConfig{
			Name:   localBackgroundBackendName,
			URL:    base,
			Weight: w,
			Tags:   s.yamlCfg.Scheduling.Background.Tags,
			APIKey: s.yamlCfg.Scheduling.Background.APIKey,
			// 固化 background 模型的模型 ID：请求该 ID 时可经 GetBackendByModel 反查到本地节点直连。
			Model: s.yamlCfg.Scheduling.Background.Model,
		})
	}
	s.balancer.Update(merged)
}

// syncLocalBackendNodeLoop 周期性（每秒）同步本地 background 节点的入池状态。
func (s *Server) syncLocalBackendNodeLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		s.syncLocalBackendNode()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// backendMetricsLoop 按配置间隔抓取各后端 /metrics 并入库。
func (s *Server) backendMetricsLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.PollInterval)
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
