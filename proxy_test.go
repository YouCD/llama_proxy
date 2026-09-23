package main

import (
	"context"
	"encoding/json"
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
	"llama_proxy/internal/model"
	"llama_proxy/internal/store"
)

func TestHandleProxyNonStreamingLlamaCppJSON(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{
			"model":"llama.cpp-actual",
			"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33},
			"timings":{"prompt_ms":50,"predicted_ms":125},
			"content":"ok"
		}`)
	}))
	defer backend.Close()

	svc, _, cleanup := newTestServer(t, backend.URL)
	defer cleanup()
	svc.cfg.RecordPaths = append(svc.cfg.RecordPaths, "/completion")

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	resp, err := proxy.Client().Post(proxy.URL+"/completion", "application/json", strings.NewReader(`{"model":"llm_prox","prompt":"hi"}`))
	if err != nil {
		t.Fatalf("proxy post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), `"llama.cpp-actual"`) {
		t.Fatalf("unexpected response body: %s", string(body))
	}

	listResp, err := proxy.Client().Get(proxy.URL + "/_proxy/requests?limit=10")
	if err != nil {
		t.Fatalf("get requests: %v", err)
	}
	defer listResp.Body.Close()
	var payload struct {
		Items []model.RequestRecord `json:"items"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode requests: %v", err)
	}
	if len(payload.Items) == 0 {
		t.Fatal("expected at least one request")
	}
	rec := payload.Items[0]
	if rec.Model != "llama.cpp-actual" {
		t.Fatalf("model=%q", rec.Model)
	}
	if rec.TotalTokens != 33 || rec.PromptTokens != 11 || rec.CompletionTokens != 22 {
		t.Fatalf("unexpected tokens: %+v", rec)
	}
	if rec.ResponseRawPath == "" {
		t.Fatal("expected response raw path")
	}
}

func TestHandleProxyStreamingLifecycle(t *testing.T) {
	backendStarted := make(chan struct{}, 1)
	releaseBackend := make(chan struct{})

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"model\":\"backend-final\",\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		flusher.Flush()
		select {
		case backendStarted <- struct{}{}:
		default:
		}
		<-releaseBackend
		_, _ = io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20,\"total_tokens\":30},\"timings\":{\"prompt_ms\":50,\"predicted_ms\":200}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer backend.Close()

	svc, _, cleanup := newTestServer(t, backend.URL)
	defer cleanup()

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	client := proxy.Client()
	reqBody := `{"model":"llm_prox","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	reqDone := make(chan error, 1)
	go func() {
		resp, err := client.Post(proxy.URL+"/v1/chat/completions", "application/json", strings.NewReader(reqBody))
		if err != nil {
			reqDone <- err
			return
		}
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
		reqDone <- err
	}()

	select {
	case <-backendStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("backend request did not start")
	}

	var liveID string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(proxy.URL + "/_proxy/requests?limit=10")
		if err != nil {
			t.Fatalf("load live requests: %v", err)
		}
		var payload struct {
			Items []model.RequestRecord `json:"items"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			resp.Body.Close()
			t.Fatalf("decode requests: %v", err)
		}
		resp.Body.Close()
		for _, item := range payload.Items {
			if item.Path == "/v1/chat/completions" {
				if item.StatusCode != 0 {
					t.Fatalf("expected in-flight status 0, got %d", item.StatusCode)
				}
				if item.ResponseRawPath != "" {
					t.Fatalf("expected empty response path while inflight, got %q", item.ResponseRawPath)
				}
				liveID = item.ID
				break
			}
		}
		if liveID != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if liveID == "" {
		t.Fatal("did not observe live request in monitor list")
	}

	close(releaseBackend)

	select {
	case err := <-reqDone:
		if err != nil {
			t.Fatalf("proxy request failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxy request did not finish")
	}

	resp, err := client.Get(proxy.URL + "/_proxy/request/" + liveID)
	if err != nil {
		t.Fatalf("load final request: %v", err)
	}
	defer resp.Body.Close()
	var rec model.RequestRecord
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		t.Fatalf("decode final request: %v", err)
	}
	if rec.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", rec.StatusCode)
	}
	if rec.Model != "backend-final" {
		t.Fatalf("model=%q", rec.Model)
	}
	if rec.TotalTokens != 30 || rec.PromptTokens != 10 || rec.CompletionTokens != 20 {
		t.Fatalf("unexpected tokens: %+v", rec)
	}
	if rec.ResponseRawPath == "" {
		t.Fatal("expected response raw path after completion")
	}
}

