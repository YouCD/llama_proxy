package main

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"

	"llama_proxy/internal/balancer"
	"llama_proxy/internal/config"
	"llama_proxy/internal/events"
	"llama_proxy/internal/gpustats"
	"llama_proxy/internal/scheduler"
	"llama_proxy/internal/store"
)

// Server 是路由器核心对象，持有配置与各子系统（存储/负载均衡/调度/事件广播）。
// HTTP 入口见 router.go 的 gin 路由引擎；
// 后端池/调度的胶水逻辑（入池同步、metrics 轮询、事件广播）见 pool.go。
type Server struct {
	cfg     config.Config
	yamlCfg *config.YAMLConfig
	// cfgMu 保护可热更新的配置三元组（cfg / yamlCfg / staticBackends）：
	// config.Load 产出的配置结构体一经发布即视为不可变，yamlCfg 指针可安全共享。
	cfgMu          sync.RWMutex
	store          *store.Store
	balancer       *balancer.Balancer
	scheduler      *scheduler.Scheduler
	staticBackends []config.BackendConfig
	// localNodeSig 记录已入池的本地 background 成员 ID 签名（逗号连接），
	// 签名不变则跳过池重建。
	localNodeSig string
	// schedStateSig 记录上次广播的调度器状态签名，变化时广播 scheduler_state
	// 事件让前端刷新面板（前端无轮询，状态更新全靠此推送）。
	schedStateSig string
	// inflightMu/inflight 跟踪在途（已选中后端、尚未完成）请求，
	// 供监控面板实时展示当前请求的客户端 IP / User-Agent（见 inflight.go）。
	inflightMu sync.RWMutex
	inflight   map[string]*inflightInfo
	client     *http.Client
	hub        *events.EventHub
	active     atomic.Int64
	// requestTimeout 代理请求整体超时（纳秒，atomic 支持配置热更新）：在
	// handleProxy 中包装请求 ctx 生效。不直接改共享 http.Client.Timeout——
	// 与在途 client.Do 的并发读写是 data race。
	requestTimeout atomic.Int64
	// gpu 是 GPU 性能轮询器（nvidia-smi，5s 间隔）；无 GPU 环境可用但返回
	// available=false。
	gpu *gpustats.Poller

	sseMu       sync.Mutex
	sseCancels  map[context.Context]context.CancelFunc
	// proxyMu/proxyCancels 跟踪在途代理请求的可取消上下文，
	// 优雅起停时 cancelAllProxy 立即打断 LLM 长流，避免 Shutdown 傻等请求完成。
	proxyMu      sync.Mutex
	proxyCancels map[context.Context]context.CancelFunc
	schedMu     sync.RWMutex
	schedEvents []map[string]any

	// routerOnce/router 惰性构建并缓存 gin 路由引擎（见 router.go）。
	routerOnce sync.Once
	router     http.Handler
}

// snapshot 原子返回当前 legacy 配置副本与 YAML 配置指针。
// 读取方在请求处理期间持有一致视图，热更新不会撕裂读到的配置。
func (s *Server) snapshot() (config.Config, *config.YAMLConfig) {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg, s.yamlCfg
}

// applyConfig 发布新配置三元组（配置热更新后调用）。
func (s *Server) applyConfig(cfg config.Config, yamlCfg *config.YAMLConfig, staticBackends []config.BackendConfig) {
	s.cfgMu.Lock()
	s.cfg = cfg
	s.yamlCfg = yamlCfg
	s.staticBackends = staticBackends
	s.cfgMu.Unlock()
}

// newInflightCtx 返回一个带 request_id 的请求上下文，供 log.WithCtx 提取，
// 以便一个请求从接收到结束的所有日志能按 request_id 串联。
func (s *Server) newInflightCtx(parent context.Context, requestID string) context.Context {
	return context.WithValue(parent, "request_id", requestID)
}

// newProxyCtx 返回在途代理请求的可取消上下文（带 request_id），供日志串联。
// 上游请求用 http.NewRequestWithContext 派生自它，取消即断开上游连接、
// 打断进行中的流式拷贝，handler 随即返回，Shutdown 不必等请求自然结束。
func (s *Server) newProxyCtx(parent context.Context, requestID string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(s.newInflightCtx(parent, requestID))
	s.proxyMu.Lock()
	if s.proxyCancels == nil {
		s.proxyCancels = make(map[context.Context]context.CancelFunc)
	}
	s.proxyCancels[ctx] = cancel
	s.proxyMu.Unlock()

	return ctx, func() {
		cancel()
		s.proxyMu.Lock()
		delete(s.proxyCancels, ctx)
		s.proxyMu.Unlock()
	}
}

// cancelAllProxy 取消所有在途代理请求，用于优雅起停：
// 收到信号时打断进行中的 LLM 流式生成等长请求，handler 立即返回，
// server.Shutdown 毫秒级完成，不再傻等到超时。
func (s *Server) cancelAllProxy() {
	s.proxyMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.proxyCancels))
	for _, c := range s.proxyCancels {
		cancels = append(cancels, c)
	}
	s.proxyMu.Unlock()

	for _, c := range cancels {
		c()
	}
}

// newSSECtx 为 SSE 长连接创建可统一取消的上下文，优雅起停时能立即断开监控连接。
func (s *Server) newSSECtx(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	s.sseMu.Lock()
	if s.sseCancels == nil {
		s.sseCancels = make(map[context.Context]context.CancelFunc)
	}
	s.sseCancels[ctx] = cancel
	s.sseMu.Unlock()

	return ctx, func() {
		cancel()
		s.sseMu.Lock()
		delete(s.sseCancels, ctx)
		s.sseMu.Unlock()
	}
}

// cancelAllSSE 取消所有 SSE 长连接，用于优雅起停时避免 Shutdown 傻等监控连接。
func (s *Server) cancelAllSSE() {
	s.sseMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.sseCancels))
	for _, c := range s.sseCancels {
		cancels = append(cancels, c)
	}
	s.sseMu.Unlock()

	for _, c := range cancels {
		c()
	}
}
