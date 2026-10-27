package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"llama_proxy/internal/config"

)

func (s *Server) selectBackend(w http.ResponseWriter, r *http.Request, ctx context.Context, reqModel string) (*backendCandidate, string, func(map[string]bool) *backendCandidate, error) {
	// 具体模型 ID：直连对应后端/本地进程（不使用 routing 规则）。
	if reqModel != "" && reqModel != ProxyModelID {
		cand, err := s.selectSpecificBackend(r, ctx, reqModel)
		if err != nil {
			return nil, r.URL.RawQuery, nil, err
		}
		return cand, r.URL.RawQuery, noFailover, nil
	}

	var first *backendCandidate
	var nextFn func(map[string]bool) *backendCandidate

	if rule := s.matchRoutingRule(r); rule != nil {
		// 命中路由规则：只从 pool 标签子池选点（含本地 background 节点）。
		// 请求失败时自动转移到子池内下一个未尝试的后端。
		if s.balancer != nil {
			if b := s.balancer.SelectFromTag(rule.pool); b != nil {
				first = candidateOf(b)
				pool := rule.pool
				nextFn = func(exclude map[string]bool) *backendCandidate {
					if b := s.balancer.SelectFromTagExcluding(pool, exclude); b != nil {
						return candidateOf(b)
					}
					return nil
				}
			}
		}
		if first == nil {
			return nil, "", nil, fmt.Errorf("routing rule %q matched but pool %q has no available backend", rule.name, rule.pool)
		}
	} else if s.balancer != nil {
		if b := s.balancer.Select(); b != nil {
			first = candidateOf(b)
			nextFn = s.nextBalancerCandidate
		}
	}

	if first == nil {
		return nil, "", nil, fmt.Errorf("no backend selected: configure backends.list")
	}

	if err := config.ValidateBackendURL(first.url); err != nil {
		return nil, "", nil, fmt.Errorf("invalid backend URL: %w", err)
	}
	if nextFn == nil {
		nextFn = noFailover
	}
	return first, r.URL.RawQuery, nextFn, nil
}

// selectSpecificBackend 按具体模型 ID 直连部署该模型的目标：
//  1. 本地调度进程（scheduling 模型条目，请求该模型 ID 时启动/切换并等待就绪，
//     coding 模式同时续期开发租约；background 条目在 coding 租约仍活跃时进程被
//     占用，返回 503）；
//  2. 代理池内声明该模型的后端（backends.list 与就绪的本地节点，
//     经 GetBackendByModel 反查，模型 ID 唯一性由启动时校验保证）。
func (s *Server) selectSpecificBackend(r *http.Request, ctx context.Context, reqModel string) (*backendCandidate, error) {
	sc := s.schedulingConfig()
	if sc != nil && s.scheduler != nil {
		if entry := sc.Entry(reqModel); entry != nil {
			return s.selectScheduledModel(ctx, reqModel, entry)
		}
	}
	if s.balancer != nil {
		if b := s.balancer.GetBackendByModel(reqModel); b != nil {
			return candidateOf(b), nil
		}
	}
	return nil, fmt.Errorf("unknown model %q: use %q for load-balanced routing or one of the configured model ids", reqModel, ProxyModelID)
}

// selectScheduledModel 把请求路由到指定模型的本地调度进程：未就绪则触发切换并等待；
// coding 模式流量续期开发租约并广播事件；background 模式请求在 coding 租约仍活跃时
// 被拒（503），保护正在使用的 coding 进程。
func (s *Server) selectScheduledModel(ctx context.Context, modelID string, entry *config.ProcessConfig) (*backendCandidate, error) {
	if entry.Mode == config.ModeCoding {
		s.scheduler.TouchCoding()
		s.pushSchedEvent("coding_request", map[string]any{"model": modelID})
	}
	if entry.Mode == config.ModeBackground && s.scheduler.IsCodingActive() {
		return nil, statusErrf(http.StatusServiceUnavailable,
			"model %q not ready: coding model is active (lease), retry later", modelID)
	}
	ready, rerr := s.scheduler.EnsureModel(ctx, modelID)
	if rerr != nil {
		return nil, rerr
	}
	select {
	case <-ready:
	case <-ctx.Done():
		return nil, fmt.Errorf("timed out waiting for model %s ready", modelID)
	}
	base := s.scheduler.BaseURLFor(modelID)
	if base == "" {
		return nil, fmt.Errorf("model %s not ready", modelID)
	}
	if entry.Mode == config.ModeCoding {
		s.scheduler.TouchCoding()
	}
	return &backendCandidate{url: strings.TrimRight(base, "/"), cfg: s.schedulerForwardBCFor(modelID)}, nil
}

// schedulingConfig 返回调度配置（未启用调度时为 nil）。
func (s *Server) schedulingConfig() *config.SchedulingConfig {
	_, yamlCfg := s.snapshot()
	if yamlCfg == nil || yamlCfg.Scheduling == nil {
		return nil
	}
	return yamlCfg.Scheduling
}

// backendCandidate 是单个转发目标（后端地址及其配置；动态指定后端时 cfg 为 nil）。
type backendCandidate struct {
	url string
	cfg *config.BackendConfig
}

// backendAttempt 记录单个后端的请求结果，用于所有后端失败时把完整的故障转移过程
// 按后端分组返回给客户端。
type backendAttempt struct {
	Name       string `json:"name,omitempty"`
	URL        string `json:"url"`
	StatusCode int    `json:"status_code,omitempty"`
	Body       string `json:"body,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	Error      string `json:"error,omitempty"`
}

// noFailover 表示没有故障转移目标（单后端或动态指定的后端）。
var noFailover = func(map[string]bool) *backendCandidate { return nil }

// candidateKey 返回故障转移时标记“已尝试”的键：池内后端用后端名（与 balancer 的
// SelectExcluding 按名排除一致），动态指定的后端用地址（其不参与故障转移）。
func candidateKey(c *backendCandidate) string {
	if c.cfg != nil {
		return c.cfg.Name
	}
	return c.url
}

// candidateName 返回后端名；动态指定的后端没有名字，返回空串。
func candidateName(c *backendCandidate) string {
	if c.cfg != nil {
		return c.cfg.Name
	}
	return ""
}

// maxFailReportBytes 是“所有后端失败”报告中每个后端响应体捕获的字节上限。
const maxFailReportBytes = 8 * 1024

// readLimited 从 body 读取至多 cap 字节并返回截断标志（不关闭 body）。

func candidateOf(b *config.BackendConfig) *backendCandidate {
	return &backendCandidate{url: strings.TrimRight(b.URL, "/"), cfg: b}
}

// nextBalancerCandidate 从全量池返回下一个未尝试过的后端（请求失败后的自动故障转移）。
func (s *Server) nextBalancerCandidate(exclude map[string]bool) *backendCandidate {
	if s.balancer == nil {
		return nil
	}
	if b := s.balancer.SelectExcluding(exclude); b != nil {
		return candidateOf(b)
	}
	return nil
}

// isStreamingResponse 判定响应是否按流式回传（请求声明 stream 或响应为 SSE）。