func TestProxyRecordPaths(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"llm_prox","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer backend.Close()

	svc, _, cleanup := newTestServer(t, backend.URL)
	defer cleanup()
	svc.cfg.RecordPaths = []string{"/v1/chat/completions", "/v1/completions"}

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	postJSON := func(path string) *http.Response {
		resp, err := proxy.Client().Post(proxy.URL+path, "application/json",
			strings.NewReader(`{"model":"llm_prox","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		return resp
	}

	resp := postJSON("/v1/embeddings")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("embeddings status=%d want 404 (not in whitelist)", resp.StatusCode)
	}

	recs, err := svc.store.GetRequests(100, 0, model.RequestFilter{})
	if err != nil {
		t.Fatalf("getRequests: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("whitelisted out path recorded: got %d", len(recs))
	}

	resp2 := postJSON("/v1/chat/completions")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("chat status=%d", resp2.StatusCode)
	}

	recs2, err := svc.store.GetRequests(100, 0, model.RequestFilter{})
	if err != nil {
		t.Fatalf("getRequests: %v", err)
	}
	if len(recs2) != 1 || recs2[0].Path != "/v1/chat/completions" {
		t.Fatalf("expected whitelisted request recorded, got %+v", recs2)
	}
}

func TestShouldRecordProxy(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()

	// 默认白名单：仅 OpenAI 兼容路径被转发记录，其余一律 404
	for _, p := range []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings"} {
		if !svc.shouldRecordProxy(p) {
			t.Fatalf("whitelisted path %s should record", p)
		}
	}
	for _, p := range []string{"/favicon.ico", "/metrics", "/.well-known/openid-configuration", "/", "/v1/foo"} {
		if svc.shouldRecordProxy(p) {
			t.Fatalf("non-whitelisted path %s should not record", p)
		}
	}

	// 自定义白名单覆盖默认
	svc.cfg.RecordPaths = []string{"/v1/chat/completions"}
	if !svc.shouldRecordProxy("/v1/chat/completions") {
		t.Fatal("whitelisted path should record")
	}
	if svc.shouldRecordProxy("/v1/embeddings") {
		t.Fatal("non-whitelisted path should not record")
	}
}

func TestSelectBackendWithBalancer(t *testing.T) {
	backends := []config.BackendConfig{
		{Name: "lb-1", URL: "http://lb1.example:8080", Weight: 1},
		{Name: "lb-2", URL: "http://lb2.example:8080", Weight: 1},
	}

	cfg := config.Config{
	}

	svc := &Server{
		cfg:      cfg,
		balancer: balancer.New(backends, "rr"),
	}

	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		cand, _, _, err := svc.selectBackend(nil, req, context.Background(), "")
		if err != nil {
			t.Fatalf("selectBackend: %v", err)
		}
		seen[cand.url] = true
	}

	if len(seen) != 2 {
		t.Fatalf("expected both backends to be selected, got %v", seen)
	}
}

func TestSelectBackendWithBalancerNoDefault(t *testing.T) {
	// No default_url, only load balancer - must still work.
	backends := []config.BackendConfig{
		{Name: "a", URL: "http://a.example:8080", Weight: 1},
		{Name: "b", URL: "http://b.example:8080", Weight: 1},
	}

	svc := &Server{
		cfg:      config.Config{},
		balancer: balancer.New(backends, "rr"),
	}

	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		cand, _, _, err := svc.selectBackend(nil, req, context.Background(), "")
		if err != nil {
			t.Fatalf("selectBackend: %v", err)
		}
		if cand.cfg == nil || cand.url == "" {
			t.Fatalf("expected balancer backend, got backend=%q cfg=%v", cand.url, cand.cfg)
		}
	}
}

func TestSelectBackendNoBackendConfigured(t *testing.T) {
	// No balancer -> error.
	svc := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	if _, _, _, err := svc.selectBackend(nil, req, context.Background(), ""); err == nil {
		t.Fatal("expected error when no backend available")
	}
}

func TestSelectBackendBalancerReturnsConfig(t *testing.T) {
	backends := []config.BackendConfig{
		{Name: "qwen", URL: "http://gpu-server-1:8080", Weight: 50, Model: "qwen", APIKey: "key-1"},
		{Name: "deepseek", URL: "http://gpu-server-2:8080", Weight: 50, Model: "deepseek", APIKey: "key-2"},
	}

	// Use weighted random so each backend can be hit many times and we can
	// verify config is attached to the right URL.
	svc := &Server{
		cfg: config.Config{},
		balancer: balancer.New(backends, "random"),
	}

	found := map[string]*config.BackendConfig{}
	for i := 0; i < 200; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		cand, _, _, err := svc.selectBackend(nil, req, context.Background(), "")
		if err != nil {
			t.Fatalf("selectBackend: %v", err)
		}
		if cand.cfg == nil {
			t.Fatal("expected non-nil backend config from balancer")
		}
		bc := cand.cfg
		if strings.TrimRight(bc.URL, "/") != cand.url {
			t.Fatalf("backend url mismatch: bc=%q sel=%q", bc.URL, cand.url)
		}
		found[bc.Name] = bc
	}

	if len(found) != 2 {
		t.Fatalf("expected both backends, got %v", found)
	}
	if found["qwen"].Model != "qwen" || found["qwen"].APIKey != "key-1" {
		t.Fatalf("qwen config wrong: %+v", found["qwen"])
	}
	if found["deepseek"].Model != "deepseek" || found["deepseek"].APIKey != "key-2" {
		t.Fatalf("deepseek config wrong: %+v", found["deepseek"])
	}
}

func TestHandleProxyWithModelRewriteAndAPIKey(t *testing.T) {
	var gotModel string
	var gotAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		if v, ok := m["model"].(string); ok {
			gotModel = v
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"qwen","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer backend.Close()

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

	backends := []config.BackendConfig{
		{Name: "qwen", URL: backend.URL, Weight: 1, Model: "qwen", APIKey: "secret-key"},
	}

	svc := &Server{
		cfg: config.Config{
			ListenAddr:          ":0",
			DataDir:             dataDir,
			RetentionDays:       14,
			MaxRequestBytes:     2 << 20,
			MaxCaptureBytes:     2 << 20,
			RequestTimeout:      15 * time.Second,
			RecordPaths:         []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings"},
		},
		store:    st,
		balancer: balancer.New(backends, "wrr"),
		client:   &http.Client{Timeout: 15 * time.Second},
		hub:      events.New(),
	}

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	resp, err := proxy.Client().Post(proxy.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"llm_prox","messages":[]}`))
	if err != nil {
		t.Fatalf("proxy post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(body))
	}

	if gotModel != "qwen" {
		t.Fatalf("backend received model=%q, want qwen (rewritten)", gotModel)
	}
	if gotAuth != "Bearer secret-key" {
		t.Fatalf("backend received Authorization=%q, want Bearer secret-key", gotAuth)
	}
}

func TestHandleProxyBackendKeyDoesNotOverrideClientKeyWhenEmpty(t *testing.T) {
	var gotAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"llm_prox","usage":{}}`)
	}))
	defer backend.Close()

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

	// No api_key configured on the backend -> client's Authorization passes through.
	backends := []config.BackendConfig{
		{Name: "plain", URL: backend.URL, Weight: 1, Model: "m"},
	}

	svc := &Server{
		cfg: config.Config{
			ListenAddr:          ":0",
			DataDir:             dataDir,
			RetentionDays:       14,
			MaxRequestBytes:     2 << 20,
			MaxCaptureBytes:     2 << 20,
			RequestTimeout:      15 * time.Second,
			RecordPaths:         []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings"},
		},
		store:    st,
		balancer: balancer.New(backends, "wrr"),
		client:   &http.Client{Timeout: 15 * time.Second},
		hub:      events.New(),
	}

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"llm_prox","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-key")
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatalf("proxy do: %v", err)
	}
	defer resp.Body.Close()
	if gotAuth != "Bearer client-key" {
		t.Fatalf("backend received Authorization=%q, want client-key to pass through", gotAuth)
	}
}

func TestHandleProxyStripsVersionPrefixWhenBackendHasV1(t *testing.T) {
	var gotPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"llm_prox","usage":{}}`)
	}))
	defer backend.Close()

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

	backends := []config.BackendConfig{
		{Name: "qwen", URL: backend.URL + "/v1", Weight: 1, Model: "qwen"},
	}

	svc := &Server{
		cfg: config.Config{
			ListenAddr:          ":0",
			DataDir:             dataDir,
			RetentionDays:       14,
			MaxRequestBytes:     2 << 20,
			MaxCaptureBytes:     2 << 20,
			RequestTimeout:      15 * time.Second,
			RecordPaths:         []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings"},
		},
		store:    st,
		balancer: balancer.New(backends, "wrr"),
		client:   &http.Client{Timeout: 15 * time.Second},
		hub:      events.New(),
	}

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	resp, err := proxy.Client().Post(proxy.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"llm_prox","messages":[]}`))
	if err != nil {
		t.Fatalf("proxy post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(body))
	}

	if gotPath != "/v1/chat/completions" {
		t.Fatalf("backend received path=%q, want /v1/chat/completions (no duplicate /v1)", gotPath)
	}
}

func TestBuildProxyPath(t *testing.T) {
	cases := []struct {
		backend, in, want string
	}{
		{"http://host/v1", "/v1/chat/completions", "/chat/completions"},
		{"http://host/v1", "/v1/completions", "/completions"},
		{"http://host/v1", "/v1/embeddings", "/embeddings"},
		{"http://host/v1", "/v1", "/"},
		{"http://host", "/v1/chat/completions", "/v1/chat/completions"},
		{"http://host", "/chat/completions", "/chat/completions"},
		{"http://host/v1", "/v2/models", "/v2/models"},
		{"http://host", "/", "/"},
	}
	for _, c := range cases {
		if got := buildProxyPath(c.backend, c.in); got != c.want {
			t.Errorf("buildProxyPath(%q, %q)=%q, want %q", c.backend, c.in, got, c.want)
		}
	}
}

// doGoClawProxy 经代理发起一次 chat 请求；ua 指定 User-Agent。
func doGoClawProxy(t *testing.T, proxyURL, ua string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, proxyURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"llm_prox","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy do: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestGoClawToolCallRouting 验证配置了 User-Agent 规则后：GoClaw 请求
// 只路由到带 tool_call 标签的后端；UA 不包含配置子串时不命中规则。
func TestGoClawToolCallRouting(t *testing.T) {
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
		Header: map[string]string{"User-Agent": "GoClaw/2.1"},
		Pool:   "tool_call",
	}}}
	svc, cleanup := newRoutingTestServer(t, backends, routing)
	defer cleanup()
	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	// UA 包含 GoClaw/2.1 → 命中规则 → 只去 tagged
	for i := 0; i < 6; i++ {
		if code := doGoClawProxy(t, proxy.URL, "GoClaw/2.1"); code != http.StatusOK {
			t.Fatalf("GoClaw request #%d status=%d, want 200", i, code)
		}
	}
	// UA 不包含 GoClaw/2.1 → 不命中规则，走全量池
	if code := doGoClawProxy(t, proxy.URL, "GoClaw/2.2"); code != http.StatusOK {
		t.Fatalf("non-matching UA request status=%d, want 200", code)
	}
	// 普通 UA：全量池 rr，两个后端各一半
	for i := 0; i < 3; i++ {
		if code := doGoClawProxy(t, proxy.URL, "curl/8.5"); code != http.StatusOK {
			t.Fatalf("normal request #%d status=%d, want 200", i, code)
		}
	}

	tc, _ := taggedRec.snapshot()
	pc, _ := plainRec.snapshot()
	// GoClaw/2.1 共 6 个，全部命中 tagged；
	// 普通流量 4 个（UA 不匹配 1 + curl 3）走全量池 {tagged, plain}，rr 各半（tagged 2 / plain 2）。
	// 若 GoClaw/2.1 泄漏到 plain，pc 会大于 2。
	if tc != 8 {
		t.Fatalf("tagged received=%d, want 8 (6 GoClaw + 2 normal rr share)", tc)
	}
	if pc != 2 {
		t.Fatalf("plain received=%d, want 2 (only normal rr share, no GoClaw)", pc)
	}
}

// TestGoClawNoToolCallBackend 验证命中规则但池中没有任何 tool_call 后端时，请求返回 400。
func TestGoClawNoToolCallBackend(t *testing.T) {
	backend, _ := newRecordingBackend()
	defer backend.Close()
	routing := &config.RoutingConfig{Rules: []config.RoutingRule{{
		Name:   "goclaw",
		Header: map[string]string{"User-Agent": "GoClaw/2.1"},
		Pool:   "tool_call",
	}}}
	svc, cleanup := newRoutingTestServer(t, []config.BackendConfig{{Name: "b", URL: backend.URL, Weight: 1}}, routing)
	defer cleanup()

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"llm_prox","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "GoClaw/2.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "tool_call") {
		t.Fatalf("error body=%s, want mention of tool_call", body)
	}
}

// newRoutingTestServer 构建带 routing 规则配置的测试 Server（无调度器，纯转发 + 路由规则）。
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
		strings.NewReader(`{"model":"llm_prox","messages":[]}`))
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
		strings.NewReader(`{"model":"llm_prox","messages":[]}`))
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
func deadBackendURL(t *testing.T) string {
	t.Helper()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "unreachable")
	}))
	u := dead.URL
	dead.Close()
	return u
}

// okJSONBackend 返回一个恒定回 200 JSON 的后端。
func okJSONBackend(t *testing.T, payload string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payload)
	}))
	return srv
}

// waitForRecords 轮询请求记录，直到数量与状态码满足条件（FinishRequest 在响应体
// 传完之后才执行，直接查询存在竞态）。
func waitForRecords(t *testing.T, svc *Server, n int, wantStatus int) []model.RequestRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		recs, err := svc.store.GetRequests(100, 0, model.RequestFilter{})
		if err != nil {
			t.Fatalf("getRequests: %v", err)
		}
		if len(recs) == n && recs[0].StatusCode == wantStatus {
			return recs
		}
		time.Sleep(20 * time.Millisecond)
	}
	recs, _ := svc.store.GetRequests(100, 0, model.RequestFilter{})
	t.Fatalf("timed out waiting for %d record(s) with status %d, got %+v", n, wantStatus, recs)
	return nil
}

func postCompletion(t *testing.T, proxyURL string, extraHeaders map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/v1/completions",
		strings.NewReader(`{"model":"llm_prox","prompt":"hi"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy do: %v", err)
	}
	return resp
}

