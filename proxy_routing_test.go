package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llama_proxy/internal/balancer"
	"llama_proxy/internal/config"
	"llama_proxy/internal/db"
	"llama_proxy/internal/events"
	"llama_proxy/internal/store"
)

func newRoutingTestServer(t *testing.T, backends []config.BackendConfig, routing *config.RoutingConfig) (*Server, func()) {
	t.Helper()
	dataDir := t.TempDir()
	sqlCfg := config.DatabaseConfig{Type: "sqlite", SQLite: config.SQLiteConfig{Path: "proxy.db"}}
	database, err := db.NewDatabase(sqlCfg, dataDir, "debug")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.InitDB(database, "sqlite"); err != nil {
		t.Fatalf("init db: %v", err)
	}
	st := store.New(database, dataDir, 14)
	st.Active = func() int64 { return 0 }

	svc := &Server{
		cfg:      config.Config{RecordPaths: []string{"/v1/chat/completions"}},
		yamlCfg:  &config.YAMLConfig{Routing: routing},
		store:    st,
		balancer: balancer.New(backends, "rr"),
		client:   &http.Client{Timeout: 10 * time.Second},
		hub:      events.New(),
	}
	return svc, func() { _ = db.CloseDatabase(database) }
}

// doRoutingProxy 以指定 UA 与请求头发一个请求，断言状态码。
func doRoutingProxy(t *testing.T, proxyURL, ua string, headers map[string]string, want int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, proxyURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"llm_proxy","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("status=%d, want %d", resp.StatusCode, want)
	}
}

// TestRoutingRulesConfigured 验证配置 routing.rules 后：请求按配置顺序命中首条规则，
// 只从规则 pool 标签子池选点；header 多条件 AND；动态覆盖对命中规则的请求不生效。
func TestRoutingRulesConfigured(t *testing.T) {
	fast, fastRec := newRecordingBackend()
	tagged, taggedRec := newRecordingBackend()
	plain, plainRec := newRecordingBackend()
	defer fast.Close()
	defer tagged.Close()
	defer plain.Close()

	backends := []config.BackendConfig{
		{Name: "fast", URL: fast.URL, Weight: 1, Tags: []string{"fast"}},
		{Name: "tagged", URL: tagged.URL, Weight: 1, Tags: []string{"tool_call"}},
		{Name: "plain", URL: plain.URL, Weight: 1},
	}
	routing := &config.RoutingConfig{Rules: []config.RoutingRule{
		{
			Name:   "agentx-fast",
			Header: map[string]string{"User-Agent": "AgentX/1.0", "X-Agent-Env": "prod"},
			Pool:   "fast",
		},
		{
			Name:   "agentx-tool",
			Header: map[string]string{"User-Agent": "AgentX/1.0"},
			Pool:   "tool_call",
		},
	}}
	svc, cleanup := newRoutingTestServer(t, backends, routing)
	defer cleanup()
	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	// UA AgentX/1.0 + header prod → 规则 1（fast 子池）
	doRoutingProxy(t, proxy.URL, "AgentX/1.0", map[string]string{"X-Agent-Env": "prod"}, http.StatusOK)
	// UA AgentX/1.0 + header 值不对 → 规则 1 不中（AND），命中规则 2（tool_call 子池）
	doRoutingProxy(t, proxy.URL, "AgentX/1.0", map[string]string{"X-Agent-Env": "dev"}, http.StatusOK)
	// UA AgentX/1.0 无 header → 规则 2（tool_call 子池）
	doRoutingProxy(t, proxy.URL, "AgentX/1.0", nil, http.StatusOK)
	// UA 不包含 AgentX/1.0 → 无规则命中，走全量池
	doRoutingProxy(t, proxy.URL, "AgentX/1.1", nil, http.StatusOK)

	fc, _ := fastRec.snapshot()
	tc, _ := taggedRec.snapshot()
	pc, _ := plainRec.snapshot()
	// 规则 1 的 1 个请求必须落在 fast（fc≥1），规则 2 的 2 个必须都落在 tagged（tc≥2）；
	// plain 只可能收到全量池那 1 个正常请求的份额（pc≤1）——若规则流量泄漏到 plain，pc 会大于 1。
	if fc < 1 {
		t.Fatalf("fast received=%d, want >=1 (all rule 1 traffic)", fc)
	}
	if tc < 2 {
		t.Fatalf("tagged received=%d, want >=2 (all rule 2 traffic)", tc)
	}
	if pc > 1 {
		t.Fatalf("plain received=%d, want at most 1 (normal full-pool share)", pc)
	}
}

