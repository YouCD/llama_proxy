package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"llama_proxy/internal/balancer"
	"llama_proxy/internal/config"
	"llama_proxy/internal/db"
	"llama_proxy/internal/events"
	"llama_proxy/internal/scheduler"
	"llama_proxy/internal/store"
)

type backendRecorder struct {
	mu    sync.Mutex
	count int
	auths []string
}

func (r *backendRecorder) snapshot() (int, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count, append([]string(nil), r.auths...)
}

// newRecordingBackend 返回一个记录请求数与收到 Authorization 的假后端。
func newRecordingBackend() (*httptest.Server, *backendRecorder) {
	rec := &backendRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.count++
		rec.auths = append(rec.auths, r.Header.Get("Authorization"))
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"m","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	return srv, rec
}

// newPoolTestServer 装配"调度 + 代理池"的测试 Server：
// 本地 background 进程就绪后作为动态节点与 staticBackends 一起入池。
func newPoolTestServer(t *testing.T, cfg *config.SchedulingConfig, staticBackends []config.BackendConfig, strategy string) *Server {
	t.Helper()
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: time.Second}
	sched := scheduler.New(cfg, time.Hour, sw, &http.Client{})
	sched.SetProbe(func(context.Context, string) bool { return true })
	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("scheduler start: %v", err)
	}
	t.Cleanup(sched.Shutdown)

	dataDir := t.TempDir()
	sqlCfg := config.DatabaseConfig{Type: "sqlite", SQLite: config.SQLiteConfig{Path: "proxy.db"}}
	database, err := db.NewDatabase(sqlCfg, dataDir, "debug")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.CloseDatabase(database) })
	if err := db.InitDB(database, "sqlite"); err != nil {
		t.Fatalf("init db: %v", err)
	}
	st := store.New(database, dataDir, 14)

	svc := &Server{
		cfg: config.Config{
			ListenAddr:          ":0",
			DataDir:             dataDir,
			MaxRequestBytes:     2 << 20,
			MaxCaptureBytes:     2 << 20,
			RequestTimeout:      15 * time.Second,
			RecordPaths:         []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings"},
			APIKey:              "client-key",
		},
		yamlCfg:        &config.YAMLConfig{Scheduling: cfg, Backends: config.BackendsConfig{Strategy: strategy}},
		store:          st,
		balancer:       balancer.New(staticBackends, strategy),
		staticBackends: staticBackends,
		scheduler:      sched,
		client:         &http.Client{Timeout: 15 * time.Second},
		hub:            events.New(),
	}
	svc.syncLocalBackendNode()
	return svc
}