// TestHandleProxyFailoverOnConnectionError 验证首个后端连接失败时自动转移到下一个
// 后端，且落库记录指向实际服务的后端。
func TestHandleProxyFailoverOnConnectionError(t *testing.T) {
	ok := okJSONBackend(t, `{"model":"ok-model","content":"ok"}`)
	defer ok.Close()

	svc, _, cleanup := newTestServer(t, ok.URL)
	defer cleanup()
	// rr 策略 + 字典序保证先选中 a-down。
	svc.balancer = balancer.New([]config.BackendConfig{
		{Name: "a-down", URL: deadBackendURL(t), Weight: 1},
		{Name: "b-ok", URL: ok.URL, Weight: 1},
	}, "rr")

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	resp := postCompletion(t, proxy.URL, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 after failover", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ok-model") {
		t.Fatalf("body=%s, want response from the healthy backend", body)
	}

	recs := waitForRecords(t, svc, 1, http.StatusOK)
	if recs[0].BackendURL != ok.URL {
		t.Fatalf("record backend=%q, want %q", recs[0].BackendURL, ok.URL)
	}
}

// TestHandleProxyFailoverOn5xx 验证首个后端返回 5xx 时自动转移到下一个后端。
func TestHandleProxyFailoverOn5xx(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer failing.Close()
	ok := okJSONBackend(t, `{"model":"ok-model","content":"ok"}`)
	defer ok.Close()

	svc, _, cleanup := newTestServer(t, ok.URL)
	defer cleanup()
	svc.balancer = balancer.New([]config.BackendConfig{
		{Name: "a-fail", URL: failing.URL, Weight: 1},
		{Name: "b-ok", URL: ok.URL, Weight: 1},
	}, "rr")

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	resp := postCompletion(t, proxy.URL, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 after failover", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ok-model") {
		t.Fatalf("body=%s, want response from the healthy backend", body)
	}

	recs := waitForRecords(t, svc, 1, http.StatusOK)
	if recs[0].BackendURL != ok.URL {
		t.Fatalf("record backend=%q, want %q", recs[0].BackendURL, ok.URL)
	}
}