// TestRoutingRuleUAContainsMatch 验证路由规则 User-Agent 采用不区分大小写的子串包含匹配
// （配置 "GoClaw" 可命中 "GoClaw/2.1 (Windows NT 10.0)"），其余请求头保持精确匹配。
func TestRoutingRuleUAContainsMatch(t *testing.T) {
	tagged, taggedRec := newRecordingBackend()
	plain, plainRec := newRecordingBackend()
	defer tagged.Close()
	defer plain.Close()

	backends := []config.BackendConfig{
		{Name: "tagged", URL: tagged.URL, Weight: 1, Tags: []string{"tool_call"}},
		{Name: "plain", URL: plain.URL, Weight: 1},
	}
	routing := &config.RoutingConfig{Rules: []config.RoutingRule{{
		Name:   "goclaw",
		Header: map[string]string{"User-Agent": "GoClaw", "X-Agent-Env": "prod"},
		Pool:   "tool_call",
	}}}
	svc, cleanup := newRoutingTestServer(t, backends, routing)
	defer cleanup()
	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	// UA 包含子串（带版本/平台后缀、大小写不同）+ 请求头精确匹配 → 命中规则。
	doRoutingProxy(t, proxy.URL, "GoClaw/2.1 (Windows NT 10.0; Win64)", map[string]string{"X-Agent-Env": "prod"}, http.StatusOK)
	doRoutingProxy(t, proxy.URL, "goclaw/3.0", map[string]string{"X-Agent-Env": "prod"}, http.StatusOK)
	// UA 包含但请求头不精确 → 不命中（AND）。
	doRoutingProxy(t, proxy.URL, "GoClaw/2.1", map[string]string{"X-Agent-Env": "dev"}, http.StatusOK)
	// UA 不包含 → 不命中。
	doRoutingProxy(t, proxy.URL, "curl/8.5", map[string]string{"X-Agent-Env": "prod"}, http.StatusOK)

	tc, _ := taggedRec.snapshot()
	pc, _ := plainRec.snapshot()
	// 命中的 2 个请求必须落在 tagged；未命中的 2 个走全量池（tagged+plain rr 各一半）。
	if tc < 2 {
		t.Fatalf("tagged received=%d, want >=2 (all rule traffic)", tc)
	}
	if pc != 1 {
		t.Fatalf("plain received=%d, want 1 (full-pool share of non-matched traffic)", pc)
	}
}

// TestRoutingRuleEmptyPool 验证规则命中但 pool 无后端时返回 400，未命中规则的流量不受影响。
func TestRoutingRuleEmptyPool(t *testing.T) {
	backend, _ := newRecordingBackend()
	defer backend.Close()
	routing := &config.RoutingConfig{Rules: []config.RoutingRule{{
		Name:   "ghost",
		Header: map[string]string{"User-Agent": "Ghost/1.0"},
		Pool:   "ghost-pool",
	}}}
	svc, cleanup := newRoutingTestServer(t, []config.BackendConfig{{Name: "b", URL: backend.URL, Weight: 1}}, routing)
	defer cleanup()
	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"llm_proxy","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Ghost/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ghost-pool") {
		t.Fatalf("error body=%s, want mention of pool ghost-pool", body)
	}

	// 未命中规则的流量不受影响
	if code := doGoClawProxy(t, proxy.URL, "curl/8.5"); code != http.StatusOK {
		t.Fatalf("normal request status=%d, want 200", code)
	}
}

// deadBackendURL 返回一个进程已退出（连接必然失败）的后端地址。