// testDo 经代理发起一次 chat 请求，返回状态码。coding=true 时请求 coding 固化的模型 ID，
// 否则请求 llm_prox（轮询代理池）。
func (s *Server) testDo(t *testing.T, proxyURL string, coding bool) int {
	t.Helper()
	model := ProxyModelID
	if coding {
		model = s.yamlCfg.Scheduling.Coding.Model
	}
	req, _ := http.NewRequest(http.MethodPost, proxyURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy do: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestHandleProxySchedulerPool 验证调度模式下非 coding 流量统一走代理池：
// 本地 background 进程就绪后以配置权重入池，与 backends.list 一起按 wrr 分发，
// 且各自注入自身的 API Key。
func TestHandleProxySchedulerPool(t *testing.T) {
	local, localRec := newRecordingBackend()
	remote, remoteRec := newRecordingBackend()
	defer local.Close()
	defer remote.Close()

	cfg := &config.SchedulingConfig{
		Coding: config.ProcessConfig{
			Model:        "qwen3.8",
			ReadinessURL: local.URL,
			APIKey:       "coding-key",
		},
		Background: config.ProcessConfig{
			Model:        "qwen3.6",
			ReadinessURL: local.URL,
			APIKey:       "bg-key",
			Weight:       2,
		},
	}
	staticBackends := []config.BackendConfig{
		{Name: "remote", URL: remote.URL, Weight: 1, APIKey: "remote-key"},
	}
	svc := newPoolTestServer(t, cfg, staticBackends, "wrr")

	if names := svc.balancer.Names(); len(names) != 2 {
		t.Fatalf("pool after sync = %v, want 2 nodes (static + local-background)", names)
	}

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	// wrr 权重 2:1 → 6 个非 coding 请求应严格分为 local:4 / remote:2。
	for i := 0; i < 6; i++ {
		if code := svc.testDo(t, proxy.URL, false); code != http.StatusOK {
			t.Fatalf("non-coding request #%d status=%d, want 200", i, code)
		}
	}

	lc, lauths := localRec.snapshot()
	rc, rauths := remoteRec.snapshot()
	if lc != 4 || rc != 2 {
		t.Fatalf("distribution local=%d remote=%d, want 4/2", lc, rc)
	}
	for i, a := range lauths {
		if a != "Bearer bg-key" {
			t.Fatalf("local request #%d auth=%q, want Bearer bg-key", i, a)
		}
	}
	for i, a := range rauths {
		if a != "Bearer remote-key" {
			t.Fatalf("remote request #%d auth=%q, want Bearer remote-key", i, a)
		}
	}
}

// TestHandleProxySchedulerCodingPool 验证 coding 期间本地 background 节点出池：
// 非 coding 流量只去 backends.list 节点，不打到本地（此时本地是 coding 进程）。
func TestHandleProxySchedulerCodingPool(t *testing.T) {
	local, localRec := newRecordingBackend()
	remote, remoteRec := newRecordingBackend()
	defer local.Close()
	defer remote.Close()

	cfg := &config.SchedulingConfig{
		Coding: config.ProcessConfig{
			Model:        "qwen3.8",
			ReadinessURL: local.URL,
			APIKey:       "coding-key",
		},
		Background: config.ProcessConfig{
			Model:        "qwen3.6",
			ReadinessURL: local.URL,
			APIKey:       "bg-key",
		},
	}
	staticBackends := []config.BackendConfig{
		{Name: "remote", URL: remote.URL, Weight: 1, APIKey: "remote-key"},
	}
	svc := newPoolTestServer(t, cfg, staticBackends, "rr")

	if got := svc.balancer.Len(); got != 2 {
		t.Fatalf("pool before coding = %d nodes, want 2", got)
	}

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	// coding 请求打到本地 coding 进程。
	if code := svc.testDo(t, proxy.URL, true); code != http.StatusOK {
		t.Fatalf("coding request status=%d, want 200", code)
	}

	// 进入 coding 模式后同步池：本地节点出池。
	svc.syncLocalBackendNode()
	if got := svc.balancer.Len(); got != 1 {
		t.Fatalf("pool during coding = %d nodes, want 1 (static only)", got)
	}

	// 非 coding 请求只去 remote。
	for i := 0; i < 2; i++ {
		if code := svc.testDo(t, proxy.URL, false); code != http.StatusOK {
			t.Fatalf("non-coding request #%d status=%d, want 200", i, code)
		}
	}

	lc, lauths := localRec.snapshot()
	rc, rauths := remoteRec.snapshot()
	if lc != 1 || len(lauths) != 1 || lauths[0] != "Bearer coding-key" {
		t.Fatalf("local received count=%d auths=%v, want 1 request with Bearer coding-key", lc, lauths)
	}
	if rc != 2 {
		t.Fatalf("remote received count=%d, want 2", rc)
	}
	for i, a := range rauths {
		if a != "Bearer remote-key" {
			t.Fatalf("remote request #%d auth=%q, want Bearer remote-key", i, a)
		}
	}
}

// TestHandleProxySchedulerKeyIsolation 验证调度模式下客户端 key 与后端 key 隔离：
// 客户端携带 proxy.api_key 通过代理层校验，转发前 Authorization 被替换为当前
// 模型进程自身的 api_key（background 与 coding 各自独立）。
func TestHandleProxySchedulerKeyIsolation(t *testing.T) {
	var mu sync.Mutex
	var gotAuth []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"m","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer backend.Close()

	cfg := &config.SchedulingConfig{
		Coding: config.ProcessConfig{
			Model:        "qwen3.8",
			ReadinessURL: backend.URL,
			APIKey:       "coding-key",
		},
		Background: config.ProcessConfig{
			Model:        "qwen3.6",
			ReadinessURL: backend.URL,
			APIKey:       "bg-key",
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: time.Second}
	sched := scheduler.New(cfg, time.Hour, sw, &http.Client{})
	sched.SetProbe(func(context.Context, string) bool { return true })
	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("scheduler start: %v", err)
	}
	defer sched.Shutdown()

	dataDir := t.TempDir()
	sqlCfg := config.DatabaseConfig{Type: "sqlite", SQLite: config.SQLiteConfig{Path: "proxy.db"}}
	database, err := db.NewDatabase(sqlCfg, dataDir, "debug")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.CloseDatabase(database)
	if err := db.InitDB(database, "sqlite"); err != nil {
		t.Fatalf("init db: %v", err)
	}
	st := store.New(database, dataDir, 14)

	// 空静态池：本地 background 节点入池后独占代理池（非 coding 流量走池）。
	var staticBackends []config.BackendConfig
	svc := &Server{
		cfg: config.Config{
			ListenAddr:          ":0",
			DataDir:             dataDir,
			MaxRequestBytes:     2 << 20,
			MaxCaptureBytes:     2 << 20,
			RequestTimeout:      15 * time.Second,
			RecordPaths:         []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings"},
			APIKey:              "client-key",
		},
		yamlCfg:        &config.YAMLConfig{Scheduling: cfg},
		store:          st,
		balancer:       balancer.New(staticBackends, "rr"),
		staticBackends: staticBackends,
		scheduler:      sched,
		client:         &http.Client{Timeout: 15 * time.Second},
		hub:            events.New(),
	}
	svc.syncLocalBackendNode()
	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	do := func(coding bool) int {
		model := ProxyModelID
		if coding {
			model = cfg.Coding.Model
		}
		req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer client-key")
		resp, err := proxy.Client().Do(req)
		if err != nil {
			t.Fatalf("proxy do: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := do(false); code != http.StatusOK {
		t.Fatalf("background request status=%d, want 200", code)
	}
	if code := do(true); code != http.StatusOK {
		t.Fatalf("coding request status=%d, want 200", code)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotAuth) != 2 || gotAuth[0] != "Bearer bg-key" || gotAuth[1] != "Bearer coding-key" {
		t.Fatalf("backend received auth=%v, want [Bearer bg-key Bearer coding-key]", gotAuth)
	}
}

// TestHandleProxySchedulerGoClawBackground 验证本地 background 节点带 tool_call 标签
// 入池后：命中 tool_call 路由规则的请求只命中该节点，普通流量仍按全量池分发。
func TestHandleProxySchedulerGoClawBackground(t *testing.T) {
	local, localRec := newRecordingBackend()
	remote, remoteRec := newRecordingBackend()
	defer local.Close()
	defer remote.Close()

	cfg := &config.SchedulingConfig{
		Coding: config.ProcessConfig{
			Model:        "qwen3.8",
			ReadinessURL: local.URL,
			APIKey:       "coding-key",
		},
		Background: config.ProcessConfig{
			Model:        "qwen3.6",
			ReadinessURL: local.URL,
			APIKey:       "bg-key",
			Weight:       1,
			Tags:         []string{"tool_call"},
		},
	}
	staticBackends := []config.BackendConfig{
		{Name: "remote", URL: remote.URL, Weight: 1, APIKey: "remote-key"},
	}
	svc := newPoolTestServer(t, cfg, staticBackends, "rr")
	svc.yamlCfg.Routing = &config.RoutingConfig{Rules: []config.RoutingRule{{
		Name:   "goclaw",
		Header: map[string]string{"User-Agent": "GoClaw/2.1"},
		Pool:   "tool_call",
	}}}

	// 本地节点入池后应携带 tool_call 标签。
	if b := svc.balancer.GetBackendByName(localBackgroundBackendName); b == nil || !b.EffectiveTags()["tool_call"] {
		t.Fatalf("local node in pool = %+v, want tool_call tag", b)
	}

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	do := func(ua string) int {
		req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"`+ProxyModelID+`","messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer client-key")
		req.Header.Set("User-Agent", ua)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("proxy do: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// 命中规则的请求只命中本地（唯一 tool_call 节点）。
	for i := 0; i < 4; i++ {
		if code := do("GoClaw/2.1"); code != http.StatusOK {
			t.Fatalf("GoClaw request #%d status=%d, want 200", i, code)
		}
	}
	// 普通流量走全量池（local+remote，rr 各半）。
	for i := 0; i < 4; i++ {
		if code := do("curl/8.5"); code != http.StatusOK {
			t.Fatalf("normal request #%d status=%d, want 200", i, code)
		}
	}

	lc, _ := localRec.snapshot()
	rc, _ := remoteRec.snapshot()
	// local = 4(GoClaw) + 2(普通 rr 份额)；remote = 2(普通 rr 份额)。
	// 若 GoClaw 泄漏到 remote，rc 会大于 2。
	if lc != 6 || rc != 2 {
		t.Fatalf("distribution local=%d remote=%d, want 6/2", lc, rc)
	}
}