// TestHandleProxyFailoverOn4xx 验证 4xx 响应（非 200）同样触发故障转移。
func TestHandleProxyFailoverOn4xx(t *testing.T) {
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such model", http.StatusNotFound)
	}))
	defer notFound.Close()
	ok := okJSONBackend(t, `{"model":"ok-model","content":"ok"}`)
	defer ok.Close()

	svc, _, cleanup := newTestServer(t, ok.URL)
	defer cleanup()
	svc.balancer = balancer.New([]config.BackendConfig{
		{Name: "a-404", URL: notFound.URL, Weight: 1},
		{Name: "b-ok", URL: ok.URL, Weight: 1},
	}, "rr")

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	resp := postCompletion(t, proxy.URL, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 after failover on 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ok-model") {
		t.Fatalf("body=%s, want response from the healthy backend", body)
	}

	recs := waitForRecords(t, svc, 1, http.StatusOK)
	if recs[0].BackendURL != ok.URL {
		t.Fatalf("record backend=%q, want %q", recs[0].BackendURL, ok.URL)
	}
}

// TestHandleProxyFailoverExhaustedMixedStatus 验证所有后端都返回非 200 时，
// 按后端分组返回每个后端的真实状态码与响应内容，顶层状态码沿用最后一个后端的。
func TestHandleProxyFailoverExhaustedMixedStatus(t *testing.T) {
	first404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "first says no", http.StatusNotFound)
	}))
	defer first404.Close()
	second500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "second says no", http.StatusInternalServerError)
	}))
	defer second500.Close()

	svc, _, cleanup := newTestServer(t, first404.URL)
	defer cleanup()
	svc.balancer = balancer.New([]config.BackendConfig{
		{Name: "a-404", URL: first404.URL, Weight: 1},
		{Name: "b-500", URL: second500.URL, Weight: 1},
	}, "rr")

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	resp := postCompletion(t, proxy.URL, nil)
	defer resp.Body.Close()
	// 顶层状态码 = 最后一个后端（b-500）的真实状态码。
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500 (last backend's status)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "\"attempts\"") || !strings.Contains(s, "all backends returned non-200") {
		t.Fatalf("body=%s, want grouped attempts report", s)
	}
	// 每个后端的真实状态码与响应内容都要在报告里。
	for _, want := range []string{
		"\"status_code\":404", "\"status_code\":500",
		first404.URL, second500.URL,
		"first says no", "second says no",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("body=%s, want to contain %q", s, want)
		}
	}

	recs := waitForRecords(t, svc, 1, http.StatusInternalServerError)
	if recs[0].BackendURL != second500.URL {
		t.Fatalf("record backend=%q, want %q (last attempted)", recs[0].BackendURL, second500.URL)
	}
}

