package main

import (
	"context"
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"llama_proxy/internal/balancer"
	"llama_proxy/internal/config"
	"llama_proxy/internal/db"
	"llama_proxy/internal/events"
	"llama_proxy/internal/model"
	"llama_proxy/internal/scheduler"
	"llama_proxy/internal/store"

	"github.com/youcd/toolkit/log"
)

func main() {
	yamlPath := flag.String("f", "", "path to YAML config file")
	flag.Parse()

	configPath := *yamlPath
	if configPath == "" {
		configPath = "config.yaml"
	}

	log.Init(&log.Config{Stdout: true})

	yamlCfg, err := config.Load(configPath)
	if err != nil {
		log.WithCtx(nil).Fatalf("load yaml config %s: %v", configPath, err)
	}

	cfg := yamlCfg.ToLegacy()
	log.SetLogLevel(cfg.LogLevel)
	if len(yamlCfg.Backends.List) == 0 && !yamlCfg.HasScheduling() {
		log.WithCtx(nil).Fatal("no enabled backend in backends.list")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.WithCtx(nil).Fatalf("mkdir data dir: %v", err)
	}

	database, err := db.NewDatabase(yamlCfg.Database, cfg.DataDir, cfg.LogLevel)
	if err != nil {
		log.WithCtx(nil).Fatalf("open db: %v", err)
	}
	defer db.CloseDatabase(database)

	dbType := yamlCfg.Database.Type
	if err := db.InitDB(database, dbType); err != nil {
		log.WithCtx(nil).Fatalf("init db: %v", err)
	}

	st := store.New(database, cfg.DataDir, cfg.RetentionDays)
	if err := st.Normalize(); err != nil {
		log.WithCtx(nil).Fatalf("normalize db: %v", err)
	}
	if err := st.RepairStuckRequests(); err != nil {
		log.WithCtx(nil).Infof("repair stuck requests failed: %v", err)
	}

	// 调度模式下本地 background 进程会作为动态节点加入代理池，因此即使 backends.list 为空也要建 balancer。
	var backendBalancer *balancer.Balancer
	if yamlCfg.HasWeightedBackends() || yamlCfg.HasScheduling() {
		backendBalancer = balancer.New(yamlCfg.Backends.List, yamlCfg.Backends.Strategy)
		// 池初始化/变更（如本地 background 节点入池/出池）时打印各池成员、权重与 strategy。
		backendBalancer.SetLogger(func(format string, args ...any) {
			log.WithCtx(nil).Infof(format, args...)
		})
		backendBalancer.LogPools()
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}

	s := &Server{
		cfg:            cfg,
		yamlCfg:        yamlCfg,
		store:          st,
		balancer:       backendBalancer,
		staticBackends: yamlCfg.Backends.List,
		client: &http.Client{
			Transport: transport,
			Timeout:   cfg.RequestTimeout,
		},
		hub: events.New(),
	}
	// 在途请求计数供 Store 的统计 active_connections 与调度器排空使用。
	st.Active = func() int64 { return s.active.Load() }

	// 装配进程调度子系统：仅在配置了 scheduling 时启用，否则保持纯转发。
	if yamlCfg.HasScheduling() {
		scheduler := scheduler.New(yamlCfg.Scheduling, yamlCfg.Scheduling.Lease.CodingIdleTimeout, yamlCfg.Scheduling.Switch, s.client)
		scheduler.SetActiveCount(func() int64 { return s.active.Load() })
		s.scheduler = scheduler
	}

	// 捕获 SIGINT / SIGTERM，用于优雅起停。
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if s.scheduler != nil {
		log.WithCtx(ctx).Infof("process scheduler enabled: coding_idle_timeout=%s drain=%s kill=%s startup=%s",
			yamlCfg.Scheduling.Lease.CodingIdleTimeout, yamlCfg.Scheduling.Switch.DrainTimeout, yamlCfg.Scheduling.Switch.KillTimeout, yamlCfg.Scheduling.Switch.StartupTimeout)
	}
	go st.CleanupLoop(ctx)
	if cfg.PollBackendMetrics {
		go s.backendMetricsLoop(ctx)
	}
	if s.scheduler != nil {
		go func() {
			if err := s.scheduler.Start(ctx); err != nil {
				log.WithCtx(ctx).Fatalf("scheduler start: %v", err)
			}
		}()
		go s.scheduler.Loop(ctx)
		go s.syncLocalBackendNodeLoop(ctx)
	}

	// 定时广播完整统计快照给 SSE 订阅者，取代前端轮询。
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			// 普通统计
			stats, err := s.store.GetStats(model.RequestFilter{})
			if err != nil {
				continue
			}
			s.hub.BroadcastWithType("stats", stats)
			// LLM 统计（只包含 chat_completions）
			llmStats, err := s.store.GetStats(model.RequestFilter{ChatCompletionsOnly: true})
			if err != nil {
				continue
			}
			s.hub.BroadcastWithType("llmStats", llmStats)
		}
	}()
	if s.scheduler != nil {
		go func() {
			ticker := time.NewTicker(3 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				status := s.schedulerStatus()
				s.hub.BroadcastWithType("scheduler", status)
			}
		}()
	}

	backendCount := 0
	if s.balancer != nil {
		backendCount = s.balancer.Len()
	}
	log.WithCtx(ctx).Infof("llama_proxy listening on %s, backends=%d", cfg.ListenAddr, backendCount)

	server := &http.Server{Addr: cfg.ListenAddr, Handler: s}
	// 优雅起停时先断开 SSE 长连接，避免 Shutdown 傻等监控页面连接直到超时。
	server.RegisterOnShutdown(s.cancelAllSSE)
	serverErr := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case err := <-serverErr:
		if err != nil {
			log.WithCtx(ctx).Fatalf("server failed: %v", err)
		}
	case <-ctx.Done():
		// 收到退出信号：停止接收新连接并排空在途请求。
		log.WithCtx(ctx).Infof("shutdown signal received, draining in-flight requests")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.WithCtx(ctx).Infof("graceful shutdown error: %v", err)
		}
		if s.scheduler != nil {
			s.scheduler.Shutdown()
		}
		cancel()
		log.WithCtx(ctx).Infof("shutdown complete")
	}
}