// TestHandleProxyFailoverExhausted 验证所有后端都失败时客户端收到 502。
func TestHandleProxyFailoverExhausted(t *testing.T) {
	deadA, deadB := deadBackendURL(t), deadBackendURL(t)
	svc, _, cleanup := newTestServer(t, deadA)
	defer cleanup()
	svc.balancer = balancer.New([]config.BackendConfig{
		{Name: "a-down", URL: deadA, Weight: 1},
		{Name: "b-down", URL: deadB, Weight: 1},
	}, "rr")

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	resp := postCompletion(t, proxy.URL, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 when all backends fail", resp.StatusCode)
	}
	// 所有后端连接失败：按后端分组返回每个后端的失败原因。
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "\"attempts\"") || !strings.Contains(s, "all backends failed") {
		t.Fatalf("body=%s, want grouped attempts report", s)
	}
	if !strings.Contains(s, deadA) || !strings.Contains(s, deadB) {
		t.Fatalf("body=%s, want both backends reported", s)
	}

	recs := waitForRecords(t, svc, 1, http.StatusBadGateway)
	if recs[0].ErrorText == "" {
		t.Fatalf("expected 502 record with error text, got %+v", recs)
	}
}

// TestGoClawFailoverStaysInToolCallPool 验证命中 tool_call 规则后故障转移只在
// tool_call 子池内转移，不会落到普通后端。
func TestGoClawFailoverStaysInToolCallPool(t *testing.T) {
	tc2 := okJSONBackend(t, `{"model":"tc2-model","content":"ok"}`)
	defer tc2.Close()
	plain := okJSONBackend(t, `{"model":"plain-model","content":"ok"}`)
	defer plain.Close()

	routing := &config.RoutingConfig{Rules: []config.RoutingRule{{
		Name:   "goclaw",
		Header: map[string]string{"User-Agent": "GoClaw/2.1"},
		Pool:   "tool_call",
	}}}
	svc, cleanup := newRoutingTestServer(t, []config.BackendConfig{
		{Name: "plain", URL: plain.URL, Weight: 1},
		{Name: "tc-1", URL: deadBackendURL(t), Weight: 1, Tags: []string{"tool_call"}},
		{Name: "tc-2", URL: tc2.URL, Weight: 1, Tags: []string{"tool_call"}},
	}, routing)
	defer cleanup()

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"llm_prox","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "GoClaw/2.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 after failover within tool_call pool", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "tc2-model") || strings.Contains(string(body), "plain-model") {
		t.Fatalf("body=%s, want response from tc-2, not the plain backend", body)
	}
}
